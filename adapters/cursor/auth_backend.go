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

func (backend *AuthBackend) Replace(_ context.Context, secret string) error {
	replacement, err := backend.managed.Prepare(secret)
	if err != nil {
		return err
	}
	if err := writePrivateSecret(backend.credentialFile, secret); err != nil {
		replacement.Close()
		return err
	}
	return backend.managed.Swap(replacement)
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
