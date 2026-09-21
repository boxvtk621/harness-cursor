package cursor

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
)

const maximumQueuedEvents = 1024

type attemptRuntime struct {
	mu               sync.Mutex
	reference        harnessadapter.AttemptRef
	context          context.Context
	cancel           context.CancelFunc
	events           []harnessadapter.Event
	pending          []harnessadapter.Event
	wake             chan struct{}
	activated        bool
	closed           bool
	claimed          bool
	err              error
	terminal         *harnessadapter.ReconcileResult
	tools            map[string]toolState
	policyHash       string
	approvalMode     string
	workspace        string
	waitingApprovals int
}

type toolState struct {
	callID     string
	toolName   string
	actionHash string
	approvalID string
	started    bool
	done       bool
}

func newAttemptRuntime(reference harnessadapter.AttemptRef) *attemptRuntime {
	executionContext, cancel := context.WithCancel(context.Background())
	return &attemptRuntime{reference: reference, context: executionContext, cancel: cancel, wake: make(chan struct{}, 1), tools: make(map[string]toolState)}
}

func (runtime *attemptRuntime) activate() {
	runtime.mu.Lock()
	if runtime.activated {
		runtime.mu.Unlock()
		return
	}
	runtime.activated = true
	runtime.enqueueLocked(harnessadapter.StartedEvent{EventBase: harnessadapter.EventBase{Attempt: runtime.reference}})
	for _, event := range runtime.pending {
		runtime.enqueueLocked(event)
	}
	runtime.pending = nil
	runtime.signalLocked()
	runtime.mu.Unlock()
}

func (runtime *attemptRuntime) push(event harnessadapter.Event) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return
	}
	if !runtime.activated {
		runtime.pending = append(runtime.pending, event)
		return
	}
	runtime.enqueueLocked(event)
	runtime.signalLocked()
}

func (runtime *attemptRuntime) enqueueLocked(event harnessadapter.Event) {
	if len(runtime.events) >= maximumQueuedEvents {
		runtime.events = []harnessadapter.Event{harnessadapter.UnknownEvent{
			EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, Reason: "adapter_protocol", EffectStatus: "unknown",
		}}
		runtime.err = errors.New("cursor event queue limit exceeded")
		runtime.closed = true
		runtime.cancel()
		return
	}
	runtime.events = append(runtime.events, event)
}

func (runtime *attemptRuntime) finish(result harnessadapter.ReconcileResult, events ...harnessadapter.Event) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed {
		return
	}
	for _, event := range events {
		if runtime.activated {
			runtime.enqueueLocked(event)
		} else {
			runtime.pending = append(runtime.pending, event)
		}
	}
	runtime.terminal = &result
	runtime.closed = true
	runtime.cancel()
	runtime.signalLocked()
}

func (runtime *attemptRuntime) failUnknown() {
	result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "unknown"}
	runtime.finish(result, harnessadapter.UnknownEvent{
		EventBase: harnessadapter.EventBase{Attempt: runtime.reference}, Reason: "provider_state", EffectStatus: "unknown",
	})
}

func (runtime *attemptRuntime) signalLocked() {
	select {
	case runtime.wake <- struct{}{}:
	default:
	}
}

func (runtime *attemptRuntime) claim() (*eventStream, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.claimed {
		return nil, errors.New("cursor event stream is already claimed")
	}
	runtime.claimed = true
	return &eventStream{runtime: runtime}, nil
}

func (runtime *attemptRuntime) reconcile() harnessadapter.ReconcileResult {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.terminal != nil {
		return *runtime.terminal
	}
	if runtime.waitingApprovals > 0 {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileWaitingInput, EffectStatus: "known"}
	}
	return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileRunning, EffectStatus: "known"}
}

func (runtime *attemptRuntime) setWaitingApproval(waiting bool) {
	runtime.mu.Lock()
	if waiting {
		runtime.waitingApprovals++
	} else if runtime.waitingApprovals > 0 {
		runtime.waitingApprovals--
	}
	runtime.mu.Unlock()
}

func (runtime *attemptRuntime) cancelExecution() {
	runtime.cancel()
}

type eventStream struct {
	runtime *attemptRuntime
	once    sync.Once
	closed  chan struct{}
}

func (stream *eventStream) closeChannel() chan struct{} {
	stream.once.Do(func() { stream.closed = make(chan struct{}) })
	return stream.closed
}

func (stream *eventStream) Next(ctx context.Context) (harnessadapter.Event, error) {
	closed := stream.closeChannel()
	for {
		stream.runtime.mu.Lock()
		if len(stream.runtime.events) > 0 {
			event := stream.runtime.events[0]
			stream.runtime.events = stream.runtime.events[1:]
			stream.runtime.mu.Unlock()
			return event, nil
		}
		ended, streamErr := stream.runtime.closed, stream.runtime.err
		stream.runtime.mu.Unlock()
		if ended {
			if streamErr != nil {
				return nil, streamErr
			}
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-closed:
			return nil, io.EOF
		case <-stream.runtime.wake:
		}
	}
}

func (stream *eventStream) Close() error {
	closed := stream.closeChannel()
	select {
	case <-closed:
	default:
		close(closed)
	}
	return nil
}
