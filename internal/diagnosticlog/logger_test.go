package diagnosticlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoggerWritesBoundedAllowlistedJSONLWithoutSensitiveText(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	logger.Emit(LevelInfo, ComponentRuntime, EventAttemptStarted, Fields{
		AttemptID: "10000000-0000-4000-8000-000000000001", Outcome: "running", Reason: "prompt with spaces",
		Generation: 1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(output.String())
	if len(line) >= maximumEntryBytes || strings.Contains(line, "prompt with spaces") || strings.Contains(line, "sk-never") {
		t.Fatalf("unsafe or oversized log line: %q", line)
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(line), &decoded) != nil || decoded["schema"] != Schema || decoded["event"] != string(EventAttemptStarted) || decoded["attemptId"] == nil || decoded["bootId"] == nil {
		t.Fatalf("unexpected log record: %s", line)
	}
}

func TestLoggerRejectsUnknownEvents(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	logger.Emit(LevelInfo, ComponentRuntime, Event("payload.dump"), Fields{Reason: "safe"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("unknown event was logged: %q", output.String())
	}
}

func TestSharedGoldenVector(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	logger.now = func() time.Time { return time.Unix(1, 0) }
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	logger.Emit(LevelWarn, ComponentRuntime, EventAttemptUnknown, Fields{
		DialogID: "20000000-0000-4000-8000-000000000001", RequestID: "20000000-0000-4000-8000-000000000002",
		AttemptID: "20000000-0000-4000-8000-000000000003", CommandID: "20000000-0000-4000-8000-000000000004",
		OperationID: "20000000-0000-4000-8000-000000000005", CallID: "20000000-0000-4000-8000-000000000006",
		Kind: "reconcile", Tool: "cursor.command", Operation: "observe", Outcome: "unknown", EffectStatus: "unknown", Reason: "provider_state",
		Generation: 2, ProcessGeneration: 3, DurationMS: 7, Count: 4, Truncated: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &decoded); err != nil {
		t.Fatal(err)
	}
	delete(decoded, "bootId")
	encoded, _ := json.Marshal(decoded)
	want := `{"attemptId":"20000000-0000-4000-8000-000000000003","commandId":"20000000-0000-4000-8000-000000000004","component":"runtime","count":4,"dialogId":"20000000-0000-4000-8000-000000000001","durationMs":7,"effectStatus":"unknown","event":"attempt.unknown","generation":2,"kind":"reconcile","level":"warn","nodeId":"10000000-0000-4000-8000-000000000000","operation":"observe","operationId":"20000000-0000-4000-8000-000000000005","outcome":"unknown","processGeneration":3,"reasonCode":"provider_state","requestId":"20000000-0000-4000-8000-000000000002","schema":"harness.console.v1","tool":"cursor.command","toolCallId":"20000000-0000-4000-8000-000000000006","truncated":true,"ts":"1970-01-01T00:00:01Z"}`
	if string(encoded) != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", encoded, want)
	}
}

type blockedWriter struct{ release chan struct{} }

func (writer blockedWriter) Write(value []byte) (int, error) {
	<-writer.release
	return len(value), nil
}

func TestLoggerEmitDoesNotBlockWhenQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	logger := New(blockedWriter{release: release})
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	done := make(chan struct{})
	go func() {
		for index := 0; index < queueCapacity*4; index++ {
			logger.Emit(LevelInfo, ComponentRuntime, EventRuntimeOpened, Fields{Count: uint64(index + 1)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked behind the writer")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

type recoveringWriter struct {
	bytes.Buffer
	failures int
}

func (writer *recoveringWriter) Write(value []byte) (int, error) {
	if writer.failures > 0 {
		writer.failures--
		return 0, errors.New("write failed")
	}
	return writer.Buffer.Write(value)
}

func TestLoggerSummaryRetriesPreserveOriginalCounts(t *testing.T) {
	writer := &recoveringWriter{failures: 3}
	logger := New(writer)
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	entry := record{Schema: Schema, Timestamp: time.Unix(1, 0).UTC().Format(time.RFC3339Nano), BootID: logger.bootID, Level: LevelInfo, Component: ComponentRuntime, Event: EventRuntimeOpened, NodeID: "10000000-0000-4000-8000-000000000000"}
	logger.write(entry)
	logger.write(entry)
	logger.write(entry)
	logger.writeSummaries(entry.Timestamp)

	var failureSummary map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(writer.Bytes()), &failureSummary); err != nil {
		t.Fatal(err)
	}
	if failureSummary["event"] != string(EventLoggerWriteError) || failureSummary["count"] != float64(3) {
		t.Fatalf("write failure summary lost its accumulated count: %s", writer.String())
	}

	writer.Reset()
	writer.failures = 1
	logger.dropped.Add(7)
	logger.writeSummaries(entry.Timestamp)
	logger.writeSummaries(entry.Timestamp)

	lines := bytes.Split(bytes.TrimSpace(writer.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected retained failure and dropped summaries, got %q", writer.String())
	}
	var writeFailure, droppedSummary map[string]any
	if err := json.Unmarshal(lines[0], &writeFailure); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &droppedSummary); err != nil {
		t.Fatal(err)
	}
	if writeFailure["event"] != string(EventLoggerWriteError) || writeFailure["count"] != float64(1) {
		t.Fatalf("summary sink failure was not retained: %s", lines[0])
	}
	if droppedSummary["event"] != string(EventLoggerDropped) || droppedSummary["count"] != float64(7) {
		t.Fatalf("dropped summary lost its accumulated count: %s", lines[1])
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
