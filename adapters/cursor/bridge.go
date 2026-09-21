package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type bridgeFrame struct {
	Type       string          `json:"type"`
	ID         string          `json:"id,omitempty"`
	Operation  string          `json:"operation,omitempty"`
	AttemptKey string          `json:"attemptKey,omitempty"`
	OK         bool            `json:"ok,omitempty"`
	Code       string          `json:"code,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Event      string          `json:"event,omitempty"`
	CallID     string          `json:"callId,omitempty"`
	Name       string          `json:"name,omitempty"`
	Status     string          `json:"status,omitempty"`
	Text       string          `json:"text,omitempty"`
	Usage      *bridgeUsage    `json:"usage,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type bridgeUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type bridgeResponse struct {
	ok     bool
	code   string
	result json.RawMessage
}

type bridge struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	maximum   int
	onEvent   func(bridgeFrame)
	onRequest func(bridgeFrame)
	onExit    func()
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan bridgeResponse
	inbound   map[string]struct{}
	done      chan struct{}
	wait      chan error
	closeOne  sync.Once
	nextID    atomic.Uint64
}

func startBridge(config Config, onEvent func(bridgeFrame), onRequest func(bridgeFrame), onExit func()) (*bridge, error) {
	command := exec.Command(config.NodeExecutable, config.WorkerEntrypoint)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = io.Discard
	instance := &bridge{
		cmd: command, stdin: stdin, maximum: config.MaxFrameBytes,
		onEvent: onEvent, onRequest: onRequest, onExit: onExit, pending: make(map[string]chan bridgeResponse), inbound: make(map[string]struct{}),
		done: make(chan struct{}), wait: make(chan error, 1),
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start cursor worker: %w", err)
	}
	go func() {
		instance.read(stdout)
		instance.wait <- command.Wait()
		close(instance.wait)
	}()
	return instance, nil
}

func (bridge *bridge) respond(id string, result any, code string) error {
	response := struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Result any    `json:"result,omitempty"`
		Code   string `json:"code,omitempty"`
	}{Type: "response", ID: id, OK: code == "", Result: result, Code: code}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > bridge.maximum {
		return errors.New("cursor worker response exceeds frame limit")
	}
	bridge.writeMu.Lock()
	_, err = bridge.stdin.Write(append(encoded, '\n'))
	bridge.writeMu.Unlock()
	if err != nil {
		return errors.New("cursor worker response delivery is unknown")
	}
	return nil
}

func (bridge *bridge) call(ctx context.Context, operation string, payload any, result any) error {
	return bridge.callAfterWrite(ctx, operation, payload, result, nil)
}

// callAfterWrite invokes afterWrite after the request bytes are written while
// still holding writeMu. A custom-tool response therefore cannot overtake a
// native cancel request that is responsible for releasing that tool.
func (bridge *bridge) callAfterWrite(ctx context.Context, operation string, payload any, result any, afterWrite func()) error {
	id := fmt.Sprintf("request-%d", bridge.nextID.Add(1))
	request := struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Operation string `json:"operation"`
		Payload   any    `json:"payload"`
	}{Type: "request", ID: id, Operation: operation, Payload: payload}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(encoded) > bridge.maximum {
		return errors.New("cursor worker request exceeds frame limit")
	}
	response := make(chan bridgeResponse, 1)
	bridge.mu.Lock()
	select {
	case <-bridge.done:
		bridge.mu.Unlock()
		return errors.New("cursor worker is unavailable")
	default:
	}
	bridge.pending[id] = response
	bridge.mu.Unlock()
	bridge.writeMu.Lock()
	_, err = bridge.stdin.Write(append(encoded, '\n'))
	if afterWrite != nil {
		afterWrite()
	}
	bridge.writeMu.Unlock()
	if err != nil {
		bridge.remove(id)
		return errors.New("cursor worker acknowledgement is unknown")
	}
	select {
	case reply := <-response:
		if !reply.ok {
			if reply.code == "rejected" {
				return errWorkerRejected
			}
			return errors.New("cursor worker operation is unknown")
		}
		if result != nil && len(reply.result) > 0 {
			if err := json.Unmarshal(reply.result, result); err != nil {
				return errors.New("cursor worker response is invalid")
			}
		}
		return nil
	case <-ctx.Done():
		bridge.remove(id)
		return ctx.Err()
	case <-bridge.done:
		bridge.remove(id)
		return errors.New("cursor worker acknowledgement is unknown")
	}
}

func (bridge *bridge) remove(id string) {
	bridge.mu.Lock()
	delete(bridge.pending, id)
	bridge.mu.Unlock()
}

func (bridge *bridge) read(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), bridge.maximum)
	for scanner.Scan() {
		var frame bridgeFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			break
		}
		switch frame.Type {
		case "response":
			bridge.mu.Lock()
			pending := bridge.pending[frame.ID]
			delete(bridge.pending, frame.ID)
			bridge.mu.Unlock()
			if pending != nil {
				pending <- bridgeResponse{ok: frame.OK, code: frame.Code, result: frame.Result}
			}
		case "event":
			bridge.onEvent(frame)
		case "request":
			if bridge.onRequest == nil || !strings.HasPrefix(frame.ID, "worker-") || frame.Operation != "execute_tool" || len(frame.Payload) == 0 || !bridge.registerInbound(frame.ID) {
				bridge.shutdown()
				return
			}
			go func() {
				defer bridge.releaseInbound(frame.ID)
				bridge.onRequest(frame)
			}()
		default:
			bridge.shutdown()
			return
		}
	}
	bridge.shutdown()
}

func (bridge *bridge) registerInbound(id string) bool {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if len(bridge.inbound) >= 128 {
		return false
	}
	if _, exists := bridge.inbound[id]; exists {
		return false
	}
	bridge.inbound[id] = struct{}{}
	return true
}

func (bridge *bridge) releaseInbound(id string) {
	bridge.mu.Lock()
	delete(bridge.inbound, id)
	bridge.mu.Unlock()
}

func (bridge *bridge) shutdown() {
	bridge.closeOne.Do(func() {
		close(bridge.done)
		_ = bridge.stdin.Close()
		bridge.mu.Lock()
		for id, pending := range bridge.pending {
			delete(bridge.pending, id)
			pending <- bridgeResponse{code: "worker_exit"}
		}
		bridge.mu.Unlock()
		bridge.onExit()
	})
}

func (bridge *bridge) Close() error {
	bridge.stop()
	return <-bridge.wait
}

// stop is safe from callbacks running on the bridge read goroutine: it never
// waits for that goroutine to reach Cmd.Wait.
func (bridge *bridge) stop() {
	bridge.shutdown()
	if bridge.cmd.Process != nil {
		_ = bridge.cmd.Process.Kill()
	}
}
