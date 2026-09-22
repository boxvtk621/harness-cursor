package providerauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

const (
	maxCommandReceipts  = 4096
	providerAuthTimeout = 30 * time.Second
)

type ProbeResult string

const (
	ProbeAuthenticated   ProbeResult = "authenticated"
	ProbeUnauthenticated ProbeResult = "unauthenticated"
	ProbeInvalid         ProbeResult = "invalid"
	ProbeUnavailable     ProbeResult = "unavailable"
)

type Backend interface {
	Bootstrap(context.Context, bool) (bool, error)
	Probe(context.Context) ProbeResult
	Logout(context.Context) error
	Busy() bool
}

// Replacement separates slow candidate construction from the fenced credential
// commit. Commit is provisional until Finalize: Rollback must restore the prior
// adapter and credential if cancellation, expiry, or durable persistence wins.
type Replacement interface {
	Commit(context.Context) error
	Rollback() error
	Finalize() error
	Close() error
}

type replacementBackend interface {
	PrepareReplacement(context.Context, string) (Replacement, error)
}

type commandRecord struct {
	Kind        string `json:"kind"`
	OperationID string `json:"operationId,omitempty"`
}

type persistedState struct {
	Initialized bool                     `json:"initialized"`
	Revision    int64                    `json:"revision"`
	State       State                    `json:"state"`
	CheckedAt   *string                  `json:"checkedAt"`
	ReasonCode  *string                  `json:"reasonCode"`
	LatestID    string                   `json:"latestOperationId,omitempty"`
	Operations  map[string]Operation     `json:"operations"`
	Commands    map[string]commandRecord `json:"commands"`
}

type Manager struct {
	nodeID  string
	dir     string
	path    string
	backend Backend
	clock   func() time.Time
	write   func(string, []byte) error
	timeout time.Duration

	mu            sync.Mutex
	actionMu      sync.Mutex
	state         persistedState
	cancels       map[string]context.CancelFunc
	transition    func() (release func(), busy bool)
	transitioning bool
}

func Open(ctx context.Context, nodeID, directory string, backend Backend) (*Manager, error) {
	if !uuidPattern.MatchString(nodeID) || !filepath.IsAbs(directory) || backend == nil {
		return nil, errors.New("provider auth config is incomplete")
	}
	if _, ok := backend.(replacementBackend); !ok {
		return nil, errors.New("provider auth backend does not support transactional replacement")
	}
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, err
	}
	manager := &Manager{nodeID: nodeID, dir: directory, path: filepath.Join(directory, "state.json"), backend: backend, clock: time.Now, write: atomicPrivateWrite, timeout: providerAuthTimeout, cancels: make(map[string]context.CancelFunc)}
	firstRun, err := manager.load()
	if err != nil {
		return nil, err
	}
	hasCredential, err := backend.Bootstrap(ctx, firstRun)
	if err != nil {
		return nil, err
	}
	manager.mu.Lock()
	if firstRun {
		manager.state.Initialized = true
		manager.state.Revision = 1
	}
	// A persisted credential is never accepted as startup proof. Admission stays
	// closed until the synchronous provider probe completes.
	manager.state.State = StateUnauthenticated
	manager.state.ReasonCode = nil
	manager.state.CheckedAt = nil
	if hasCredential {
		manager.state.State = StateUnknown
	}
	if !firstRun {
		manager.state.Revision++
	}
	recovered := false
	for id, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			reason := "interrupted_by_restart"
			operation.Status, operation.ReasonCode = OperationFailed, &reason
			operation.UpdatedAt = manager.now()
			manager.state.Operations[id] = operation
			manager.state.Revision++
			recovered = true
		}
	}
	if firstRun || recovered || manager.state.Revision > 0 {
		if err := manager.persistLocked(); err != nil {
			manager.mu.Unlock()
			return nil, err
		}
	}
	manager.mu.Unlock()
	return manager, nil
}

// SetTransitionGate connects authentication mutation to the runtime's native
// start barrier. The callback must acquire that barrier, evaluate busy while it
// is held, and return a release function.
func (manager *Manager) SetTransitionGate(gate func() (func(), bool)) {
	manager.actionMu.Lock()
	manager.transition = gate
	manager.actionMu.Unlock()
}

func (manager *Manager) beginTransition() (func(), bool) {
	if manager.transition != nil {
		return manager.transition()
	}
	return func() {}, manager.backend.Busy()
}

func (manager *Manager) load() (bool, error) {
	raw, err := os.ReadFile(manager.path)
	if errors.Is(err, os.ErrNotExist) {
		manager.state = persistedState{Operations: map[string]Operation{}, Commands: map[string]commandRecord{}}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(manager.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false, errors.New("provider auth state file is not private")
	}
	if json.Unmarshal(raw, &manager.state) != nil || !manager.state.Initialized || manager.state.Revision < 1 {
		return false, errors.New("provider auth state is invalid")
	}
	if manager.state.Operations == nil {
		manager.state.Operations = map[string]Operation{}
	}
	if manager.state.Commands == nil {
		manager.state.Commands = map[string]commandRecord{}
	}
	return false, nil
}

func (manager *Manager) ProviderAuthReady() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		return false
	}
	if manager.state.State != StateAuthenticated || manager.transitioning {
		return false
	}
	for _, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			return false
		}
	}
	return true
}

func (manager *Manager) Snapshot(_ context.Context, nodeID string) (Envelope, *APIError) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if nodeID != manager.nodeID {
		return Envelope{}, ErrNotFound
	}
	if manager.expirePendingLocked(manager.clock()) != nil {
		return Envelope{}, ErrProviderUnavailable
	}
	return manager.envelopeLocked(""), nil
}

func (manager *Manager) Operation(_ context.Context, nodeID, operationID string) (Envelope, *APIError) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if nodeID != manager.nodeID {
		return Envelope{}, ErrNotFound
	}
	if !uuidPattern.MatchString(operationID) {
		return Envelope{}, ErrInvalidRequest
	}
	if manager.expirePendingLocked(manager.clock()) != nil {
		return Envelope{}, ErrProviderUnavailable
	}
	if _, ok := manager.state.Operations[operationID]; !ok {
		return Envelope{}, ErrNotFound
	}
	return manager.envelopeLocked(operationID), nil
}

func (manager *Manager) Start(_ context.Context, request StartRequest) (Envelope, *APIError) {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	if request.NodeID != manager.nodeID {
		return Envelope{}, ErrNotFound
	}
	if !uuidPattern.MatchString(request.CommandID) {
		return Envelope{}, ErrInvalidRequest
	}
	if request.Method != "secret" {
		return Envelope{}, ErrUnsupportedMethod
	}
	if request.Secret == "" || len(request.Secret) > 16*1024 || containsControl(request.Secret) {
		return Envelope{}, ErrInvalidSecret
	}
	release, busy := manager.beginTransition()
	defer release()
	manager.mu.Lock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	if prior, ok := manager.state.Commands[request.CommandID]; ok {
		defer manager.mu.Unlock()
		if prior.Kind != "start:secret" {
			return Envelope{}, ErrIDConflict
		}
		return manager.envelopeLocked(prior.OperationID), nil
	}
	if busy {
		manager.mu.Unlock()
		return Envelope{}, ErrBusy
	}
	for _, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			manager.mu.Unlock()
			return Envelope{}, ErrPendingOperation
		}
	}
	if issue := manager.ensureCommandCapacityLocked(); issue != nil {
		manager.mu.Unlock()
		return Envelope{}, issue
	}
	operationID, err := randomUUID()
	if err != nil {
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	nowTime := manager.clock()
	now := manager.timestamp(nowTime)
	deadline := nowTime.Add(manager.timeout)
	timeout := manager.timestamp(deadline)
	operation := Operation{OperationID: operationID, CommandID: request.CommandID, Method: "secret", Status: OperationPending, CreatedAt: now, UpdatedAt: now, TimeoutAt: &timeout}
	manager.state.Operations[operationID] = operation
	manager.state.Commands[request.CommandID] = commandRecord{Kind: "start:secret", OperationID: operationID}
	manager.state.LatestID = operationID
	manager.state.Revision++
	if err := manager.persistLocked(); err != nil {
		delete(manager.state.Operations, operationID)
		delete(manager.state.Commands, request.CommandID)
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	operationContext, cancel := context.WithDeadline(context.Background(), deadline)
	manager.cancels[operationID] = cancel
	envelope := manager.envelopeLocked(operationID)
	manager.mu.Unlock()
	go manager.expireOperation(operationContext, operationID, deadline)
	go manager.replace(operationContext, operationID, request.Secret)
	return envelope, nil
}

func (manager *Manager) replace(ctx context.Context, operationID, secret string) {
	result := manager.backend.Probe(withSecret(ctx, secret))
	switch result {
	case ProbeAuthenticated:
	case ProbeInvalid, ProbeUnauthenticated, ProbeUnavailable:
		manager.finishReplacement(ctx, operationID, result, nil)
		return
	default:
		manager.finishReplacement(ctx, operationID, ProbeUnavailable, nil)
		return
	}
	replacement, err := manager.prepareReplacement(ctx, secret)
	if err != nil || replacement == nil {
		manager.finishReplacement(ctx, operationID, ProbeUnavailable, nil)
		return
	}
	manager.finishReplacement(ctx, operationID, ProbeAuthenticated, replacement)
	if err := replacement.Close(); err != nil {
		manager.recordReplacementCleanupFailure(operationID)
	}
}

func (manager *Manager) prepareReplacement(ctx context.Context, secret string) (Replacement, error) {
	return manager.backend.(replacementBackend).PrepareReplacement(ctx, secret)
}

func (manager *Manager) finishReplacement(ctx context.Context, operationID string, result ProbeResult, replacement Replacement) {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	release := func() {}
	busy := false
	if result == ProbeAuthenticated && replacement != nil {
		release, busy = manager.beginTransition()
	}
	defer release()
	manager.mu.Lock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		manager.mu.Unlock()
		if replacement != nil {
			_ = replacement.Rollback()
		}
		return
	}
	operation, current := manager.state.Operations[operationID]
	if !current || operation.Status != OperationPending {
		manager.mu.Unlock()
		return
	}
	terminalize := func(status OperationStatus, reason string) {
		operation.Status, operation.ReasonCode, operation.UpdatedAt = status, &reason, manager.now()
		manager.state.Operations[operationID] = operation
		if cancel := manager.cancels[operationID]; cancel != nil {
			cancel()
		}
		delete(manager.cancels, operationID)
		manager.state.Revision++
		if manager.persistLocked() != nil {
			manager.failClosedLocked()
			_ = manager.persistLocked()
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		terminalize(OperationExpired, "expired")
		manager.mu.Unlock()
		return
	} else if ctx.Err() != nil {
		terminalize(OperationCancelled, "cancelled")
		manager.mu.Unlock()
		return
	} else if result == ProbeInvalid || result == ProbeUnauthenticated {
		terminalize(OperationFailed, "invalid_secret")
		manager.mu.Unlock()
		return
	} else if result == ProbeUnavailable || busy {
		terminalize(OperationFailed, "provider_unavailable")
		manager.mu.Unlock()
		return
	}
	manager.transitioning = true
	manager.mu.Unlock()

	commitErr := replacement.Commit(ctx)
	manager.mu.Lock()
	manager.transitioning = false
	expireErr := manager.expirePendingLocked(manager.clock())
	operation, current = manager.state.Operations[operationID]
	if expireErr != nil || !current || operation.Status != OperationPending || ctx.Err() != nil || commitErr != nil {
		if current && operation.Status == OperationPending {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				terminalize(OperationExpired, "expired")
			} else if ctx.Err() != nil {
				terminalize(OperationCancelled, "cancelled")
			} else {
				terminalize(OperationFailed, "provider_unavailable")
			}
		}
		manager.mu.Unlock()
		if rollbackErr := replacement.Rollback(); rollbackErr != nil {
			manager.recordReplacementCleanupFailure(operationID)
		}
		return
	}
	operation.Status, operation.ReasonCode, operation.UpdatedAt = OperationSucceeded, nil, manager.now()
	manager.state.Operations[operationID] = operation
	manager.state.State, manager.state.ReasonCode = StateAuthenticated, nil
	checked := manager.now()
	manager.state.CheckedAt = &checked
	if cancel := manager.cancels[operationID]; cancel != nil {
		cancel()
	}
	delete(manager.cancels, operationID)
	manager.state.Revision++
	if err := manager.persistLocked(); err != nil {
		reason := "provider_unavailable"
		operation.Status, operation.ReasonCode, operation.UpdatedAt = OperationFailed, &reason, manager.now()
		manager.state.Operations[operationID] = operation
		manager.failClosedLocked()
		_ = manager.persistLocked()
		manager.mu.Unlock()
		if rollbackErr := replacement.Rollback(); rollbackErr != nil {
			manager.recordReplacementCleanupFailure(operationID)
		}
		return
	}
	manager.mu.Unlock()
	if err := replacement.Finalize(); err != nil {
		manager.recordReplacementCleanupFailure(operationID)
	}
}

func (manager *Manager) recordReplacementCleanupFailure(operationID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if operation, ok := manager.state.Operations[operationID]; ok && operation.Status == OperationPending {
		reason := "provider_unavailable"
		operation.Status, operation.ReasonCode, operation.UpdatedAt = OperationFailed, &reason, manager.now()
		manager.state.Operations[operationID] = operation
	}
	manager.failClosedLocked()
	_ = manager.persistLocked()
}

func (manager *Manager) expireOperation(ctx context.Context, operationID string, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	operation, ok := manager.state.Operations[operationID]
	if !ok || operation.Status != OperationPending {
		return
	}
	_ = manager.expirePendingLocked(manager.clock())
}

func (manager *Manager) Check(ctx context.Context, request CommandRequest) (Envelope, *APIError) {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	if issue := manager.validateCommand(request, "check"); issue != nil {
		return Envelope{}, issue
	}
	manager.mu.Lock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	if prior, ok := manager.state.Commands[request.CommandID]; ok {
		defer manager.mu.Unlock()
		if prior.Kind != "check" {
			return Envelope{}, ErrIDConflict
		}
		return manager.envelopeLocked(""), nil
	}
	for _, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			manager.mu.Unlock()
			return Envelope{}, ErrPendingOperation
		}
	}
	if issue := manager.ensureCommandCapacityLocked(); issue != nil {
		manager.mu.Unlock()
		return Envelope{}, issue
	}
	manager.mu.Unlock()
	result := manager.backend.Probe(ctx)
	manager.mu.Lock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	defer manager.mu.Unlock()
	manager.state.Commands[request.CommandID] = commandRecord{Kind: "check"}
	manager.state.Revision++
	checked := manager.now()
	manager.state.CheckedAt = &checked
	switch result {
	case ProbeAuthenticated:
		manager.state.State, manager.state.ReasonCode = StateAuthenticated, nil
	case ProbeUnauthenticated:
		manager.state.State, manager.state.ReasonCode = StateUnauthenticated, nil
	case ProbeInvalid:
		reason := "credential_rejected"
		manager.state.State, manager.state.ReasonCode = StateReauthenticationRequired, &reason
	case ProbeUnavailable:
		reason := "provider_unavailable"
		manager.state.State, manager.state.ReasonCode = StateUnknown, &reason
	default:
		reason := "provider_unavailable"
		manager.state.State, manager.state.ReasonCode = StateUnknown, &reason
	}
	if manager.persistLocked() != nil {
		manager.failClosedLocked()
		_ = manager.persistLocked()
		return Envelope{}, ErrProviderUnavailable
	}
	return manager.envelopeLocked(""), nil
}

// Refresh verifies persisted credentials after restart. File presence alone
// never makes the node ready.
func (manager *Manager) Refresh(ctx context.Context) error {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	manager.mu.Lock()
	if err := manager.expirePendingLocked(manager.clock()); err != nil {
		manager.mu.Unlock()
		return err
	}
	for _, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			manager.mu.Unlock()
			return nil
		}
	}
	manager.mu.Unlock()
	result := manager.backend.Probe(ctx)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := manager.expirePendingLocked(manager.clock()); err != nil {
		return err
	}
	manager.state.Revision++
	checked := manager.now()
	manager.state.CheckedAt = &checked
	switch result {
	case ProbeAuthenticated:
		manager.state.State, manager.state.ReasonCode = StateAuthenticated, nil
	case ProbeUnauthenticated:
		manager.state.State, manager.state.ReasonCode = StateUnauthenticated, nil
	case ProbeInvalid:
		reason := "credential_rejected"
		manager.state.State, manager.state.ReasonCode = StateReauthenticationRequired, &reason
	case ProbeUnavailable:
		reason := "provider_unavailable"
		manager.state.State, manager.state.ReasonCode = StateUnknown, &reason
	default:
		reason := "provider_unavailable"
		manager.state.State, manager.state.ReasonCode = StateUnknown, &reason
	}
	if err := manager.persistLocked(); err != nil {
		manager.failClosedLocked()
		_ = manager.persistLocked()
		return err
	}
	return nil
}

func (manager *Manager) Cancel(_ context.Context, operationID string, request CommandRequest) (Envelope, *APIError) {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	if issue := manager.validateCommand(request, "cancel:"+operationID); issue != nil {
		return Envelope{}, issue
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		return Envelope{}, ErrProviderUnavailable
	}
	operation, ok := manager.state.Operations[operationID]
	if !ok {
		return Envelope{}, ErrNotFound
	}
	if prior, ok := manager.state.Commands[request.CommandID]; ok {
		if prior.Kind != "cancel:"+operationID {
			return Envelope{}, ErrIDConflict
		}
		return manager.envelopeLocked(operationID), nil
	}
	if issue := manager.ensureCommandCapacityLocked(); issue != nil {
		return Envelope{}, issue
	}
	manager.state.Commands[request.CommandID] = commandRecord{Kind: "cancel:" + operationID, OperationID: operationID}
	if operation.Status == OperationPending {
		if cancel := manager.cancels[operationID]; cancel != nil {
			cancel()
		}
		reason := "cancelled"
		operation.Status, operation.ReasonCode, operation.UpdatedAt = OperationCancelled, &reason, manager.now()
		manager.state.Operations[operationID] = operation
		delete(manager.cancels, operationID)
	}
	manager.state.Revision++
	if manager.persistLocked() != nil {
		manager.failClosedLocked()
		_ = manager.persistLocked()
		return Envelope{}, ErrProviderUnavailable
	}
	return manager.envelopeLocked(operationID), nil
}

func (manager *Manager) Logout(ctx context.Context, request CommandRequest) (Envelope, *APIError) {
	manager.actionMu.Lock()
	defer manager.actionMu.Unlock()
	if issue := manager.validateCommand(request, "logout"); issue != nil {
		return Envelope{}, issue
	}
	release, busy := manager.beginTransition()
	defer release()
	manager.mu.Lock()
	if manager.expirePendingLocked(manager.clock()) != nil {
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	if prior, ok := manager.state.Commands[request.CommandID]; ok {
		defer manager.mu.Unlock()
		if prior.Kind != "logout" {
			return Envelope{}, ErrIDConflict
		}
		return manager.envelopeLocked(""), nil
	}
	if busy {
		manager.mu.Unlock()
		return Envelope{}, ErrBusy
	}
	for _, operation := range manager.state.Operations {
		if operation.Status == OperationPending {
			manager.mu.Unlock()
			return Envelope{}, ErrPendingOperation
		}
	}
	if issue := manager.ensureCommandCapacityLocked(); issue != nil {
		manager.mu.Unlock()
		return Envelope{}, issue
	}
	manager.transitioning = true
	manager.mu.Unlock()
	if err := manager.backend.Logout(ctx); err != nil {
		manager.mu.Lock()
		manager.transitioning = false
		manager.failClosedLocked()
		_ = manager.persistLocked()
		manager.mu.Unlock()
		return Envelope{}, ErrProviderUnavailable
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.transitioning = false
	manager.state.Commands[request.CommandID] = commandRecord{Kind: "logout"}
	manager.state.State, manager.state.ReasonCode = StateUnauthenticated, nil
	checked := manager.now()
	manager.state.CheckedAt = &checked
	manager.state.Revision++
	if manager.persistLocked() != nil {
		manager.failClosedLocked()
		_ = manager.persistLocked()
		return Envelope{}, ErrProviderUnavailable
	}
	return manager.envelopeLocked(""), nil
}

func (manager *Manager) validateCommand(request CommandRequest, kind string) *APIError {
	if request.NodeID != manager.nodeID {
		return ErrNotFound
	}
	if !uuidPattern.MatchString(request.CommandID) {
		return ErrInvalidRequest
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if prior, ok := manager.state.Commands[request.CommandID]; ok && prior.Kind != kind {
		return ErrIDConflict
	}
	return nil
}

func (manager *Manager) envelopeLocked(operationID string) Envelope {
	if operationID == "" {
		operationID = manager.state.LatestID
	}
	var operation *Operation
	if value, ok := manager.state.Operations[operationID]; ok {
		copy := value
		operation = &copy
	}
	return Envelope{SchemaID: SchemaID, NodeID: manager.nodeID, Revision: manager.state.Revision, State: manager.state.State, CheckedAt: manager.state.CheckedAt, ReasonCode: manager.state.ReasonCode, Capabilities: Capabilities{Methods: []string{"secret"}, CanCheck: true, CanLogout: true}, Operation: operation}
}

func (manager *Manager) expirePendingLocked(now time.Time) error {
	changed := false
	for id, operation := range manager.state.Operations {
		if operation.Status != OperationPending || operation.TimeoutAt == nil {
			continue
		}
		deadline, err := time.Parse(time.RFC3339Nano, *operation.TimeoutAt)
		if err == nil && now.Before(deadline) {
			continue
		}
		if cancel := manager.cancels[id]; cancel != nil {
			cancel()
		}
		delete(manager.cancels, id)
		reason := "expired"
		operation.Status, operation.ReasonCode = OperationExpired, &reason
		operation.UpdatedAt = manager.timestamp(now)
		manager.state.Operations[id] = operation
		changed = true
	}
	if !changed {
		return nil
	}
	manager.state.Revision++
	if err := manager.persistLocked(); err != nil {
		manager.failClosedLocked()
		_ = manager.persistLocked()
		return err
	}
	return nil
}

func (manager *Manager) persistLocked() error {
	raw, err := json.Marshal(manager.state)
	if err != nil {
		return err
	}
	return manager.write(manager.path, append(raw, '\n'))
}

func (manager *Manager) failClosedLocked() {
	reason := "provider_unavailable"
	manager.state.State = StateUnknown
	manager.state.ReasonCode = &reason
	manager.state.Revision++
}

// Receipts are never evicted: forgetting a command ID would permit a repeated
// request to perform a second login/logout. New commands fail closed at the
// durable bound while every known command remains replayable.
func (manager *Manager) ensureCommandCapacityLocked() *APIError {
	if len(manager.state.Commands) >= maxCommandReceipts {
		return ErrBusy
	}
	return nil
}

func (manager *Manager) now() string { return manager.timestamp(manager.clock()) }
func (manager *Manager) timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

type secretContextKey struct{}

func withSecret(ctx context.Context, secret string) context.Context {
	return context.WithValue(ctx, secretContextKey{}, secret)
}
func SecretFromContext(ctx context.Context) string {
	value, _ := ctx.Value(secretContextKey{}).(string)
	return value
}

func containsControl(value string) bool {
	for _, r := range value {
		if r == 0 || r == '\r' || r == '\n' {
			return true
		}
	}
	return false
}

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	value := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", value[:8], value[8:12], value[12:16], value[16:20], value[20:]), nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("provider auth directory is invalid")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func atomicPrivateWrite(path string, content []byte) error {
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".provider-auth-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	ok := false
	defer func() {
		temporary.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	ok = true
	return nil
}
