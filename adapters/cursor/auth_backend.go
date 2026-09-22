package cursor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/boxvtk621/harness-cursor/providerauth"
)

type AuthBackend struct {
	managed         *Managed
	credentialFile  string
	seedFile        string
	nodeExecutable  string
	probeEntrypoint string

	mu   sync.RWMutex
	busy func() bool
}

type authReplacement struct {
	backend        *AuthBackend
	adapter        *Adapter
	prior          *Adapter
	secret         string
	previousSecret string
	hadPrevious    bool
	mu             sync.Mutex
	committed      bool
	finalized      bool
	rolledBack     bool
	closed         bool
}

func NewAuthBackend(managed *Managed, credentialFile, seedFile, nodeExecutable, probeEntrypoint string) (*AuthBackend, error) {
	if managed == nil || !filepath.IsAbs(credentialFile) || nodeExecutable == "" || !filepath.IsAbs(probeEntrypoint) {
		return nil, errors.New("cursor auth backend config is incomplete")
	}
	return &AuthBackend{managed: managed, credentialFile: credentialFile, seedFile: seedFile, nodeExecutable: nodeExecutable, probeEntrypoint: probeEntrypoint}, nil
}

func (backend *AuthBackend) SetBusy(probe func() bool) {
	backend.mu.Lock()
	backend.busy = probe
	backend.mu.Unlock()
}
func (backend *AuthBackend) Busy() bool {
	backend.mu.RLock()
	probe := backend.busy
	backend.mu.RUnlock()
	return probe != nil && probe()
}

func (backend *AuthBackend) Bootstrap(ctx context.Context, firstRun bool) (bool, error) {
	if err := backend.recoverInterruptedReplacement(); err != nil {
		return false, err
	}
	secret, found, err := readPrivateSecret(backend.credentialFile)
	if err != nil {
		return false, err
	}
	if !found && firstRun && backend.seedFile != "" {
		secret, found, err = readPrivateSecret(backend.seedFile)
		if err != nil {
			return false, err
		}
		if found && writePrivateSecret(backend.credentialFile, secret) != nil {
			return false, errors.New("persist cursor seed credential")
		}
	}
	if !found {
		return false, nil
	}
	replacement, err := backend.managed.Prepare(secret)
	if err != nil {
		return false, err
	}
	if err := backend.managed.Swap(replacement); err != nil {
		return false, err
	}
	return true, nil
}

func (backend *AuthBackend) Probe(ctx context.Context) providerauth.ProbeResult {
	secret := providerauth.SecretFromContext(ctx)
	if secret == "" {
		var found bool
		var err error
		secret, found, err = readPrivateSecret(backend.credentialFile)
		if err != nil {
			return providerauth.ProbeUnavailable
		}
		if !found {
			return providerauth.ProbeUnauthenticated
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, backend.nodeExecutable, backend.probeEntrypoint)
	command.Env = append(os.Environ(), "CURSOR_API_KEY="+secret)
	command.Stderr = io.Discard
	output, err := command.Output()
	if err == nil && bytes.Equal(bytes.TrimSpace(output), []byte(`{"status":"authenticated"}`)) {
		return providerauth.ProbeAuthenticated
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return providerauth.ProbeInvalid
	}
	return providerauth.ProbeUnavailable
}

func (backend *AuthBackend) Replace(ctx context.Context, secret string) error {
	replacement, err := backend.PrepareReplacement(ctx, secret)
	if err != nil {
		return err
	}
	defer replacement.Close()
	if err := replacement.Commit(ctx); err != nil {
		_ = replacement.Rollback()
		return err
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := replacement.Rollback(); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	return replacement.Finalize()
}

func (backend *AuthBackend) PrepareReplacement(ctx context.Context, secret string) (providerauth.Replacement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	replacement, err := backend.managed.Prepare(secret)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = replacement.Close()
		return nil, err
	}
	return &authReplacement{backend: backend, adapter: replacement, secret: secret}, nil
}

func (replacement *authReplacement) Commit(ctx context.Context) error {
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	if replacement.closed || replacement.committed || replacement.finalized || replacement.rolledBack || replacement.adapter == nil {
		return errors.New("cursor replacement is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous, hadPrevious, err := readPrivateSecret(replacement.backend.credentialFile)
	if err != nil {
		return err
	}
	replacement.previousSecret, replacement.hadPrevious = previous, hadPrevious
	if err := replacement.backend.writeRollbackMarker(previous, hadPrevious); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = replacement.backend.removeRollbackMarker()
		return err
	}
	replacement.prior, err = replacement.backend.managed.Exchange(replacement.adapter)
	if err != nil {
		_ = replacement.backend.removeRollbackMarker()
		return err
	}
	replacement.committed = true
	if err := ctx.Err(); err != nil {
		return errors.Join(err, replacement.rollbackLocked())
	}
	if err := writePrivateSecret(replacement.backend.credentialFile, replacement.secret); err != nil {
		return errors.Join(err, replacement.rollbackLocked())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, replacement.rollbackLocked())
	}
	return nil
}

func (replacement *authReplacement) Rollback() error {
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	return replacement.rollbackLocked()
}

func (replacement *authReplacement) rollbackLocked() error {
	if replacement.finalized || replacement.rolledBack {
		return nil
	}
	var rollbackErr error
	if replacement.committed {
		if err := replacement.backend.managed.Restore(replacement.adapter, replacement.prior); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		} else if replacement.adapter != nil {
			// Adapter.Close reports the expected worker kill status; the
			// generation has already been detached under Managed's lock.
			_ = replacement.adapter.Close()
			replacement.adapter = nil
		}
		rollbackErr = errors.Join(rollbackErr, restorePrivateSecret(replacement.backend.credentialFile, replacement.previousSecret, replacement.hadPrevious))
	} else if replacement.adapter != nil {
		_ = replacement.adapter.Close()
		replacement.adapter = nil
	}
	if rollbackErr != nil {
		return rollbackErr
	}
	if err := replacement.backend.removeRollbackMarker(); err != nil {
		return err
	}
	replacement.rolledBack = true
	return nil
}

func (replacement *authReplacement) Finalize() error {
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	if replacement.closed || !replacement.committed || replacement.rolledBack {
		return errors.New("cursor replacement is unavailable")
	}
	if replacement.finalized {
		return nil
	}
	if err := replacement.backend.removeRollbackMarker(); err != nil {
		return err
	}
	replacement.finalized = true
	replacement.adapter = nil // The managed adapter now owns the candidate.
	if replacement.prior != nil {
		_ = replacement.prior.Close()
		replacement.prior = nil
	}
	return nil
}

func (replacement *authReplacement) Close() error {
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	if replacement.closed {
		return nil
	}
	replacement.closed = true
	if replacement.committed && !replacement.finalized && !replacement.rolledBack {
		return replacement.rollbackLocked()
	}
	if replacement.adapter == nil {
		return nil
	}
	_ = replacement.adapter.Close()
	replacement.adapter = nil
	return nil
}

func (backend *AuthBackend) rollbackMarkerPath() string {
	return backend.credentialFile + ".rollback"
}

func (backend *AuthBackend) writeRollbackMarker(secret string, existed bool) error {
	marker := "0"
	if existed {
		marker = "1" + secret
	}
	return writePrivateSecret(backend.rollbackMarkerPath(), marker)
}

func (backend *AuthBackend) removeRollbackMarker() error {
	path := backend.rollbackMarkerPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (backend *AuthBackend) recoverInterruptedReplacement() error {
	marker, found, err := readPrivateSecret(backend.rollbackMarkerPath())
	if err != nil || !found {
		return err
	}
	switch {
	case marker == "0":
		err = restorePrivateSecret(backend.credentialFile, "", false)
	case strings.HasPrefix(marker, "1") && len(marker) > 1:
		err = restorePrivateSecret(backend.credentialFile, marker[1:], true)
	default:
		err = errors.New("cursor rollback marker is invalid")
	}
	if err != nil {
		return err
	}
	return backend.removeRollbackMarker()
}

func (backend *AuthBackend) Logout(context.Context) error {
	if err := os.Remove(backend.credentialFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(backend.credentialFile))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return backend.managed.Deactivate()
}

func readPrivateSecret(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", false, errors.New("cursor credential file is not private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if secret == "" || len(secret) > 16*1024 || strings.ContainsAny(secret, "\r\n\x00") {
		return "", false, errors.New("cursor credential is invalid")
	}
	return secret, true, nil
}

func writePrivateSecret(path, secret string) error {
	if secret == "" || len(secret) > 16*1024 || strings.ContainsAny(secret, "\r\n\x00") {
		return errors.New("cursor credential is invalid")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".cursor-key-*")
	if err != nil {
		return err
	}
	name, ok := temporary.Name(), false
	defer func() {
		temporary.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.WriteString(secret); err != nil {
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

func restorePrivateSecret(path, secret string, existed bool) error {
	if existed {
		return writePrivateSecret(path, secret)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
