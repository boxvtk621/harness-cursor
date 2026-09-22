// Package diagnosticlog writes bounded, best-effort operational diagnostics.
// It deliberately exposes no message, prompt, payload, path, or error fields.
package diagnosticlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Schema            = "harness.console.v1"
	maximumEntryBytes = 4 << 10
	queueCapacity     = 256
)

type Level string

const (
	LevelInfo  Level = "info"
	LevelDebug Level = "debug"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

type Component string

const (
	ComponentService  Component = "service"
	ComponentRuntime  Component = "runtime"
	ComponentProvider Component = "provider"
	ComponentLogger   Component = "logger"
	ComponentAPI      Component = "api"
)

type Event string

const (
	EventServiceStarting       Event = "service.starting"
	EventServiceStopped        Event = "service.stopped"
	EventServiceFailed         Event = "service.failed"
	EventRuntimeOpened         Event = "runtime.opened"
	EventRuntimeClosed         Event = "runtime.closed"
	EventRecoveryStarted       Event = "recovery.started"
	EventRecoveryComplete      Event = "recovery.completed"
	EventRecoveryFailed        Event = "recovery.failed"
	EventRuntimeRecovered      Event = "runtime.recovered"
	EventCommandFailed         Event = "command.failed"
	EventCommandAccepted       Event = "command.accepted"
	EventCommandRejected       Event = "command.rejected"
	EventCommandDuplicate      Event = "command.deduplicated"
	EventAttemptDispatch       Event = "attempt.dispatching"
	EventAttemptStarted        Event = "attempt.started"
	EventAttemptTerminal       Event = "attempt.terminal"
	EventAttemptUnknown        Event = "attempt.unknown"
	EventProviderReady         Event = "provider.process_ready"
	EventProviderExited        Event = "provider.process_exited"
	EventToolStarted           Event = "tool.started"
	EventToolCompleted         Event = "tool.completed"
	EventProviderAuthStarted   Event = "provider.auth_started"
	EventProviderAuthCompleted Event = "provider.auth_completed"
	EventProviderAuthCancelled Event = "provider.auth_cancelled"
	EventProviderAuthExpired   Event = "provider.auth_expired"
	EventProviderAuthFailed    Event = "provider.auth_failed"
	EventProviderAuthLogout    Event = "provider.auth_logout"
	EventReadinessChanged      Event = "readiness.changed"
	EventLoggerDropped         Event = "log.records_dropped"
	EventLoggerWriteError      Event = "log.write_failed"
)

var (
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	allowedEvents = map[Event]bool{
		EventServiceStarting: true, EventServiceStopped: true, EventServiceFailed: true, EventRuntimeOpened: true, EventRuntimeRecovered: true, EventRuntimeClosed: true,
		EventRecoveryStarted: true, EventRecoveryComplete: true, EventRecoveryFailed: true, EventCommandFailed: true, EventCommandAccepted: true, EventCommandRejected: true,
		EventCommandDuplicate: true, EventAttemptDispatch: true, EventAttemptStarted: true,
		EventAttemptTerminal: true, EventAttemptUnknown: true, EventProviderReady: true, EventProviderExited: true,
		EventToolStarted: true, EventToolCompleted: true, EventLoggerDropped: true,
		EventLoggerWriteError: true, EventProviderAuthStarted: true, EventProviderAuthCompleted: true,
		EventProviderAuthCancelled: true, EventProviderAuthExpired: true, EventProviderAuthFailed: true,
		EventProviderAuthLogout: true, EventReadinessChanged: true,
	}
	disabled = &Logger{disabled: true}
)

// Fields is intentionally restricted to identifiers and bounded enums/counters.
// Do not add free-form strings here.
type Fields struct {
	NodeID            string
	DialogID          string
	RequestID         string
	AttemptID         string
	CommandID         string
	OperationID       string
	CallID            string
	Kind              string
	Tool              string
	Operation         string
	Outcome           string
	EffectStatus      string
	Reason            string
	Generation        int64
	ProcessGeneration int64
	DurationMS        int64
	Count             uint64
	Truncated         bool
}

type record struct {
	Schema            string    `json:"schema"`
	Timestamp         string    `json:"ts"`
	BootID            string    `json:"bootId"`
	Level             Level     `json:"level"`
	Component         Component `json:"component"`
	Event             Event     `json:"event"`
	NodeID            string    `json:"nodeId"`
	DialogID          string    `json:"dialogId,omitempty"`
	RequestID         string    `json:"requestId,omitempty"`
	AttemptID         string    `json:"attemptId,omitempty"`
	CommandID         string    `json:"commandId,omitempty"`
	OperationID       string    `json:"operationId,omitempty"`
	CallID            string    `json:"toolCallId,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	Tool              string    `json:"tool,omitempty"`
	Operation         string    `json:"operation,omitempty"`
	Outcome           string    `json:"outcome,omitempty"`
	EffectStatus      string    `json:"effectStatus,omitempty"`
	Reason            string    `json:"reasonCode,omitempty"`
	Generation        int64     `json:"generation,omitempty"`
	ProcessGeneration int64     `json:"processGeneration,omitempty"`
	DurationMS        int64     `json:"durationMs,omitempty"`
	Count             uint64    `json:"count,omitempty"`
	Truncated         bool      `json:"truncated,omitempty"`
}

type Logger struct {
	output   io.Writer
	now      func() time.Time
	bootID   string
	queue    chan record
	done     chan struct{}
	disabled bool
	mu       sync.RWMutex
	closed   bool
	closeOne sync.Once
	dropped  atomic.Uint64
	failures atomic.Uint64
	nodeID   string
}

func Disabled() *Logger { return disabled }

func New(output io.Writer) *Logger {
	if output == nil {
		return Disabled()
	}
	logger := &Logger{output: output, now: time.Now, bootID: newBootID(), queue: make(chan record, queueCapacity), done: make(chan struct{})}
	go logger.writeLoop()
	return logger
}

// SetNodeID installs the node identity once configuration has been validated.
// Every subsequently accepted record, including logger summaries, carries it.
func (logger *Logger) SetNodeID(nodeID string) {
	if logger == nil || logger.disabled {
		return
	}
	nodeID = safeToken(nodeID, 128)
	if nodeID == "" {
		return
	}
	logger.mu.Lock()
	logger.nodeID = nodeID
	logger.mu.Unlock()
}

// Emit never waits for stderr. When the queue is full, the loss is summarized
// by logger.dropped on the next successful drain or during shutdown.
func (logger *Logger) Emit(level Level, component Component, event Event, fields Fields) {
	if logger == nil || logger.disabled || !validLevel(level) || !validComponent(component) || !allowedEvents[event] {
		return
	}
	logger.mu.RLock()
	defer logger.mu.RUnlock()
	nodeID := safeToken(fields.NodeID, 128)
	if nodeID == "" {
		nodeID = logger.nodeID
	}
	if nodeID == "" || logger.closed {
		return
	}
	entry := record{
		Schema: Schema, Timestamp: logger.now().UTC().Format(time.RFC3339Nano), BootID: logger.bootID, Level: level, Component: component, Event: event,
		NodeID: nodeID, DialogID: safeToken(fields.DialogID, 128), RequestID: safeToken(fields.RequestID, 128),
		AttemptID: safeToken(fields.AttemptID, 128), CommandID: safeToken(fields.CommandID, 128), OperationID: safeToken(fields.OperationID, 128), CallID: safeToken(fields.CallID, 128),
		Kind: safeToken(fields.Kind, 64), Tool: safeToken(fields.Tool, 64), Operation: safeToken(fields.Operation, 64), Outcome: safeToken(fields.Outcome, 64),
		EffectStatus: safeToken(fields.EffectStatus, 64), Reason: safeToken(fields.Reason, 64),
		Generation: nonnegative(fields.Generation), ProcessGeneration: nonnegative(fields.ProcessGeneration), DurationMS: nonnegative(fields.DurationMS), Count: fields.Count, Truncated: fields.Truncated,
	}
	select {
	case logger.queue <- entry:
	default:
		logger.dropped.Add(1)
	}
}

// Shutdown drains accepted entries. Callers should use a bounded context.
func (logger *Logger) Shutdown(ctx context.Context) error {
	if logger == nil || logger.disabled {
		return nil
	}
	logger.closeOne.Do(func() {
		logger.mu.Lock()
		logger.closed = true
		close(logger.queue)
		logger.mu.Unlock()
	})
	select {
	case <-logger.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (logger *Logger) writeLoop() {
	defer close(logger.done)
	for entry := range logger.queue {
		logger.writeSummaries(entry.Timestamp)
		logger.write(entry)
	}
	logger.writeSummaries(logger.now().UTC().Format(time.RFC3339Nano))
}

func (logger *Logger) writeSummaries(timestamp string) {
	logger.mu.RLock()
	nodeID := logger.nodeID
	logger.mu.RUnlock()
	if nodeID == "" {
		return
	}
	if count := logger.failures.Swap(0); count > 0 {
		if !logger.writeRecord(record{Schema: Schema, Timestamp: timestamp, BootID: logger.bootID, Level: LevelWarn, Component: ComponentLogger, Event: EventLoggerWriteError, NodeID: nodeID, Count: count}) {
			logger.failures.Add(count)
		}
	}
	if count := logger.dropped.Swap(0); count > 0 {
		if !logger.writeRecord(record{Schema: Schema, Timestamp: timestamp, BootID: logger.bootID, Level: LevelWarn, Component: ComponentLogger, Event: EventLoggerDropped, NodeID: nodeID, Count: count}) {
			logger.dropped.Add(count)
			logger.failures.Add(1)
		}
	}
}

func (logger *Logger) write(entry record) {
	if logger.writeRecord(entry) {
		return
	}
	logger.failures.Add(1)
}

func (logger *Logger) writeRecord(entry record) bool {
	encoded, err := json.Marshal(entry)
	if err != nil || len(encoded)+1 > maximumEntryBytes {
		logger.dropped.Add(1)
		return true
	}
	encoded = append(encoded, '\n')
	if written, err := logger.output.Write(encoded); err != nil || written != len(encoded) {
		return false
	}
	return true
}

func validLevel(level Level) bool {
	return level == LevelDebug || level == LevelInfo || level == LevelWarn || level == LevelError
}
func validComponent(component Component) bool {
	return component == ComponentService || component == ComponentRuntime || component == ComponentProvider || component == ComponentLogger || component == ComponentAPI
}

func newBootID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(raw[:])
}

func safeToken(value string, maximum int) string {
	if value == "" || len(value) > maximum || !tokenPattern.MatchString(value) {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "ghp_") || strings.HasPrefix(lower, "gho_") ||
		strings.HasPrefix(value, "eyJ") || strings.Contains(lower, "bearer:") {
		return ""
	}
	return value
}

func nonnegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
