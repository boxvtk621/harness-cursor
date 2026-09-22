package cursor

import (
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
