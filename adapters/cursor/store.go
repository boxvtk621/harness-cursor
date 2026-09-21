package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
)

const mappingSchemaVersion = 1

type persistedAttempt struct {
	Reference  harnessadapter.AttemptRef      `json:"reference"`
	Context    harnessadapter.ContextBoundary `json:"context"`
	PolicyHash string                         `json:"policyHash"`
	AgentID    string                         `json:"agentId,omitempty"`
	RunID      string                         `json:"runId,omitempty"`
	State      string                         `json:"state"`
}

type persistedDialog struct {
	AgentID  string                         `json:"agentId"`
	Boundary harnessadapter.ContextBoundary `json:"boundary"`
}

type mappingState struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Attempts      map[string]persistedAttempt `json:"attempts"`
	Dialogs       map[string]persistedDialog  `json:"dialogs"`
}

type mappingStore struct {
	mu       sync.Mutex
	dir      string
	path     string
	contents mappingState
}

func openMappingStore(dir string) (*mappingStore, error) {
	if dir == "" {
		return nil, errors.New("cursor state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cursor state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("cursor state directory is unsafe")
	}
	store := &mappingStore{
		dir:  dir,
		path: filepath.Join(dir, "native-mapping.json"),
		contents: mappingState{SchemaVersion: mappingSchemaVersion,
			Attempts: make(map[string]persistedAttempt), Dialogs: make(map[string]persistedDialog)},
	}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cursor native mapping: %w", err)
	}
	info, err = os.Lstat(store.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("cursor native mapping file is unsafe")
	}
	if err := json.Unmarshal(data, &store.contents); err != nil || store.contents.SchemaVersion != mappingSchemaVersion || store.contents.Attempts == nil || store.contents.Dialogs == nil {
		return nil, errors.New("cursor native mapping is invalid")
	}
	return store, nil
}

func attemptKey(reference harnessadapter.AttemptRef) string {
	return reference.NodeID + "/" + reference.DialogID + "/" + reference.RequestID + "/" + reference.AttemptID + fmt.Sprintf("/%d", reference.Generation)
}

func (store *mappingStore) attempt(reference harnessadapter.AttemptRef) (persistedAttempt, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.contents.Attempts[attemptKey(reference)]
	return value, ok
}

func (store *mappingStore) dialog(dialogID string) (persistedDialog, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.contents.Dialogs[dialogID]
	return value, ok
}

func (store *mappingStore) putIntent(reference harnessadapter.AttemptRef, boundary harnessadapter.ContextBoundary, policyHash string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.contents.Attempts[attemptKey(reference)] = persistedAttempt{
		Reference: reference, Context: boundary, PolicyHash: policyHash, State: "dispatching",
	}
	return store.writeLocked()
}

func (store *mappingStore) activate(reference harnessadapter.AttemptRef, boundary harnessadapter.ContextBoundary, policyHash, agentID, runID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.contents.Attempts[attemptKey(reference)] = persistedAttempt{
		Reference: reference, Context: boundary, PolicyHash: policyHash,
		AgentID: agentID, RunID: runID, State: "active",
	}
	store.contents.Dialogs[reference.DialogID] = persistedDialog{AgentID: agentID, Boundary: boundary}
	return store.writeLocked()
}

func (store *mappingStore) terminal(reference harnessadapter.AttemptRef) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.contents.Attempts[attemptKey(reference)]
	if !ok {
		return nil
	}
	value.State = "terminal"
	store.contents.Attempts[attemptKey(reference)] = value
	return store.writeLocked()
}

func (store *mappingStore) writeLocked() error {
	data, err := json.Marshal(store.contents)
	if err != nil {
		return err
	}
	temporary := store.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if removeErr := os.Remove(temporary); removeErr != nil {
			return removeErr
		}
		file, err = os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, store.path); err != nil {
		return err
	}
	directory, err := os.Open(store.dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	keep = true
	return nil
}
