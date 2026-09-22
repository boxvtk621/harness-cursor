// Package cursor implements the private Cursor SDK adapter for the Harness node.
package cursor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tools"
)

const (
	defaultModel            = "composer-2.5"
	defaultOperationTimeout = 10 * time.Second
	defaultMaximumFrame     = 1024 * 1024

	// The manifest contains provider-neutral logical names. The Cursor SDK
	// receives the corresponding bare custom-tool keys declared in tools.go.
	// Byte identity is intentional: the manifest hash is part of the effective
	// policy, so packaging and the adapter must agree on one representation.
	cursorExplicitToolManifest = "[{\"name\":\"cursor.command\"},{\"name\":\"cursor.file_change\"}]\n"
)

var (
	errWorkerRejected = errors.New("cursor worker rejected operation")
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Config contains only private adapter process settings. APIKey is supplied to
// the pinned SDK through its supported CURSOR_API_KEY environment variable. It
// never appears in argv, protocol frames, events, or durable native-ID mapping.
type Config struct {
	NodeExecutable   string
	WorkerEntrypoint string
	StateDir         string
	WorkingDir       string
	APIKey           string
	Model            string
	OperationTimeout time.Duration
	MaxFrameBytes    int
	ToolRunner       toolrunner.Runner
}

// Adapter owns the Cursor agent/run references. Only AttemptRef values cross
// the provider-neutral Harness boundary.
type Adapter struct {
	config    Config
	artifacts node.ArtifactSink
	store     *mappingStore
	bridge    *bridge
	runner    toolrunner.Runner

	dispatchMu sync.Mutex
	mu         sync.Mutex
	attempts   map[string]*attemptRuntime
	approvals  map[string]*pendingApproval
	closed     bool
}

// New starts one bounded stdio worker and verifies its SDK version before the
// adapter can be passed to node.Open.
func New(config Config, artifacts node.ArtifactSink) (*Adapter, error) {
	if config.NodeExecutable == "" || config.WorkerEntrypoint == "" || len(config.APIKey) == 0 || len(config.APIKey) > 4096 || strings.ContainsAny(config.APIKey, "\r\n") ||
		!filepath.IsAbs(config.StateDir) || config.WorkingDir != "" && (!filepath.IsAbs(config.WorkingDir) || pathsOverlap(config.StateDir, config.WorkingDir) || reservedWorkspacePath(config.WorkingDir)) {
		return nil, errors.New("cursor adapter config is incomplete")
	}
	if config.Model == "" {
		config.Model = defaultModel
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = defaultOperationTimeout
	}
	if config.OperationTimeout < 0 {
		return nil, errors.New("cursor operation timeout is invalid")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = defaultMaximumFrame
	}
	if config.MaxFrameBytes < 4096 || config.MaxFrameBytes > harnessprotocol.MaximumWireBytes {
		return nil, errors.New("cursor worker frame limit is invalid")
	}
	store, err := openMappingStore(config.StateDir)
	if err != nil {
		return nil, err
	}
	if config.WorkingDir != "" {
		if err := prepareWorkspaceRoot(config.WorkingDir); err != nil {
			return nil, err
		}
		if !canonicalPathsDisjoint(config.StateDir, config.WorkingDir) || !canonicalWorkspacePathAllowed(config.WorkingDir) {
			return nil, errors.New("cursor state and workspace roots overlap")
		}
	}
	adapter := &Adapter{config: config, artifacts: artifacts, store: store, runner: config.ToolRunner, attempts: make(map[string]*attemptRuntime), approvals: make(map[string]*pendingApproval)}
	worker, err := startBridge(config, adapter.handleWorkerEvent, adapter.handleWorkerRequest, adapter.handleWorkerExit)
	if err != nil {
		return nil, err
	}
	adapter.bridge = worker
	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	err = worker.call(ctx, "init", map[string]any{
		"model": config.Model, "stateDir": config.StateDir,
		"maxFrameBytes": config.MaxFrameBytes,
	}, &initialized)
	if err != nil || initialized.Version != harnessadapter.CursorSDKVersion {
		_ = worker.Close()
		if err != nil {
			return nil, fmt.Errorf("initialize cursor worker: %w", err)
		}
		return nil, errors.New("cursor worker SDK version mismatch")
	}
	return adapter, nil
}

func (adapter *Adapter) Identity(context.Context) (harnessadapter.Identity, error) {
	declared := make(map[harnessadapter.Capability]bool)
	verified := make(map[harnessadapter.Capability]bool)
	for _, capability := range []harnessadapter.Capability{
		harnessadapter.CapabilityChat, harnessadapter.CapabilityEvents,
		harnessadapter.CapabilityToolResults, harnessadapter.CapabilityCancel,
		harnessadapter.CapabilitySteerAttached, harnessadapter.CapabilitySessionResume,
		harnessadapter.CapabilityPolicyEnforcement,
	} {
		declared[capability] = true
		verified[capability] = true
	}
	return harnessadapter.Identity{
		Kind: harnessadapter.KindCursor, Version: harnessadapter.CursorSDKVersion,
		ProtocolVersion: harnessprotocol.ProtocolVersion, SchemaID: harnessprotocol.SchemaID,
		SchemaSHA256: harnessprotocol.SchemaSHA256, Declared: declared, Verified: verified,
	}, nil
}

func (adapter *Adapter) Start(ctx context.Context, input harnessadapter.StartInput) (harnessadapter.StartResult, error) {
	policy, failure := adapter.validateDispatch(input.Attempt, input.Prompt, input.Context, input.Policy)
	if failure != nil {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartRejected, Failure: failure}, nil
	}
	if _, exists := adapter.store.dialog(input.Attempt.DialogID); exists {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartRejected, Failure: taskFailure("cursor_dialog_exists", "cursor dialog already has native context")}, nil
	}
	result, err := adapter.dispatch(ctx, input.Attempt, input.Prompt, input.Context, policy, "")
	if err != nil {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartUnknown, Failure: nodeFailure("cursor_dispatch_unknown", "cursor dispatch acknowledgement is unknown", true)}, err
	}
	if result == nil {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartStarted}, nil
	}
	return harnessadapter.StartResult{Outcome: harnessadapter.StartUnknown, Failure: result}, nil
}

func (adapter *Adapter) Resume(ctx context.Context, input harnessadapter.ResumeInput) (harnessadapter.ResumeResult, error) {
	policy, failure := adapter.validateDispatch(input.Attempt, input.Prompt, input.Context, input.Policy)
	if failure != nil {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeRejected, Failure: failure}, nil
	}
	dialog, exists := adapter.store.dialog(input.Attempt.DialogID)
	exactRetryBoundary := input.Context == dialog.Boundary
	if !exists || dialog.AgentID == "" || input.Context.Sequence < dialog.Boundary.Sequence ||
		(input.Context.Sequence == dialog.Boundary.Sequence && !exactRetryBoundary) {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeContextMissing, Failure: taskFailure("cursor_context_missing", "cursor dialog context is unavailable")}, nil
	}
	result, err := adapter.dispatch(ctx, input.Attempt, input.Prompt, input.Context, policy, dialog.AgentID)
	if err != nil {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeUnknown, Failure: nodeFailure("cursor_dispatch_unknown", "cursor dispatch acknowledgement is unknown", true)}, err
	}
	if result == nil {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeStarted}, nil
	}
	return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeUnknown, Failure: result}, nil
}

func (adapter *Adapter) dispatch(ctx context.Context, reference harnessadapter.AttemptRef, prompt string, boundary harnessadapter.ContextBoundary, policy harnessadapter.PolicySnapshot, resumeAgentID string) (*harnessadapter.Failure, error) {
	adapter.dispatchMu.Lock()
	defer adapter.dispatchMu.Unlock()
	if previous, exists := adapter.store.attempt(reference); exists {
		if previous.Context != boundary || previous.PolicyHash != policy.EffectiveHash {
			return protocolFailure("cursor_dispatch_conflict", "cursor dispatch replay does not match its durable context and policy"), nil
		}
		if previous.State == "active" {
			adapter.mu.Lock()
			active := adapter.attempts[attemptKey(reference)] != nil
			adapter.mu.Unlock()
			if active {
				return nil, nil
			}
		}
		return nodeFailure("cursor_dispatch_unknown", "cursor dispatch was previously attempted", true), nil
	}
	workspace := adapter.config.StateDir
	if policy.ApprovalMode == harnessadapter.ApprovalModeExplicitOnce {
		var err error
		workspace, err = adapter.prepareWorkspace(reference.DialogID)
		if err != nil {
			return nodeFailure("cursor_workspace_unavailable", "cursor dialog workspace is unavailable", true), err
		}
	}
	if err := adapter.store.putIntent(reference, boundary, policy.EffectiveHash); err != nil {
		return nil, err
	}
	runtime := newAttemptRuntime(reference)
	runtime.policyHash = policy.EffectiveHash
	runtime.approvalMode = policy.ApprovalMode
	runtime.workspace = workspace
	key := attemptKey(reference)
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return nil, errors.New("cursor adapter is closed")
	}
	adapter.attempts[key] = runtime
	adapter.mu.Unlock()
	var response struct {
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
	}
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	err := adapter.bridge.call(operationCtx, "dispatch", map[string]any{
		"attemptKey": key, "prompt": prompt, "resumeAgentId": resumeAgentID,
		"policyContent": string(policy.Content), "approvalMode": policy.ApprovalMode,
		"workspace": workspace,
	}, &response)
	if err != nil || !boundedNativeID(response.AgentID) || !boundedNativeID(response.RunID) {
		runtime.failUnknown()
		if err == nil {
			err = errors.New("cursor dispatch acknowledgement is invalid")
		}
		return nil, err
	}
	if err := adapter.store.activate(reference, boundary, policy.EffectiveHash, response.AgentID, response.RunID); err != nil {
		runtime.failUnknown()
		return nil, err
	}
	runtime.activate()
	return nil, nil
}

func (adapter *Adapter) Events(_ context.Context, input harnessadapter.EventsInput) (harnessadapter.EventStream, error) {
	adapter.mu.Lock()
	runtime := adapter.attempts[attemptKey(input.Attempt)]
	adapter.mu.Unlock()
	if runtime == nil {
		return nil, errors.New("cursor event stream is unavailable for attempt")
	}
	return runtime.claim()
}

func (adapter *Adapter) Steer(ctx context.Context, input harnessadapter.SteerInput) (harnessadapter.SteerResult, error) {
	if !validReference(input.Attempt) || !uuidPattern.MatchString(input.MessageID) || !boundedText(input.Text) {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerRejected, Failure: protocolFailure("cursor_steer_invalid", "cursor steer input is invalid")}, nil
	}
	mapping, runtime := adapter.active(input.Attempt)
	if runtime == nil {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerUnknown, Failure: nodeFailure("cursor_run_unavailable", "cursor run is unavailable", true)}, nil
	}
	var response struct {
		Status string `json:"status"`
	}
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	err := adapter.bridge.call(operationCtx, "steer", map[string]any{
		"attemptKey": attemptKey(input.Attempt), "runId": mapping.RunID, "text": input.Text,
	}, &response)
	if err != nil {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerUnknown, Failure: nodeFailure("cursor_steer_unknown", "cursor steer acknowledgement is unknown", true)}, err
	}
	if response.Status == "complete_delivered" {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerApplied}, nil
	}
	if response.Status == "queued" {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerFallbackQueued}, nil
	}
	return harnessadapter.SteerResult{Outcome: harnessadapter.SteerUnknown, Failure: nodeFailure("cursor_steer_unknown", "cursor steer outcome is unknown", true)}, nil
}

func (adapter *Adapter) Cancel(ctx context.Context, input harnessadapter.CancelInput) (harnessadapter.CancelResult, error) {
	if !validReference(input.Attempt) {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelRejected, Failure: protocolFailure("cursor_cancel_invalid", "cursor cancel input is invalid")}, nil
	}
	mapping, runtime := adapter.cancellable(input.Attempt)
	if runtime == nil {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelUnknown, Failure: nodeFailure("cursor_run_unavailable", "cursor run is unavailable", true)}, nil
	}
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	err := adapter.bridge.callAfterWrite(operationCtx, "cancel", map[string]any{
		"attemptKey": attemptKey(input.Attempt), "runId": mapping.RunID,
	}, nil, runtime.cancelExecution)
	// callAfterWrite can fail before reaching its write hook (for example after
	// worker exit). Cancellation is idempotent and must still release the runner.
	runtime.cancelExecution()
	if err != nil {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelUnknown, Failure: nodeFailure("cursor_cancel_unknown", "cursor cancel acknowledgement is unknown", true)}, err
	}
	return harnessadapter.CancelResult{Outcome: harnessadapter.CancelAcknowledged}, nil
}

func (adapter *Adapter) RespondInput(context.Context, harnessadapter.RespondInputInput) (harnessadapter.ResponseResult, error) {
	return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: policyFailure("cursor_input_unsupported", "cursor interactive input is disabled by policy")}, nil
}

func (adapter *Adapter) Reconcile(_ context.Context, input harnessadapter.ReconcileInput) (harnessadapter.ReconcileResult, error) {
	if !validReference(input.Attempt) {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "unknown", Failure: protocolFailure("cursor_reconcile_invalid", "cursor reconcile input is invalid")}, nil
	}
	adapter.mu.Lock()
	runtime := adapter.attempts[attemptKey(input.Attempt)]
	adapter.mu.Unlock()
	if runtime != nil {
		return runtime.reconcile(), nil
	}
	if _, exists := adapter.store.attempt(input.Attempt); exists {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "unknown", Failure: nodeFailure("cursor_provider_state_unknown", "cursor provider state is unknown after restart", true)}, nil
	}
	return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "none", Failure: taskFailure("cursor_attempt_missing", "cursor attempt mapping is unavailable")}, nil
}

// Close stops the private worker. It does not synthesize a provider terminal;
// active attempts become unknown because their native effect cannot be inferred.
func (adapter *Adapter) Close() error {
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return nil
	}
	adapter.closed = true
	for _, runtime := range adapter.attempts {
		runtime.cancelExecution()
	}
	adapter.mu.Unlock()
	return adapter.bridge.Close()
}

func (adapter *Adapter) active(reference harnessadapter.AttemptRef) (persistedAttempt, *attemptRuntime) {
	return adapter.activeWithStates(reference, harnessadapter.ReconcileRunning)
}

func (adapter *Adapter) cancellable(reference harnessadapter.AttemptRef) (persistedAttempt, *attemptRuntime) {
	return adapter.activeWithStates(reference, harnessadapter.ReconcileRunning, harnessadapter.ReconcileWaitingInput)
}

func (adapter *Adapter) activeWithStates(reference harnessadapter.AttemptRef, states ...harnessadapter.ReconcileOutcome) (persistedAttempt, *attemptRuntime) {
	mapping, exists := adapter.store.attempt(reference)
	if !exists || mapping.State != "active" || mapping.RunID == "" {
		return persistedAttempt{}, nil
	}
	adapter.mu.Lock()
	runtime := adapter.attempts[attemptKey(reference)]
	adapter.mu.Unlock()
	if runtime == nil {
		return persistedAttempt{}, nil
	}
	outcome := runtime.reconcile().Outcome
	for _, state := range states {
		if outcome == state {
			return mapping, runtime
		}
	}
	return persistedAttempt{}, nil
}

func (adapter *Adapter) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, adapter.config.OperationTimeout)
}

func (adapter *Adapter) validateDispatch(reference harnessadapter.AttemptRef, prompt string, boundary harnessadapter.ContextBoundary, policy harnessadapter.PolicySnapshot) (harnessadapter.PolicySnapshot, *harnessadapter.Failure) {
	if !validReference(reference) || !boundedText(prompt) || !uuidPattern.MatchString(boundary.MessageID) || boundary.Sequence < 1 || boundary.Sequence > harnessprotocol.MaximumSafeInteger {
		return harnessadapter.PolicySnapshot{}, protocolFailure("cursor_dispatch_invalid", "cursor dispatch input is invalid")
	}
	prepared, err := harnessadapter.PreparePolicySnapshot(policy)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, protocolFailure("cursor_policy_invalid", "cursor policy snapshot is invalid")
	}
	if !validCursorToolManifest(prepared) {
		return harnessadapter.PolicySnapshot{}, policyFailure("cursor_policy_unsupported", "cursor node policy and tool manifest are unsupported")
	}
	if prepared.ApprovalMode == harnessadapter.ApprovalModeExplicitOnce && (adapter.runner == nil || adapter.config.WorkingDir == "") {
		return harnessadapter.PolicySnapshot{}, policyFailure("cursor_tool_runner_unavailable", "cursor tools require the isolated tool runner")
	}
	if !boundedText(string(prepared.Content)) || strings.TrimSpace(string(prepared.Content)) == "" {
		return harnessadapter.PolicySnapshot{}, policyFailure("cursor_policy_unsupported", "cursor policy content cannot be applied by the native SDK")
	}
	return prepared, nil
}

func validCursorToolManifest(policy harnessadapter.PolicySnapshot) bool {
	if policy.ApprovalMode == harnessadapter.ApprovalModeDeny {
		// Preserve the legacy deny manifest while keeping it semantically exact.
		return bytes.Equal(bytes.TrimSpace(policy.ToolManifest), []byte("[]"))
	}
	return policy.ApprovalMode == harnessadapter.ApprovalModeExplicitOnce &&
		bytes.Equal(policy.ToolManifest, []byte(cursorExplicitToolManifest))
}

func validReference(reference harnessadapter.AttemptRef) bool {
	return uuidPattern.MatchString(reference.NodeID) && uuidPattern.MatchString(reference.DialogID) && uuidPattern.MatchString(reference.RequestID) && uuidPattern.MatchString(reference.AttemptID) && reference.Generation > 0 && reference.Generation <= harnessprotocol.MaximumSafeInteger
}

func boundedText(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= harnessprotocol.MaximumMessageBytes
}

func boundedNativeID(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n")
}

func taskFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureTask, Code: code, SafeMessage: message}
}

func nodeFailure(code, message string, retryable bool) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureNode, Code: code, SafeMessage: message, Retryable: retryable}
}

func policyFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailurePolicy, Code: code, SafeMessage: message}
}

func protocolFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureProtocol, Code: code, SafeMessage: message}
}

func (adapter *Adapter) handleWorkerExit() {
	adapter.mu.Lock()
	runtimes := make([]*attemptRuntime, 0, len(adapter.attempts))
	for _, runtime := range adapter.attempts {
		runtimes = append(runtimes, runtime)
	}
	adapter.mu.Unlock()
	for _, runtime := range runtimes {
		outcome := runtime.reconcile().Outcome
		if outcome == harnessadapter.ReconcileRunning || outcome == harnessadapter.ReconcileWaitingInput {
			runtime.failUnknown()
		}
	}
}

func (adapter *Adapter) handleWorkerEvent(frame bridgeFrame) {
	adapter.mu.Lock()
	runtime := adapter.attempts[frame.AttemptKey]
	adapter.mu.Unlock()
	if runtime == nil {
		return
	}
	switch frame.Event {
	case "tool":
		for _, event := range mapToolEvent(runtime, frame) {
			runtime.push(event)
		}
	case "terminal":
		adapter.finishRuntime(frame.AttemptKey, runtime, frame)
	}
}

func (adapter *Adapter) finishRuntime(key string, runtime *attemptRuntime, frame bridgeFrame) {
	reference := runtime.reference
	usage := safeUsage(frame.Usage)
	base := harnessadapter.EventBase{Attempt: reference}
	switch frame.Status {
	case "finished":
		content := safeInline(frame.Text)
		fullText := frame.Text
		messageID := derivedUUID("assistant", key)
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileCompleted, Output: &content, Usage: usage, EffectStatus: "known"}
		runtime.finish(result,
			harnessadapter.AssistantMessageEvent{EventBase: base, MessageID: messageID, Content: content, FinishReason: finishReason(content), FullText: &fullText},
			harnessadapter.TerminalEvent{EventBase: base, Outcome: harnessadapter.ReconcileCompleted, Output: &content, Usage: usage, EffectStatus: "known"})
	case "cancelled":
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileInterrupted, Usage: usage, EffectStatus: "known"}
		runtime.finish(result, harnessadapter.TerminalEvent{EventBase: base, Outcome: harnessadapter.ReconcileInterrupted, Usage: usage, EffectStatus: "known"})
	case "error":
		failure := taskFailure("cursor_run_failed", "cursor run failed")
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileFailed, Usage: usage, Failure: failure, EffectStatus: "unknown"}
		runtime.finish(result, harnessadapter.TerminalEvent{EventBase: base, Outcome: harnessadapter.ReconcileFailed, Usage: usage, Failure: failure, EffectStatus: "unknown"})
	default:
		failure := nodeFailure("cursor_provider_state_unknown", "cursor provider state is unknown", true)
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, Usage: usage, Failure: failure, EffectStatus: "unknown"}
		runtime.finish(result, harnessadapter.UnknownEvent{EventBase: base, Reason: "provider_state", EffectStatus: "unknown"})
	}
	_ = adapter.store.terminal(reference)
}

func mapToolEvent(runtime *attemptRuntime, frame bridgeFrame) []harnessadapter.Event {
	if !boundedNativeID(frame.CallID) || frame.Name == "" || !utf8.ValidString(frame.Name) || len(frame.Name) > 200 {
		return []harnessadapter.Event{harnessadapter.UnknownEvent{EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, Reason: "unmapped_event", EffectStatus: "unknown"}}
	}
	runtime.mu.Lock()
	state := runtime.tools[frame.CallID]
	if state.callID == "" {
		state.callID = derivedUUID("tool", attemptKey(runtime.reference)+"\x00"+frame.CallID)
	}
	base := harnessadapter.EventBase{Attempt: runtime.reference}
	content := harnessprotocol.SafeContent{Kind: "unavailable", Reason: "not_observed", Redaction: "unknown"}
	events := make([]harnessadapter.Event, 0, 2)
	if !state.started {
		state.started = true
		events = append(events, harnessadapter.ToolStartedEvent{
			EventBase: base, CallID: state.callID, ToolName: frame.Name,
			ActionHash: digestString("cursor-tool-v1\x00" + frame.Name + "\x00" + frame.CallID), Input: content,
		})
	}
	if !state.done && (frame.Status == "completed" || frame.Status == "error") {
		state.done = true
		status := "succeeded"
		if frame.Status == "error" {
			status = "failed"
		}
		events = append(events, harnessadapter.ToolCompletedEvent{
			EventBase: base, CallID: state.callID, Status: status, Result: content, EffectStatus: "unknown",
		})
	}
	runtime.tools[frame.CallID] = state
	runtime.mu.Unlock()
	return events
}

func safeInline(value string) harnessprotocol.SafeContent {
	if !utf8.ValidString(value) {
		return harnessprotocol.SafeContent{Kind: "unavailable", Reason: "provider_redacted", Redaction: "unknown"}
	}
	truncated := false
	if len(value) > harnessprotocol.MaximumMessageBytes {
		value = value[:harnessprotocol.MaximumMessageBytes]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		truncated = true
	}
	return harnessprotocol.SafeContent{Kind: "inline", Content: value, Redaction: "none", Truncated: truncated}
}

func finishReason(content harnessprotocol.SafeContent) string {
	if content.Truncated {
		return "length"
	}
	return "complete"
}

func safeUsage(value *bridgeUsage) *harnessprotocol.Usage {
	if value == nil || value.InputTokens < 0 || value.OutputTokens < 0 || value.TotalTokens < 0 || value.InputTokens > harnessprotocol.MaximumSafeInteger || value.OutputTokens > harnessprotocol.MaximumSafeInteger || value.TotalTokens > harnessprotocol.MaximumSafeInteger {
		return nil
	}
	return &harnessprotocol.Usage{Source: "per_attempt", InputTokens: value.InputTokens, OutputTokens: value.OutputTokens, TotalTokens: value.TotalTokens}
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func derivedUUID(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	raw := sum[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:])
}
