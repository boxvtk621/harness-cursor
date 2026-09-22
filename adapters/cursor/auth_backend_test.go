package cursor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateSecretWriteIsAtomicAndOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials", "cursor.key")
	if err := writePrivateSecret(path, "secret-value"); err != nil {
		t.Fatal(err)
	}
	value, found, err := readPrivateSecret(path)
	if err != nil || !found || value != "secret-value" {
		t.Fatalf("read=%q found=%v err=%v", value, found, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%04o", info.Mode().Perm())
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("credential directory mode=%04o", directory.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cursor.key" {
		t.Fatalf("temporary credential remained: %v", entries)
	}
}

func TestPrivateSecretRejectsBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.key")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPrivateSecret(path); err == nil {
		t.Fatal("broad credential permissions accepted")
	}
}

func TestRestorePrivateSecretPreservesPriorCredentialOrAbsence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "credentials", "cursor.key")
	if err := writePrivateSecret(path, "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := restorePrivateSecret(path, "existing", true); err != nil {
		t.Fatal(err)
	}
	value, found, err := readPrivateSecret(path)
	if err != nil || !found || value != "existing" {
		t.Fatalf("restored=%q found=%v err=%v", value, found, err)
	}
	if err := restorePrivateSecret(path, "", false); err != nil {
		t.Fatal(err)
	}
	if _, found, err := readPrivateSecret(path); err != nil || found {
		t.Fatalf("absent credential was not restored: found=%v err=%v", found, err)
	}
}

func TestBootstrapRecoveryRestoresCredentialFromInterruptedReplacement(t *testing.T) {
	for name, previous := range map[string]*string{
		"existing credential": func() *string { value := "existing"; return &value }(),
		"previously absent":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "cursor.key")
			backend := &AuthBackend{credentialFile: path}
			if err := writePrivateSecret(path, "candidate"); err != nil {
				t.Fatal(err)
			}
			if previous == nil {
				if err := backend.writeRollbackMarker("", false); err != nil {
					t.Fatal(err)
				}
			} else if err := backend.writeRollbackMarker(*previous, true); err != nil {
				t.Fatal(err)
			}
			if err := backend.recoverInterruptedReplacement(); err != nil {
				t.Fatal(err)
			}
			value, found, err := readPrivateSecret(path)
			if err != nil || found != (previous != nil) || previous != nil && value != *previous {
				t.Fatalf("recovered=%q found=%v err=%v", value, found, err)
			}
			if _, found, err := readPrivateSecret(backend.rollbackMarkerPath()); err != nil || found {
				t.Fatalf("rollback marker remained: found=%v err=%v", found, err)
			}
		})
	}
}

func TestInterruptedReplacementRejectsInvalidRollbackMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.key")
	backend := &AuthBackend{credentialFile: path}
	if err := writePrivateSecret(path, "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateSecret(backend.rollbackMarkerPath(), "invalid"); err != nil {
		t.Fatal(err)
	}
	if err := backend.recoverInterruptedReplacement(); err == nil {
		t.Fatal("invalid rollback marker was accepted")
	}
	value, found, err := readPrivateSecret(path)
	if err != nil || !found || value != "candidate" {
		t.Fatalf("invalid recovery mutated credential: value=%q found=%v err=%v", value, found, err)
	}
}

func TestAuthReplacementTransactionCommitsOrRestoresRealManagedAdapter(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "finalize"
		if rollback {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			config := fakeConfig(t)
			managed, err := NewManaged(config, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer managed.Close()
			credentialFile := filepath.Join(t.TempDir(), "credentials", "cursor.key")
			backend, err := NewAuthBackend(managed, credentialFile, "", config.NodeExecutable, config.WorkerEntrypoint)
			if err != nil {
				t.Fatal(err)
			}
			prior, err := managed.Prepare("existing")
			if err != nil {
				t.Fatal(err)
			}
			if err := managed.Swap(prior); err != nil {
				t.Fatal(err)
			}
			if err := writePrivateSecret(credentialFile, "existing"); err != nil {
				t.Fatal(err)
			}

			replacement, err := backend.PrepareReplacement(context.Background(), "candidate")
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			if err := replacement.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			candidate, found, err := readPrivateSecret(credentialFile)
			if err != nil || !found || candidate != "candidate" || managed.current == prior {
				t.Fatalf("candidate was not provisionally installed: credential=%q found=%v err=%v", candidate, found, err)
			}
			if _, found, err := readPrivateSecret(backend.rollbackMarkerPath()); err != nil || !found {
				t.Fatalf("rollback marker missing during provisional commit: found=%v err=%v", found, err)
			}

			if rollback {
				if err := replacement.Rollback(); err != nil {
					t.Fatal(err)
				}
				value, found, err := readPrivateSecret(credentialFile)
				if err != nil || !found || value != "existing" || managed.current != prior {
					t.Fatalf("rollback did not restore prior generation: credential=%q found=%v err=%v", value, found, err)
				}
			} else {
				if err := replacement.Finalize(); err != nil {
					t.Fatal(err)
				}
				value, found, err := readPrivateSecret(credentialFile)
				if err != nil || !found || value != "candidate" || managed.current == prior {
					t.Fatalf("finalize did not retain candidate generation: credential=%q found=%v err=%v", value, found, err)
				}
			}
			if _, found, err := readPrivateSecret(backend.rollbackMarkerPath()); err != nil || found {
				t.Fatalf("rollback marker remained after terminal transaction: found=%v err=%v", found, err)
			}
		})
	}
}
