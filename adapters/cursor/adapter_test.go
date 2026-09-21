package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/tools"
)

func TestStartResumeAndDurablePrivateMapping(t *testing.T) {
	config := fakeConfig(t)
	first, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	reference := testReference(1)
	start, err := first.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "first", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || start.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", start, err)
	}
	stream, err := first.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	if _, ok := events[0].(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", events[0])
	}
	message, ok := events[1].(harnessadapter.AssistantMessageEvent)
	if !ok || message.Content.Content != "reply:first" || message.FinishReason != "complete" || message.FullText == nil || *message.FullText != "reply:first" {
		t.Fatalf("assistant event = %#v", events[1])
	}
	terminal, ok := events[2].(harnessadapter.TerminalEvent)
	if !ok || terminal.Outcome != harnessadapter.ReconcileCompleted {
		t.Fatalf("terminal event = %#v", events[2])
	}
	mapping, err := os.ReadFile(filepath.Join(config.StateDir, "native-mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mapping) == "" || contains(mapping, []byte(config.APIKey)) {
		t.Fatal("native mapping is empty or contains the API key")
	}
	mappingInfo, err := os.Lstat(filepath.Join(config.StateDir, "native-mapping.json"))
	if err != nil || mappingInfo.Mode().Perm() != 0o600 {
		t.Fatalf("native mapping permissions = %v, %v", mappingInfo, err)
	}
	if err := first.Close(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// An explicit Close kills the long-lived worker; its wait status is not a
		// provider outcome and is intentionally ignored by callers as well.
	}

	second, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	resumedReference := testReference(2)
	resume, err := second.Resume(context.Background(), harnessadapter.ResumeInput{
		Attempt: resumedReference, Prompt: "resume", Policy: denyPolicy(), Context: testBoundary(2),
	})
	if err != nil || resume.Outcome != harnessadapter.ResumeStarted {
		t.Fatalf("resume = %#v, %v", resume, err)
	}
	resumed, err := second.Events(context.Background(), harnessadapter.EventsInput{Attempt: resumedReference})
	if err != nil {
		t.Fatal(err)
	}
	resumeEvents := readEvents(t, resumed, 3)
	if message, ok := resumeEvents[1].(harnessadapter.AssistantMessageEvent); !ok || message.Content.Content != "reply:resume:agent-1" || message.FullText == nil || *message.FullText != "reply:resume:agent-1" {
		t.Fatalf("resumed assistant event = %#v", resumeEvents[1])
	}
}

func TestResumeAcceptsExactRetryBoundaryAndRejectsConflictingReplay(t *testing.T) {
	adapter, err := New(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx := context.Background()
	boundary := testBoundary(1)
	first := testReference(1)
	started, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: first, Prompt: "first", Policy: denyPolicy(), Context: boundary,
	})
	if err != nil || started.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", started, err)
	}
	firstEvents, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: first})
	if err != nil {
		t.Fatal(err)
	}
	readEvents(t, firstEvents, 3)

	retry := testReference(2)
	resumed, err := adapter.Resume(ctx, harnessadapter.ResumeInput{
		Attempt: retry, Prompt: "first", Policy: denyPolicy(), Context: boundary,
	})
	if err != nil || resumed.Outcome != harnessadapter.ResumeStarted {
		t.Fatalf("exact retry resume = %#v, %v", resumed, err)
	}
	retryEvents, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: retry})
	if err != nil {
		t.Fatal(err)
	}
	readEvents(t, retryEvents, 3)

	conflicting := boundary
	conflicting.MessageID = testBoundary(2).MessageID
	rejected, err := adapter.Resume(ctx, harnessadapter.ResumeInput{
		Attempt: testReference(3), Prompt: "conflict", Policy: denyPolicy(), Context: conflicting,
	})
	if err != nil || rejected.Outcome != harnessadapter.ResumeContextMissing {
		t.Fatalf("conflicting retry boundary = %#v, %v", rejected, err)
	}
}

func TestLegacyDenyStartsWithoutWorkspaceOrToolRunner(t *testing.T) {
	config := fakeConfig(t)
	config.WorkingDir = ""
	config.ToolRunner = nil
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	started, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "legacy", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || started.Outcome != harnessadapter.StartStarted {
		t.Fatalf("legacy deny start = %#v, %v", started, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	if events := readEvents(t, stream, 3); len(events) != 3 {
		t.Fatalf("legacy deny events = %d", len(events))
	}
}

func TestExplicitToolsRejectMissingWorkspaceOrRunnerAtDispatch(t *testing.T) {
	for name, configure := range map[string]func(*Config){
		"workspace": func(config *Config) {
			config.WorkingDir = ""
			config.ToolRunner = &fakeRunner{requests: make(chan toolrunner.Request, 1)}
		},
		"runner": func(config *Config) { config.ToolRunner = nil },
	} {
		t.Run(name, func(t *testing.T) {
			config := fakeConfig(t)
			configure(&config)
			adapter, err := New(config, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
				Attempt: testReference(1), Prompt: "must reject", Policy: explicitPolicy(), Context: testBoundary(1),
			})
			if err != nil || result.Outcome != harnessadapter.StartRejected || result.Failure == nil || result.Failure.Code != "cursor_tool_runner_unavailable" {
				t.Fatalf("explicit tools without %s = %#v, %v", name, result, err)
			}
		})
	}
}

func TestDispatchIsAsyncAndCancelHasSeparateTerminal(t *testing.T) {
	adapter, err := New(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	startedAt := time.Now()
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "long", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted || time.Since(startedAt) > time.Second {
		t.Fatalf("asynchronous start = %#v, %v, elapsed %s", result, err, time.Since(startedAt))
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", first)
	}
	cancelled, err := adapter.Cancel(context.Background(), harnessadapter.CancelInput{Attempt: reference})
	if err != nil || cancelled.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	event, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := event.(harnessadapter.TerminalEvent)
	if !ok || terminal.Outcome != harnessadapter.ReconcileInterrupted || terminal.EffectStatus != "known" {
		t.Fatalf("cancel terminal = %#v", event)
	}
}

func TestSteerToolLifecycleAndPolicyRejection(t *testing.T) {
	adapter, err := New(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	identity, err := adapter.Identity(context.Background())
	if err != nil || harnessadapter.ValidateIdentity(identity) != nil {
		t.Fatalf("identity = %#v, %v", identity, err)
	}
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "tool", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	steered, err := adapter.Steer(context.Background(), harnessadapter.SteerInput{
		Attempt: reference, MessageID: "50000000-0000-4000-8000-000000000001", Text: "continue",
	})
	if err != nil || steered.Outcome != harnessadapter.SteerApplied {
		t.Fatalf("steer = %#v, %v", steered, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 5)
	started, ok := events[1].(harnessadapter.ToolStartedEvent)
	if !ok || started.ToolName != "fixture" || len(started.ActionHash) != 64 || started.Input.Kind != "unavailable" {
		t.Fatalf("tool start = %#v", events[1])
	}
	completed, ok := events[2].(harnessadapter.ToolCompletedEvent)
	if !ok || completed.CallID != started.CallID || completed.Status != "succeeded" || completed.Result.Kind != "unavailable" {
		t.Fatalf("tool completion = %#v", events[2])
	}

	unsupported := denyPolicy()
	unsupported.ToolManifest = []byte(`[{"name":"shell"}]`)
	sum := sha256.Sum256(unsupported.ToolManifest)
	unsupported.ToolManifestHash = hex.EncodeToString(sum[:])
	unsupported.EffectiveHash = harnessadapter.EffectivePolicyHash(unsupported)
	rejected, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: testReference(2), Prompt: "must reject", Policy: unsupported, Context: testBoundary(2),
	})
	if err != nil || rejected.Outcome != harnessadapter.StartRejected || rejected.Failure == nil || rejected.Failure.Class != harnessadapter.FailurePolicy {
		t.Fatalf("unsupported policy = %#v, %v", rejected, err)
	}
}

func TestReadCommandRunsImmediatelyInDialogWorkspace(t *testing.T) {
	runner := &fakeRunner{requests: make(chan toolrunner.Request, 1), result: toolrunner.Result{Success: true, Output: []byte("workspace\n")}}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "read-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 5)
	started, ok := events[1].(harnessadapter.ToolStartedEvent)
	if !ok || started.ToolName != "cursor.command" || !actionHashPattern.MatchString(started.ActionHash) || started.Input.Redaction != "none" || !strings.Contains(started.Input.Content, "Команда: pwd") {
		t.Fatalf("tool started = %#v", events[1])
	}
	if output, ok := events[2].(harnessadapter.ToolOutputEvent); !ok || output.CallID != started.CallID || output.Output.Content != "workspace\n" {
		t.Fatalf("tool output = %#v", events[2])
	}
	if completed, ok := events[3].(harnessadapter.ToolCompletedEvent); !ok || completed.Status != "succeeded" || completed.EffectStatus != "none" || completed.EffectRef != "" {
		t.Fatalf("tool completed = %#v", events[3])
	}
	request := <-runner.requests
	if request.Workspace != filepath.Join(config.WorkingDir, reference.DialogID) || request.Command == nil || request.Command.Access != toolrunner.AccessRead || request.Command.CWD != "." {
		t.Fatalf("runner request = %#v", request)
	}
	if info, err := os.Lstat(request.Workspace); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("dialog workspace = %v, %v", info, err)
	}
}

func TestFileChangeRequiresExactAllowOnce(t *testing.T) {
	runner := &fakeRunner{
		requests: make(chan toolrunner.Request, 1),
		result:   toolrunner.Result{Success: true, Output: []byte("changed"), Changes: []toolrunner.FileChangeResult{{Path: "note.txt", Operation: toolrunner.FileWrite}}},
	}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "file-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	started := events[1].(harnessadapter.ToolStartedEvent)
	requested, ok := events[2].(harnessadapter.ApprovalRequestedEvent)
	if !ok || requested.CallID != started.CallID || requested.ActionHash != started.ActionHash || !strings.Contains(requested.SafePrompt, "Запись: note.txt") || !strings.Contains(started.Input.Content, "Запись: note.txt") {
		t.Fatalf("approval requested = %#v", events[2])
	}
	if reconciled, err := adapter.Reconcile(context.Background(), harnessadapter.ReconcileInput{Attempt: reference}); err != nil || reconciled.Outcome != harnessadapter.ReconcileWaitingInput {
		t.Fatalf("waiting reconcile = %#v, %v", reconciled, err)
	}
	stale, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: strings.Repeat("a", 64), Decision: "allow_once",
	})
	if err != nil || stale.Outcome != harnessadapter.ResponseRejected {
		t.Fatalf("stale approval = %#v, %v", stale, err)
	}
	select {
	case request := <-runner.requests:
		t.Fatalf("runner started before approval: %#v", request)
	default:
	}
	allowed, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: requested.ActionHash, Decision: "allow_once",
	})
	if err != nil || allowed.Outcome != harnessadapter.ResponseApplied {
		t.Fatalf("allow once = %#v, %v", allowed, err)
	}
	duplicate, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: requested.ActionHash, Decision: "allow_once",
	})
	if err != nil || duplicate.Outcome != harnessadapter.ResponseRejected {
		t.Fatalf("duplicate approval = %#v, %v", duplicate, err)
	}
	request := <-runner.requests
	if request.Kind != toolrunner.KindFileChange || request.Workspace != filepath.Join(config.WorkingDir, reference.DialogID) || request.FileChange == nil || len(request.FileChange.Changes) != 1 || string(request.FileChange.Changes[0].Content) != "hello" {
		t.Fatalf("runner request = %#v", request)
	}
	remaining := readEvents(t, stream, 3)
	if completed, ok := remaining[1].(harnessadapter.ToolCompletedEvent); !ok || completed.Status != "succeeded" || completed.EffectStatus != "known" || completed.EffectRef != started.ActionHash {
		t.Fatalf("tool completed = %#v", remaining[1])
	}
}

func TestDeniedWriteCommandNeverStartsRunner(t *testing.T) {
	runner := &fakeRunner{requests: make(chan toolrunner.Request, 1), result: toolrunner.Result{Success: true}}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "write-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	requested := events[2].(harnessadapter.ApprovalRequestedEvent)
	denied, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: requested.ActionHash, Decision: "deny",
	})
	if err != nil || denied.Outcome != harnessadapter.ResponseApplied {
		t.Fatalf("deny = %#v, %v", denied, err)
	}
	remaining := readEvents(t, stream, 2)
	if completed, ok := remaining[0].(harnessadapter.ToolCompletedEvent); !ok || completed.Status != "failed" || completed.EffectStatus != "none" {
		t.Fatalf("denied completion = %#v", remaining[0])
	}
	select {
	case request := <-runner.requests:
		t.Fatalf("runner started after deny: %#v", request)
	default:
	}
}

func TestCancelWhileWaitingForApprovalClearsRequestWithoutStartingRunner(t *testing.T) {
	runner := &fakeRunner{requests: make(chan toolrunner.Request, 1)}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "write-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	requested, ok := events[2].(harnessadapter.ApprovalRequestedEvent)
	if !ok {
		t.Fatalf("approval requested = %#v", events[2])
	}
	if reconciled, err := adapter.Reconcile(context.Background(), harnessadapter.ReconcileInput{Attempt: reference}); err != nil || reconciled.Outcome != harnessadapter.ReconcileWaitingInput {
		t.Fatalf("waiting reconcile = %#v, %v", reconciled, err)
	}
	cancelled, err := adapter.Cancel(context.Background(), harnessadapter.CancelInput{Attempt: reference})
	if err != nil || cancelled.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		adapter.mu.Lock()
		pending := len(adapter.approvals)
		adapter.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending approvals after cancel = %d", pending)
		}
		time.Sleep(time.Millisecond)
	}
	stale, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: requested.ActionHash, Decision: "allow_once",
	})
	if err != nil || stale.Outcome != harnessadapter.ResponseRejected {
		t.Fatalf("approval after cancel = %#v, %v", stale, err)
	}
	select {
	case request := <-runner.requests:
		t.Fatalf("runner started while approval was canceled: %#v", request)
	default:
	}
}

func TestCancelReleasesApprovedRunnerBeforeNativeAcknowledgement(t *testing.T) {
	forbiddenPath := filepath.Join(t.TempDir(), "after-cancel.txt")
	runner := &cancelBlockingRunner{
		started: make(chan toolrunner.Request, 1), canceled: make(chan struct{}), forbiddenPath: forbiddenPath,
	}
	config := fakeConfig(t)
	config.ToolRunner = runner
	config.OperationTimeout = 2 * time.Second
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "blocking-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	requested, ok := events[2].(harnessadapter.ApprovalRequestedEvent)
	if !ok {
		t.Fatalf("approval requested = %#v", events[2])
	}
	allowed, err := adapter.RespondApproval(context.Background(), harnessadapter.RespondApprovalInput{
		Attempt: reference, ApprovalID: requested.ApprovalID, ApprovalVersion: 2,
		ActionHash: requested.ActionHash, Decision: "allow_once",
	})
	if err != nil || allowed.Outcome != harnessadapter.ResponseApplied {
		t.Fatalf("allow once = %#v, %v", allowed, err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("approved runner did not start")
	}
	startedAt := time.Now()
	cancelled, err := adapter.Cancel(context.Background(), harnessadapter.CancelInput{Attempt: reference})
	if err != nil || cancelled.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("cancel acknowledgement took %s", elapsed)
	}
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("runner context was not canceled")
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(forbiddenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runner wrote after cancellation: %v", err)
	}
}

func TestToolPreviewAndOutputRedactSecretLikeValues(t *testing.T) {
	secrets := []struct {
		name  string
		value string
	}{
		{name: "authorization", value: `Authorization: Bearer abcdefghijklmnop`},
		{name: "aws_secret_access_key", value: `AWS_SECRET_ACCESS_KEY=abcdefghijklmnopqrstuv`},
		{name: "openai_api_key", value: `OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz`},
		{name: "github_token", value: `GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz`},
		{name: "database_password", value: `DATABASE_PASSWORD=hunterhunter`},
		{name: "client_secret", value: `CLIENT_SECRET=abcdefghijklmnop`},
		{name: "session_token", value: `SESSION_TOKEN=abcdefghijklmnop`},
		{name: "private_key", value: `PRIVATE_KEY=abcdefghijklmnop`},
		{name: "bare_jwt", value: `eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dGVzdHNpZ25hdHVyZQ`},
		{name: "credential_url", value: `https://user:password@example.test/path`},
	}
	for _, secret := range secrets {
		t.Run(secret.name, func(t *testing.T) {
			request := toolrunner.Request{
				Kind: toolrunner.KindCommand,
				Command: &toolrunner.CommandRequest{
					Command: "printf %s " + secret.value, CWD: ".", Access: toolrunner.AccessWrite,
				},
			}
			input, prompt := safeToolInput(request)
			if input.Kind != "unavailable" || input.Redaction != "applied" || input.Content != "" || strings.Contains(prompt, secret.value) || !strings.Contains(prompt, "похоже на секрет") {
				t.Fatalf("secret preview = %#v, %q", input, prompt)
			}
			worker, event, full, incomplete := safeToolResult(toolrunner.Result{Success: true, Output: []byte(secret.value)})
			if !worker.OutputUnavailable || worker.Output != "" || event.Kind != "unavailable" || event.Redaction != "applied" || event.Content != "" || full != nil || incomplete {
				t.Fatalf("secret output = %#v, %#v", worker, event)
			}
		})
	}
}

func TestToolOutputIsBoundedIndependentlyOfRunner(t *testing.T) {
	original := strings.Repeat("🛠", harnessprotocol.MaximumMessageBytes)
	worker, event, full, incomplete := safeToolResult(toolrunner.Result{Success: true, Output: []byte(original)})
	if !worker.Truncated || !event.Truncated || len(worker.Output) > harnessprotocol.MaximumMessageBytes || len(event.Content) > harnessprotocol.MaximumMessageBytes || !utf8.ValidString(worker.Output) || full == nil || *full != original || incomplete {
		t.Fatalf("bounded worker output = %d bytes, event = %d bytes, truncated=%v/%v", len(worker.Output), len(event.Content), worker.Truncated, event.Truncated)
	}
	secretAfterBoundary := strings.Repeat("x", harnessprotocol.MaximumMessageBytes) + " api_key=abcdefghijklmnop"
	worker, event, full, incomplete = safeToolResult(toolrunner.Result{Success: true, Output: []byte(secretAfterBoundary)})
	if !worker.OutputUnavailable || worker.Output != "" || event.Kind != "unavailable" || event.Redaction != "applied" || full != nil || incomplete {
		t.Fatalf("secret after output boundary was exposed: %#v %#v", worker, event)
	}
	retained := strings.Repeat("x", harnessprotocol.MaximumMessageBytes+1)
	_, _, full, incomplete = safeToolResult(toolrunner.Result{Success: true, Output: []byte(retained), Truncated: true})
	if full == nil || *full != retained || !incomplete {
		t.Fatal("runner truncation was not carried as an explicit incomplete full-text source")
	}
}

func TestApprovalPromptFitsProtocolLimitsForLongASCIIAndUnicode(t *testing.T) {
	for name, command := range map[string]string{
		"ascii":   strings.Repeat("x", 32*1024),
		"unicode": strings.Repeat("🛠", 8192),
	} {
		t.Run(name, func(t *testing.T) {
			request := toolrunner.Request{
				Kind: toolrunner.KindCommand,
				Command: &toolrunner.CommandRequest{
					Command: command, CWD: ".", Access: toolrunner.AccessWrite,
				},
			}
			input, prompt := safeToolInput(request)
			if !utf8.ValidString(input.Content) || len(input.Content) > maximumToolPreviewBytes || !input.Truncated {
				t.Fatalf("tool input length = %d bytes, %d runes, truncated=%v", len(input.Content), utf8.RuneCountInString(input.Content), input.Truncated)
			}
			if !utf8.ValidString(prompt) || len(prompt) > maximumApprovalPromptBytes || utf8.RuneCountInString(prompt) > maximumApprovalPromptRunes || !strings.Contains(prompt, "Описание сокращено") || !strings.HasSuffix(prompt, "Разрешить один раз?") {
				t.Fatalf("approval prompt length = %d bytes, %d runes: %q", len(prompt), utf8.RuneCountInString(prompt), prompt)
			}
		})
	}
}

func TestNewRejectsCanonicalStateWorkspaceOverlapThroughSymlink(t *testing.T) {
	config := fakeConfig(t)
	realRoot := t.TempDir()
	config.StateDir = filepath.Join(realRoot, "state")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	config.WorkingDir = filepath.Join(alias, "state", "workspaces")
	adapter, err := New(config, nil)
	if adapter != nil || err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("canonical overlap = %#v, %v", adapter, err)
	}
}

func TestNewRejectsReservedWorkspaceRoots(t *testing.T) {
	for _, workspace := range []string{"/auth/dialogs", "/config", "/state/cursor/workspaces"} {
		t.Run(workspace, func(t *testing.T) {
			config := fakeConfig(t)
			config.WorkingDir = workspace
			adapter, err := New(config, nil)
			if adapter != nil || err == nil {
				t.Fatalf("reserved workspace = %#v, %v", adapter, err)
			}
		})
	}
}

func TestCursorToolManifestHasOneCanonicalByteRepresentation(t *testing.T) {
	const expected = "[{\"name\":\"cursor.command\"},{\"name\":\"cursor.file_change\"}]\n"
	if cursorExplicitToolManifest != expected || !strings.HasSuffix(cursorExplicitToolManifest, "\n") {
		t.Fatalf("canonical manifest bytes = %q", cursorExplicitToolManifest)
	}
	policy := explicitPolicy()
	if !validCursorToolManifest(policy) {
		t.Fatal("canonical explicit manifest was rejected")
	}
	for name, manifest := range map[string]string{
		"missing final newline": strings.TrimSuffix(expected, "\n"),
		"pretty encoding":       "[\n  {\"name\": \"cursor.command\"},\n  {\"name\": \"cursor.file_change\"}\n]\n",
		"reordered":             "[{\"name\":\"cursor.file_change\"},{\"name\":\"cursor.command\"}]\n",
		"extra field":           "[{\"name\":\"cursor.command\",\"provider\":\"cursor\"},{\"name\":\"cursor.file_change\"}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			policy.ToolManifest = []byte(manifest)
			if validCursorToolManifest(policy) {
				t.Fatalf("non-canonical manifest accepted: %q", manifest)
			}
		})
	}
}

func TestWorkerToolRequestParsingIsStrictAndBounded(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "dialog")
	invalid := []workerToolRequest{
		{Name: "cursor.command", CallID: "call", Args: json.RawMessage(`{"command":"pwd","cwd":".","workspaceAccess":"read"}`)},
		{Name: "cursor_command", CallID: "call", Args: json.RawMessage(`{"command":"pwd","cwd":".","workspaceAccess":"read","extra":true}`)},
		{Name: "cursor_command", CallID: "call", Args: json.RawMessage(`{"command":"pwd","cwd":".","workspaceAccess":"read","timeoutMs":9223372036854775807}`)},
		{Name: "cursor_file_change", CallID: "call", Args: json.RawMessage(`{"changes":[{"path":"../state","operation":"write","content":"x"}]}`)},
		{Name: "cursor_file_change", CallID: "call", Args: json.RawMessage(`{"changes":[{"path":"note.txt","operation":"delete"}]}`)},
	}
	for index, payload := range invalid {
		if request, err := runnerRequest(workspace, payload); err == nil {
			t.Fatalf("invalid request %d accepted: %#v", index, request)
		}
	}
	var envelope workerToolRequest
	if decodeStrict([]byte(`{"attemptKey":"attempt","callId":"call","name":"cursor_command","args":{},"extra":"secret"}`), &envelope) == nil {
		t.Fatal("worker tool envelope with unknown field was accepted")
	}
}

func TestDispatchReplayRejectsPolicyOrContextMismatch(t *testing.T) {
	config := fakeConfig(t)
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	policy := denyPolicy()
	boundary := testBoundary(1)
	if err := adapter.store.putIntent(reference, boundary, policy.EffectiveHash); err != nil {
		t.Fatal(err)
	}
	changed := denyPolicy()
	changed.Content = []byte("different deny policy")
	contentSum := sha256.Sum256(changed.Content)
	changed.ContentHash = hex.EncodeToString(contentSum[:])
	changed.EffectiveHash = harnessadapter.EffectivePolicyHash(changed)
	failure, err := adapter.dispatch(context.Background(), reference, "replay", boundary, changed, "")
	if err != nil || failure == nil || failure.Class != harnessadapter.FailureProtocol || failure.Code != "cursor_dispatch_conflict" {
		t.Fatalf("policy replay = %#v, %v", failure, err)
	}
	failure, err = adapter.dispatch(context.Background(), reference, "replay", testBoundary(2), policy, "")
	if err != nil || failure == nil || failure.Class != harnessadapter.FailureProtocol || failure.Code != "cursor_dispatch_conflict" {
		t.Fatalf("context replay = %#v, %v", failure, err)
	}
}

func TestCloseRejectsAndClearsPendingApproval(t *testing.T) {
	runner := &fakeRunner{requests: make(chan toolrunner.Request, 1)}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "write-tool", Policy: explicitPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	_ = readEvents(t, stream, 3)
	_ = adapter.Close()
	deadline := time.Now().Add(time.Second)
	for {
		adapter.mu.Lock()
		pending := len(adapter.approvals)
		adapter.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending approvals after close = %d", pending)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case request := <-runner.requests:
		t.Fatalf("runner started during teardown: %#v", request)
	default:
	}
}

func TestLostAcknowledgementIsNotBlindlyRedispatched(t *testing.T) {
	config := fakeConfig(t)
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	input := harnessadapter.StartInput{Attempt: reference, Prompt: "lost", Policy: denyPolicy(), Context: testBoundary(1)}
	// Inject the lost acknowledgement timeout after worker initialization.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	first, err := adapter.Start(ctx, input)
	if err == nil || first.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("lost acknowledgement = %#v, %v", first, err)
	}
	second, err := adapter.Start(context.Background(), input)
	if err != nil || second.Outcome != harnessadapter.StartUnknown || second.Failure == nil {
		t.Fatalf("repeat after lost acknowledgement = %#v, %v", second, err)
	}
}

func readEvents(t *testing.T, stream harnessadapter.EventStream, count int) []harnessadapter.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make([]harnessadapter.Event, 0, count)
	for len(events) < count {
		event, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("read event %d: %v", len(events), err)
		}
		events = append(events, event)
	}
	return events
}

func denyPolicy() harnessadapter.PolicySnapshot {
	content := []byte("deny all tools")
	tools := []byte(`[]`)
	contentSum := sha256.Sum256(content)
	toolsSum := sha256.Sum256(tools)
	policy := harnessadapter.PolicySnapshot{
		Revision: "cursor-alpha@1", Content: content, ContentHash: hex.EncodeToString(contentSum[:]),
		ToolManifest: tools, ToolManifestHash: hex.EncodeToString(toolsSum[:]), ApprovalMode: harnessadapter.ApprovalModeDeny,
	}
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return policy
}

func explicitPolicy() harnessadapter.PolicySnapshot {
	content := []byte("allow tools")
	tools := []byte(cursorExplicitToolManifest)
	contentSum := sha256.Sum256(content)
	toolsSum := sha256.Sum256(tools)
	policy := harnessadapter.PolicySnapshot{
		Revision: "cursor-tools@1", Content: content, ContentHash: hex.EncodeToString(contentSum[:]),
		ToolManifest: tools, ToolManifestHash: hex.EncodeToString(toolsSum[:]), ApprovalMode: harnessadapter.ApprovalModeExplicitOnce,
	}
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return policy
}

type fakeRunner struct {
	requests chan toolrunner.Request
	result   toolrunner.Result
	err      error
}

func (runner *fakeRunner) SelfTest(context.Context) error { return nil }

func (runner *fakeRunner) Run(_ context.Context, request toolrunner.Request) (toolrunner.Result, error) {
	runner.requests <- request
	return runner.result, runner.err
}

type cancelBlockingRunner struct {
	started       chan toolrunner.Request
	canceled      chan struct{}
	forbiddenPath string
}

func (*cancelBlockingRunner) SelfTest(context.Context) error { return nil }

func (runner *cancelBlockingRunner) Run(ctx context.Context, request toolrunner.Request) (toolrunner.Result, error) {
	runner.started <- request
	<-ctx.Done()
	close(runner.canceled)
	if ctx.Err() == nil {
		_ = os.WriteFile(runner.forbiddenPath, []byte("unexpected"), 0o600)
	}
	return toolrunner.Result{}, ctx.Err()
}

func testReference(generation int64) harnessadapter.AttemptRef {
	last := byte('0' + generation)
	return harnessadapter.AttemptRef{
		NodeID: "10000000-0000-4000-8000-000000000001", DialogID: "20000000-0000-4000-8000-000000000001",
		RequestID: "30000000-0000-4000-8000-00000000000" + string(last), AttemptID: "40000000-0000-4000-8000-00000000000" + string(last), Generation: generation,
	}
}

func testBoundary(sequence int64) harnessadapter.ContextBoundary {
	return harnessadapter.ContextBoundary{MessageID: "50000000-0000-4000-8000-00000000000" + string(byte('0'+sequence)), Sequence: sequence}
}

func fakeConfig(t *testing.T) Config {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node executable is unavailable")
	}
	workerPath := filepath.Join(t.TempDir(), "fake-worker.mjs")
	if err := os.WriteFile(workerPath, []byte(fakeWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		NodeExecutable: nodePath, WorkerEntrypoint: workerPath, StateDir: filepath.Join(t.TempDir(), "state"),
		WorkingDir: filepath.Join(t.TempDir(), "workspace"),
		APIKey:     "test-key-never-provider", OperationTimeout: time.Second, MaxFrameBytes: 64 * 1024,
	}
}

func contains(value, part []byte) bool {
	if len(part) == 0 || len(part) > len(value) {
		return false
	}
	for index := 0; index <= len(value)-len(part); index++ {
		if string(value[index:index+len(part)]) == string(part) {
			return true
		}
	}
	return false
}

const fakeWorker = `
import readline from 'node:readline';
const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
const active = new Map();
const pendingCancels = new Map();
const send = (value) => process.stdout.write(JSON.stringify(value) + '\n');
for await (const line of lines) {
  const frame = JSON.parse(line);
  const payload = frame.payload || {};
  if (frame.operation === 'init') {
    send({ type: 'response', id: frame.id, ok: true, result: { version: '1.0.31' } });
  } else if (frame.operation === 'dispatch') {
    if (payload.policyContent !== 'deny all tools' && payload.policyContent !== 'allow tools') {
      send({ type: 'response', id: frame.id, ok: false, code: 'rejected' });
      continue;
    }
    if (payload.prompt === 'lost') continue;
    const agentId = payload.resumeAgentId || 'agent-1';
    active.set(payload.attemptKey, { runId: 'run-' + payload.attemptKey, prompt: payload.prompt, agentId });
    send({ type: 'response', id: frame.id, ok: true, result: { agentId, runId: 'run-' + payload.attemptKey } });
    if (payload.prompt === 'read-tool') {
      send({ type: 'request', id: 'worker-read', operation: 'execute_tool', payload: { attemptKey: payload.attemptKey, callId: 'native-read', name: 'cursor_command', args: { command: 'pwd', cwd: '.', workspaceAccess: 'read' } } });
    } else if (payload.prompt === 'write-tool') {
      send({ type: 'request', id: 'worker-write', operation: 'execute_tool', payload: { attemptKey: payload.attemptKey, callId: 'native-write', name: 'cursor_command', args: { command: 'touch note.txt', cwd: '.', workspaceAccess: 'write' } } });
    } else if (payload.prompt === 'blocking-tool') {
      send({ type: 'request', id: 'worker-blocking', operation: 'execute_tool', payload: { attemptKey: payload.attemptKey, callId: 'native-blocking', name: 'cursor_command', args: { command: 'long-running-write', cwd: '.', workspaceAccess: 'write' } } });
    } else if (payload.prompt === 'file-tool') {
      send({ type: 'request', id: 'worker-file', operation: 'execute_tool', payload: { attemptKey: payload.attemptKey, callId: 'native-file', name: 'cursor_file_change', args: { changes: [{ path: 'note.txt', operation: 'write', content: 'hello' }] } } });
    } else if (payload.prompt === 'tool') {
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'tool', callId: 'private-call', name: 'fixture', status: 'running' });
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'tool', callId: 'private-call', name: 'fixture', status: 'completed' });
    } else if (payload.prompt !== 'long') {
      setTimeout(() => send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'finished', text: 'reply:' + payload.prompt + (payload.resumeAgentId ? ':' + payload.resumeAgentId : ''), usage: { inputTokens: 2, outputTokens: 1, totalTokens: 3 } }), 10);
    }
  } else if (frame.type === 'response' && frame.id.startsWith('worker-')) {
    const suffix = frame.id.slice('worker-'.length);
    for (const [attemptKey, run] of active) {
      if (run.prompt === suffix + '-tool') {
        const cancelId = pendingCancels.get(attemptKey);
        if (cancelId) {
          send({ type: 'response', id: cancelId, ok: true, result: {} });
          send({ type: 'event', attemptKey, event: 'terminal', status: 'cancelled' });
          pendingCancels.delete(attemptKey);
        } else {
          send({ type: 'event', attemptKey, event: 'terminal', status: 'finished', text: frame.ok ? 'reply:' + suffix : 'reply:tool-error' });
        }
        active.delete(attemptKey);
        break;
      }
    }
  } else if (frame.operation === 'steer') {
    const run = active.get(payload.attemptKey);
    const accepted = run && run.runId === payload.runId;
    send(accepted ? { type: 'response', id: frame.id, ok: true, result: { status: 'complete_delivered' } } : { type: 'response', id: frame.id, ok: false, code: 'rejected' });
    if (accepted && run.prompt === 'tool') {
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'finished', text: 'reply:tool' });
      active.delete(payload.attemptKey);
    }
  } else if (frame.operation === 'cancel') {
    const run = active.get(payload.attemptKey);
    if (!run || run.runId !== payload.runId) send({ type: 'response', id: frame.id, ok: false, code: 'rejected' });
    else if (run.prompt === 'blocking-tool') pendingCancels.set(payload.attemptKey, frame.id);
    else {
      send({ type: 'response', id: frame.id, ok: true, result: {} });
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'cancelled' });
      active.delete(payload.attemptKey);
    }
  }
}
`
