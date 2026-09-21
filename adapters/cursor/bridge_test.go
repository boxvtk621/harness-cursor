package cursor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBridgeHandlesWorkerRequestWithoutBlockingReader(t *testing.T) {
	config := fakeConfig(t)
	entrypoint := filepath.Join(t.TempDir(), "request-worker.mjs")
	if err := os.WriteFile(entrypoint, []byte(requestWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	config.WorkerEntrypoint = entrypoint
	received := make(chan bridgeFrame, 1)
	completed := make(chan struct{}, 1)
	var worker *bridge
	worker, err := startBridge(config, func(frame bridgeFrame) {
		if frame.Event == "terminal" {
			completed <- struct{}{}
		}
	}, func(frame bridgeFrame) {
		received <- frame
		if err := worker.respond(frame.ID, map[string]any{"success": true, "output": "ok"}, ""); err != nil {
			t.Errorf("respond: %v", err)
		}
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()

	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	if err := worker.call(ctx, "init", map[string]any{}, &initialized); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		if frame.ID != "worker-1" || frame.Operation != "execute_tool" {
			t.Fatalf("request = %#v", frame)
		}
	case <-ctx.Done():
		t.Fatal("worker request was not delivered")
	}
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal("worker did not receive the response")
	}
}

func TestBridgeCloseWaitsForEventHandler(t *testing.T) {
	config := fakeConfig(t)
	started := make(chan struct{})
	release := make(chan struct{})
	worker, err := startBridge(config, func(frame bridgeFrame) {
		if frame.Event != "terminal" {
			return
		}
		close(started)
		<-release
	}, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	if err := worker.call(ctx, "init", map[string]any{}, &initialized); err != nil {
		close(release)
		_ = worker.Close()
		t.Fatal(err)
	}
	var dispatched struct {
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
	}
	if err := worker.call(ctx, "dispatch", map[string]any{
		"attemptKey": "close-waits", "prompt": "complete", "policyContent": "deny all tools",
	}, &dispatched); err != nil {
		close(release)
		_ = worker.Close()
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		_ = worker.Close()
		t.Fatal("terminal event handler did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- worker.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("close returned before event handler completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-closed // The worker is deliberately killed, so only completion matters here.
}

func TestBridgeEventHandlerCanStopWithoutDeadlock(t *testing.T) {
	config := fakeConfig(t)
	stopped := make(chan struct{})
	var worker *bridge
	worker, err := startBridge(config, func(frame bridgeFrame) {
		if frame.Event == "terminal" {
			worker.stop()
			close(stopped)
		}
	}, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	var initialized struct {
		Version string `json:"version"`
	}
	if err := worker.call(ctx, "init", map[string]any{}, &initialized); err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}
	var dispatched struct {
		AgentID string `json:"agentId"`
		RunID   string `json:"runId"`
	}
	if err := worker.call(ctx, "dispatch", map[string]any{
		"attemptKey": "handler-stops", "prompt": "complete", "policyContent": "deny all tools",
	}, &dispatched); err != nil {
		_ = worker.Close()
		t.Fatal(err)
	}

	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("event handler stop deadlocked")
	}
	_ = worker.Close() // Reap the deliberately stopped worker.
}

const requestWorker = `
import readline from 'node:readline';
const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
const send = (value) => process.stdout.write(JSON.stringify(value) + '\n');
for await (const line of lines) {
  const frame = JSON.parse(line);
  if (frame.type === 'request' && frame.operation === 'init') {
    send({ type: 'response', id: frame.id, ok: true, result: { version: '1.0.31' } });
    send({ type: 'request', id: 'worker-1', operation: 'execute_tool', payload: { attemptKey: 'attempt', callId: 'native-call', name: 'cursor_command', args: { command: 'pwd', cwd: '.', workspaceAccess: 'read' } } });
  } else if (frame.type === 'response' && frame.id === 'worker-1' && frame.ok && frame.result?.output === 'ok') {
    send({ type: 'event', attemptKey: 'attempt', event: 'terminal', status: 'finished' });
  }
}
`
