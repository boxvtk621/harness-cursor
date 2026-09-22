package providerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNodeID = "11111111-1111-4111-8111-111111111111"

type fakeBackend struct {
	mu                   sync.Mutex
	result               ProbeResult
	replaced             string
	has                  bool
	busy                 bool
	probe                chan struct{}
	probeStarted         chan struct{}
	prepare              chan struct{}
	prepareStarted       chan struct{}
	ignorePrepareContext bool
	commit               chan struct{}
	commitStarted        chan struct{}
	ignoreCommitContext  bool
	returnNilReplacement bool
	closedCandidates     int
	logoutGate           chan struct{}
	logoutCalled         chan struct{}
}

func (backend *fakeBackend) Bootstrap(context.Context, bool) (bool, error) { return backend.has, nil }
func (backend *fakeBackend) Probe(ctx context.Context) ProbeResult {
	backend.mu.Lock()
	result := backend.result
	backend.mu.Unlock()
	if backend.probeStarted != nil {
		select {
		case backend.probeStarted <- struct{}{}:
		default:
		}
	}
	if backend.probe != nil {
		select {
		case <-backend.probe:
		case <-ctx.Done():
			return ProbeUnavailable
		}
	}
	return result
}
func (backend *fakeBackend) Replace(_ context.Context, secret string) error {
	backend.mu.Lock()
	backend.replaced = secret
	backend.has = true
	backend.mu.Unlock()
	return nil
}
func (backend *fakeBackend) PrepareReplacement(ctx context.Context, secret string) (Replacement, error) {
	if backend.prepareStarted != nil {
		select {
		case backend.prepareStarted <- struct{}{}:
		default:
		}
	}
	if backend.prepare != nil {
		if backend.ignorePrepareContext {
			<-backend.prepare
		} else {
			select {
			case <-backend.prepare:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if backend.returnNilReplacement {
		return nil, nil
	}
	return &fakeReplacement{backend: backend, secret: secret}, nil
}

type fakeReplacement struct {
	backend   *fakeBackend
	secret    string
	previous  string
	had       bool
	committed bool
	finished  bool
}

func (replacement *fakeReplacement) Commit(ctx context.Context) error {
	if replacement.backend.commitStarted != nil {
		select {
		case replacement.backend.commitStarted <- struct{}{}:
		default:
		}
	}
	if replacement.backend.commit != nil {
		if replacement.backend.ignoreCommitContext {
			<-replacement.backend.commit
		} else {
			select {
			case <-replacement.backend.commit:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	replacement.backend.mu.Lock()
	replacement.previous, replacement.had = replacement.backend.replaced, replacement.backend.has
	replacement.backend.replaced = replacement.secret
	replacement.backend.has = true
	replacement.backend.mu.Unlock()
	replacement.committed = true
	return nil
}

func (replacement *fakeReplacement) Rollback() error {
	if !replacement.committed || replacement.finished {
		return nil
	}
	replacement.backend.mu.Lock()
	replacement.backend.replaced, replacement.backend.has = replacement.previous, replacement.had
	replacement.backend.mu.Unlock()
	replacement.finished = true
	return nil
}

func (replacement *fakeReplacement) Finalize() error {
	replacement.finished = true
	return nil
}

func (replacement *fakeReplacement) Close() error {
	if replacement.committed && replacement.finished {
		return nil
	}
	replacement.backend.mu.Lock()
	replacement.backend.closedCandidates++
	replacement.backend.mu.Unlock()
	return nil
}
func (backend *fakeBackend) Logout(ctx context.Context) error {
	if backend.logoutCalled != nil {
		select {
		case backend.logoutCalled <- struct{}{}:
		default:
		}
	}
	if backend.logoutGate != nil {
		select {
		case <-backend.logoutGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	backend.mu.Lock()
	backend.has = false
	backend.mu.Unlock()
	return nil
}
func (backend *fakeBackend) Busy() bool {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.busy
}

func command(id string) CommandRequest { return CommandRequest{NodeID: testNodeID, CommandID: id} }

func waitOperation(t *testing.T, manager *Manager, id string, want OperationStatus) Envelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		envelope, issue := manager.Operation(context.Background(), testNodeID, id)
		if issue == nil && envelope.Operation != nil && envelope.Operation.Status == want {
			return envelope
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation %s did not become %s", id, want)
	return Envelope{}
}

func TestSecretStartIsDurableIdempotentAndNeverPersistsSecret(t *testing.T) {
	directory := t.TempDir()
	backend := &fakeBackend{result: ProbeAuthenticated}
	manager, err := Open(context.Background(), testNodeID, directory, backend)
	if err != nil {
		t.Fatal(err)
	}
	request := StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000001", Method: "secret", Secret: "write-only-value"}
	started, issue := manager.Start(context.Background(), request)
	if issue != nil || started.Operation == nil || started.Operation.Status != OperationPending {
		t.Fatalf("start=%+v issue=%+v", started, issue)
	}
	done := waitOperation(t, manager, started.Operation.OperationID, OperationSucceeded)
	if done.State != StateAuthenticated || backend.replaced != request.Secret {
		t.Fatalf("done=%+v backend=%q", done, backend.replaced)
	}
	replayed, issue := manager.Start(context.Background(), request)
	if issue != nil || replayed.Operation == nil || replayed.Operation.OperationID != started.Operation.OperationID {
		t.Fatalf("replay=%+v issue=%+v", replayed, issue)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), request.Secret) {
		t.Fatal("durable operation state contains the secret")
	}
	if info, err := os.Stat(filepath.Join(directory, "state.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestInvalidReplacementPreservesAuthenticatedState(t *testing.T) {
	backend := &fakeBackend{result: ProbeAuthenticated}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	first, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000002", Method: "secret", Secret: "valid"})
	if issue != nil {
		t.Fatal(issue)
	}
	waitOperation(t, manager, first.Operation.OperationID, OperationSucceeded)
	backend.mu.Lock()
	backend.result = ProbeInvalid
	backend.mu.Unlock()
	second, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000003", Method: "secret", Secret: "invalid"})
	if issue != nil {
		t.Fatal(issue)
	}
	failed := waitOperation(t, manager, second.Operation.OperationID, OperationFailed)
	if failed.State != StateAuthenticated || backend.replaced != "valid" {
		t.Fatalf("failed replacement changed active auth: %+v key=%q", failed, backend.replaced)
	}
}

func TestUnauthenticatedProbeCannotCommitReplacement(t *testing.T) {
	backend := &fakeBackend{result: ProbeUnauthenticated}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000017", Method: "secret", Secret: "candidate"})
	if issue != nil {
		t.Fatal(issue)
	}
	failed := waitOperation(t, manager, started.Operation.OperationID, OperationFailed)
	if failed.State == StateAuthenticated || backend.replaced != "" || failed.Operation.ReasonCode == nil || *failed.Operation.ReasonCode != "invalid_secret" {
		t.Fatalf("unauthenticated probe committed replacement: envelope=%+v replaced=%q", failed, backend.replaced)
	}
}

func TestReplacementExpiryDiscardsLateCandidateAndPreservesExistingAuth(t *testing.T) {
	backend := &fakeBackend{result: ProbeAuthenticated}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	first, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000018", Method: "secret", Secret: "existing"})
	if issue != nil {
		t.Fatal(issue)
	}
	waitOperation(t, manager, first.Operation.OperationID, OperationSucceeded)
	prepare, prepareStarted := make(chan struct{}), make(chan struct{}, 1)
	backend.prepare, backend.prepareStarted, backend.ignorePrepareContext = prepare, prepareStarted, true
	manager.timeout = 30 * time.Millisecond
	second, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000019", Method: "secret", Secret: "late"})
	if issue != nil || second.Operation == nil || second.Operation.TimeoutAt == nil {
		t.Fatalf("start=%+v issue=%+v", second, issue)
	}
	select {
	case <-prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement preparation did not start")
	}
	expired := waitOperation(t, manager, second.Operation.OperationID, OperationExpired)
	close(prepare)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		replaced, closed := backend.replaced, backend.closedCandidates
		backend.mu.Unlock()
		if replaced == "existing" && closed == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	backend.mu.Lock()
	replaced, closed := backend.replaced, backend.closedCandidates
	backend.mu.Unlock()
	if expired.State != StateAuthenticated || !manager.ProviderAuthReady() || replaced != "existing" || closed != 1 {
		t.Fatalf("late replacement escaped fence: envelope=%+v ready=%v replaced=%q closed=%d", expired, manager.ProviderAuthReady(), replaced, closed)
	}
}

func TestExpiryWatchdogPersistsWithoutReadPolling(t *testing.T) {
	directory := t.TempDir()
	prepare, prepareStarted := make(chan struct{}), make(chan struct{}, 1)
	backend := &fakeBackend{result: ProbeAuthenticated, prepare: prepare, prepareStarted: prepareStarted, ignorePrepareContext: true}
	manager, err := Open(context.Background(), testNodeID, directory, backend)
	if err != nil {
		t.Fatal(err)
	}
	manager.timeout = 25 * time.Millisecond
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000021", Method: "secret", Secret: "candidate"})
	if issue != nil || started.Operation == nil {
		t.Fatalf("start=%+v issue=%+v", started, issue)
	}
	select {
	case <-prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement preparation did not start")
	}
	deadline := time.Now().Add(time.Second)
	var persisted persistedState
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(filepath.Join(directory, "state.json"))
		if readErr == nil && json.Unmarshal(raw, &persisted) == nil && persisted.Operations[started.Operation.OperationID].Status == OperationExpired {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if persisted.Operations[started.Operation.OperationID].Status != OperationExpired {
		t.Fatalf("watchdog did not durably expire idle operation: %+v", persisted.Operations[started.Operation.OperationID])
	}
	close(prepare)
}

func TestDeadlineDuringCommitExpiresAndRollsBackCandidate(t *testing.T) {
	commit, commitStarted := make(chan struct{}), make(chan struct{}, 1)
	backend := &fakeBackend{result: ProbeAuthenticated, replaced: "existing", has: true, commit: commit, commitStarted: commitStarted, ignoreCommitContext: true}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	manager.timeout = 25 * time.Millisecond
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000022", Method: "secret", Secret: "late"})
	if issue != nil || started.Operation == nil {
		t.Fatalf("start=%+v issue=%+v", started, issue)
	}
	select {
	case <-commitStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement commit did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(commit)
	expired := waitOperation(t, manager, started.Operation.OperationID, OperationExpired)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		replaced := backend.replaced
		backend.mu.Unlock()
		if replaced == "existing" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	backend.mu.Lock()
	replaced := backend.replaced
	backend.mu.Unlock()
	if replaced != "existing" || manager.ProviderAuthReady() {
		t.Fatalf("late commit escaped rollback: envelope=%+v replaced=%q", expired, replaced)
	}
}

func TestInvalidProbeAndNilReplacementFailClosed(t *testing.T) {
	for name, backend := range map[string]*fakeBackend{
		"unknown probe":   {result: ProbeResult("future-value")},
		"nil replacement": {result: ProbeAuthenticated, returnNilReplacement: true},
	} {
		t.Run(name, func(t *testing.T) {
			manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
			if err != nil {
				t.Fatal(err)
			}
			started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000023", Method: "secret", Secret: "candidate"})
			if issue != nil || started.Operation == nil {
				t.Fatalf("start=%+v issue=%+v", started, issue)
			}
			failed := waitOperation(t, manager, started.Operation.OperationID, OperationFailed)
			if failed.Operation.ReasonCode == nil || *failed.Operation.ReasonCode != "provider_unavailable" {
				t.Fatalf("unexpected terminal result: %+v", failed)
			}
		})
	}
}

func TestReplacementCommitCannotOvertakeRuntimeBusyTransition(t *testing.T) {
	prepare, prepareStarted := make(chan struct{}), make(chan struct{}, 1)
	backend := &fakeBackend{result: ProbeAuthenticated, prepare: prepare, prepareStarted: prepareStarted}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	var gateMu sync.Mutex
	busy := false
	manager.SetTransitionGate(func() (func(), bool) {
		gateMu.Lock()
		return gateMu.Unlock, busy
	})
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000020", Method: "secret", Secret: "candidate"})
	if issue != nil || started.Operation == nil {
		t.Fatalf("start=%+v issue=%+v", started, issue)
	}
	select {
	case <-prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement preparation did not start")
	}
	gateMu.Lock()
	busy = true
	gateMu.Unlock()
	close(prepare)
	failed := waitOperation(t, manager, started.Operation.OperationID, OperationFailed)
	backend.mu.Lock()
	replaced, closed := backend.replaced, backend.closedCandidates
	backend.mu.Unlock()
	if failed.Operation.ReasonCode == nil || *failed.Operation.ReasonCode != "provider_unavailable" || replaced != "" || closed != 1 {
		t.Fatalf("busy transition accepted replacement: envelope=%+v replaced=%q closed=%d", failed, replaced, closed)
	}
}

func TestPendingConflictCancelAndBusyLogout(t *testing.T) {
	blocked := make(chan struct{})
	backend := &fakeBackend{result: ProbeAuthenticated, probe: blocked}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000004", Method: "secret", Secret: "value"})
	if issue != nil {
		t.Fatal(issue)
	}
	if manager.ProviderAuthReady() {
		t.Fatal("pending replacement left provider admission ready")
	}
	_, issue = manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000005", Method: "secret", Secret: "other"})
	if issue == nil || issue.Code != "pending_operation" {
		t.Fatalf("pending conflict=%+v", issue)
	}
	cancelled, issue := manager.Cancel(context.Background(), started.Operation.OperationID, command("20000000-0000-4000-8000-000000000006"))
	if issue != nil || cancelled.Operation.Status != OperationCancelled {
		t.Fatalf("cancel=%+v issue=%+v", cancelled, issue)
	}
	close(blocked)
	backend.mu.Lock()
	backend.busy = true
	backend.mu.Unlock()
	_, issue = manager.Logout(context.Background(), command("20000000-0000-4000-8000-000000000007"))
	if issue == nil || issue.Code != "busy" {
		t.Fatalf("busy logout=%+v", issue)
	}
}

func seedExpiredOperation(t *testing.T, manager *Manager, id string) {
	t.Helper()
	past := manager.timestamp(manager.clock().Add(-time.Second))
	now := manager.now()
	manager.mu.Lock()
	manager.state.Operations[id] = Operation{OperationID: id, CommandID: id, Method: "secret", Status: OperationPending, CreatedAt: now, UpdatedAt: now, TimeoutAt: &past}
	manager.state.LatestID = id
	if err := manager.persistLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
}

func TestExpiredOperationCannotBeCancelledOrBlockRefreshAndLogout(t *testing.T) {
	t.Run("cancel preserves expiry", func(t *testing.T) {
		manager, err := Open(context.Background(), testNodeID, t.TempDir(), &fakeBackend{})
		if err != nil {
			t.Fatal(err)
		}
		operationID := "30000000-0000-4000-8000-000000000024"
		seedExpiredOperation(t, manager, operationID)
		envelope, issue := manager.Cancel(context.Background(), operationID, command("20000000-0000-4000-8000-000000000024"))
		if issue != nil || envelope.Operation == nil || envelope.Operation.Status != OperationExpired {
			t.Fatalf("cancel=%+v issue=%+v", envelope, issue)
		}
	})

	t.Run("refresh probes after expiry", func(t *testing.T) {
		backend := &fakeBackend{result: ProbeAuthenticated, has: true}
		manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
		if err != nil {
			t.Fatal(err)
		}
		operationID := "30000000-0000-4000-8000-000000000025"
		seedExpiredOperation(t, manager, operationID)
		if err := manager.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		envelope, issue := manager.Operation(context.Background(), testNodeID, operationID)
		if issue != nil || envelope.Operation.Status != OperationExpired || envelope.State != StateAuthenticated {
			t.Fatalf("refresh=%+v issue=%+v", envelope, issue)
		}
	})

	t.Run("logout proceeds after expiry", func(t *testing.T) {
		backend := &fakeBackend{has: true}
		manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
		if err != nil {
			t.Fatal(err)
		}
		operationID := "30000000-0000-4000-8000-000000000026"
		seedExpiredOperation(t, manager, operationID)
		envelope, issue := manager.Logout(context.Background(), command("20000000-0000-4000-8000-000000000026"))
		if issue != nil || envelope.Operation == nil || envelope.Operation.Status != OperationExpired || envelope.State != StateUnauthenticated {
			t.Fatalf("logout=%+v issue=%+v", envelope, issue)
		}
	})
}

func TestExpiryPersistenceFailureRejectsNewMutation(t *testing.T) {
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), &fakeBackend{result: ProbeAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	seedExpiredOperation(t, manager, "30000000-0000-4000-8000-000000000027")
	manager.mu.Lock()
	manager.write = func(string, []byte) error { return errors.New("injected persistence failure") }
	manager.mu.Unlock()
	_, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000027", Method: "secret", Secret: "candidate"})
	if issue == nil || issue.Code != "provider_unavailable" {
		t.Fatalf("new mutation escaped failed expiry persistence: %+v", issue)
	}
}

func TestUnavailableCheckDoesNotClaimUnauthenticated(t *testing.T) {
	backend := &fakeBackend{result: ProbeUnavailable, has: true}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	envelope, issue := manager.Check(context.Background(), command("20000000-0000-4000-8000-000000000008"))
	if issue != nil || envelope.State != StateUnknown || envelope.ReasonCode == nil || *envelope.ReasonCode != "provider_unavailable" {
		t.Fatalf("check=%+v issue=%+v", envelope, issue)
	}
}

func TestRestartTerminatesPersistedPendingOperation(t *testing.T) {
	directory := t.TempDir()
	blocked := make(chan struct{})
	firstBackend := &fakeBackend{result: ProbeAuthenticated, probe: blocked}
	first, err := Open(context.Background(), testNodeID, directory, firstBackend)
	if err != nil {
		t.Fatal(err)
	}
	started, issue := first.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000009", Method: "secret", Secret: "pending"})
	if issue != nil {
		t.Fatal(issue)
	}
	second, err := Open(context.Background(), testNodeID, directory, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	recovered, issue := second.Operation(context.Background(), testNodeID, started.Operation.OperationID)
	if issue != nil || recovered.Operation.Status != OperationFailed || recovered.Operation.ReasonCode == nil || *recovered.Operation.ReasonCode != "interrupted_by_restart" {
		t.Fatalf("recovered=%+v issue=%+v", recovered, issue)
	}
	third, err := Open(context.Background(), testNodeID, directory, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	recoveredAgain, issue := third.Operation(context.Background(), testNodeID, started.Operation.OperationID)
	if issue != nil || recoveredAgain.Operation.Status != OperationFailed || recoveredAgain.Operation.ReasonCode == nil || *recoveredAgain.Operation.ReasonCode != "interrupted_by_restart" {
		t.Fatalf("second restart repeated/lost recovery: recovered=%+v issue=%+v", recoveredAgain, issue)
	}
	close(blocked)
	waitOperation(t, first, started.Operation.OperationID, OperationSucceeded)
}

func TestStartupCredentialStaysUnknownUntilSynchronousProbeCompletes(t *testing.T) {
	directory := t.TempDir()
	firstBackend := &fakeBackend{result: ProbeAuthenticated, has: true}
	first, err := Open(context.Background(), testNodeID, directory, firstBackend)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Refresh(context.Background()); err != nil || !first.ProviderAuthReady() {
		t.Fatalf("initial proof failed: ready=%v err=%v", first.ProviderAuthReady(), err)
	}
	probe := make(chan struct{})
	restarted, err := Open(context.Background(), testNodeID, directory, &fakeBackend{result: ProbeAuthenticated, has: true, probe: probe})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.ProviderAuthReady() {
		t.Fatal("persisted credential was trusted before restart proof")
	}
	done := make(chan error, 1)
	go func() { done <- restarted.Refresh(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("slow startup probe completed early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(probe)
	if err := <-done; err != nil || !restarted.ProviderAuthReady() {
		t.Fatalf("restart proof did not open admission: ready=%v err=%v", restarted.ProviderAuthReady(), err)
	}
}

func TestReceiptCapacityFailsClosedWithoutForgettingKnownCommand(t *testing.T) {
	backend := &fakeBackend{result: ProbeAuthenticated}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	knownCommand := "20000000-0000-4000-8000-000000000010"
	knownOperation := "30000000-0000-4000-8000-000000000010"
	manager.mu.Lock()
	manager.state.Operations[knownOperation] = Operation{OperationID: knownOperation, CommandID: knownCommand, Method: "secret", Status: OperationFailed, CreatedAt: manager.now(), UpdatedAt: manager.now()}
	manager.state.Commands[knownCommand] = commandRecord{Kind: "start:secret", OperationID: knownOperation}
	for index := 1; len(manager.state.Commands) < maxCommandReceipts; index++ {
		manager.state.Commands[fmt.Sprintf("receipt-%04d", index)] = commandRecord{Kind: "check"}
	}
	manager.mu.Unlock()
	replayed, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: knownCommand, Method: "secret", Secret: "ignored"})
	if issue != nil || replayed.Operation == nil || replayed.Operation.OperationID != knownOperation {
		t.Fatalf("known command was not replayed at capacity: envelope=%+v issue=%+v", replayed, issue)
	}
	_, issue = manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000011", Method: "secret", Secret: "new"})
	if issue == nil || issue.Code != "busy" || backend.replaced != "" {
		t.Fatalf("new command did not fail closed at capacity: issue=%+v replaced=%q", issue, backend.replaced)
	}
}

func TestTerminalReplacementPersistFailureNeverPublishesSuccess(t *testing.T) {
	probe := make(chan struct{})
	backend := &fakeBackend{result: ProbeAuthenticated, probe: probe}
	directory := t.TempDir()
	manager, err := Open(context.Background(), testNodeID, directory, backend)
	if err != nil {
		t.Fatal(err)
	}
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000012", Method: "secret", Secret: "valid"})
	if issue != nil {
		t.Fatal(issue)
	}
	manager.mu.Lock()
	manager.write = func(string, []byte) error { return errors.New("injected persistence failure") }
	manager.mu.Unlock()
	close(probe)
	failed := waitOperation(t, manager, started.Operation.OperationID, OperationFailed)
	if failed.State != StateUnknown || failed.ReasonCode == nil || *failed.ReasonCode != "provider_unavailable" || manager.ProviderAuthReady() {
		t.Fatalf("persistence failure published usable auth: %+v", failed)
	}
	manager.mu.Lock()
	manager.write = atomicPrivateWrite
	manager.mu.Unlock()
	restarted, err := Open(context.Background(), testNodeID, directory, &fakeBackend{has: true})
	if err != nil {
		t.Fatal(err)
	}
	recovered, issue := restarted.Operation(context.Background(), testNodeID, started.Operation.OperationID)
	if issue != nil || recovered.Operation == nil || recovered.Operation.Status == OperationSucceeded {
		t.Fatalf("restart observed phantom success: envelope=%+v issue=%+v", recovered, issue)
	}
}

func TestSlowOldCredentialCheckCannotOverwriteSuccessfulReplacement(t *testing.T) {
	probe := make(chan struct{})
	startedProbe := make(chan struct{}, 1)
	backend := &fakeBackend{result: ProbeUnauthenticated, probe: probe, probeStarted: startedProbe}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan *APIError, 1)
	go func() {
		_, issue := manager.Check(context.Background(), command("20000000-0000-4000-8000-000000000013"))
		checked <- issue
	}()
	select {
	case <-startedProbe:
	case <-time.After(time.Second):
		t.Fatal("old credential check did not start")
	}
	backend.mu.Lock()
	backend.result = ProbeAuthenticated
	backend.mu.Unlock()
	replacement := make(chan struct {
		envelope Envelope
		issue    *APIError
	}, 1)
	go func() {
		envelope, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000014", Method: "secret", Secret: "new"})
		replacement <- struct {
			envelope Envelope
			issue    *APIError
		}{envelope, issue}
	}()
	select {
	case <-replacement:
		t.Fatal("replacement overtook the in-flight old credential probe")
	case <-time.After(20 * time.Millisecond):
	}
	close(probe)
	if issue := <-checked; issue != nil {
		t.Fatalf("check failed: %+v", issue)
	}
	result := <-replacement
	if result.issue != nil || result.envelope.Operation == nil {
		t.Fatalf("replacement start=%+v issue=%+v", result.envelope, result.issue)
	}
	done := waitOperation(t, manager, result.envelope.Operation.OperationID, OperationSucceeded)
	if done.State != StateAuthenticated || !manager.ProviderAuthReady() {
		t.Fatalf("stale check overwrote replacement: %+v", done)
	}
}

func TestLogoutClosesReadinessForEntireMutation(t *testing.T) {
	backend := &fakeBackend{result: ProbeAuthenticated}
	manager, err := Open(context.Background(), testNodeID, t.TempDir(), backend)
	if err != nil {
		t.Fatal(err)
	}
	started, issue := manager.Start(context.Background(), StartRequest{NodeID: testNodeID, CommandID: "20000000-0000-4000-8000-000000000015", Method: "secret", Secret: "valid"})
	if issue != nil {
		t.Fatal(issue)
	}
	waitOperation(t, manager, started.Operation.OperationID, OperationSucceeded)
	backend.logoutGate = make(chan struct{})
	backend.logoutCalled = make(chan struct{}, 1)
	done := make(chan *APIError, 1)
	go func() {
		_, issue := manager.Logout(context.Background(), command("20000000-0000-4000-8000-000000000016"))
		done <- issue
	}()
	select {
	case <-backend.logoutCalled:
	case <-time.After(time.Second):
		t.Fatal("logout backend was not reached")
	}
	if manager.ProviderAuthReady() {
		t.Fatal("logout mutation left readiness open")
	}
	close(backend.logoutGate)
	if issue := <-done; issue != nil {
		t.Fatalf("logout failed: %+v", issue)
	}
	if manager.ProviderAuthReady() {
		t.Fatal("logout left provider ready")
	}
}
