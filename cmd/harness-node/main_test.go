package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harnessadapter "github.com/boxvtk621/harness-cursor/adapters/contract"
)

func TestLoadConfigAcceptsExactCursorShape(t *testing.T) {
	path := writeConfig(t, `{"adapter":"cursor","cursor":{"workingDir":"/workspace"}}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cursor == nil || cfg.Cursor.WorkingDir != "/workspace" || selectedAdapter(cfg) != "cursor" {
		t.Fatalf("cursor config = %#v", cfg)
	}
}

func TestLoadConfigRejectsCodexAndUnknownProviderShapes(t *testing.T) {
	for _, test := range []struct {
		name, body string
	}{
		{name: "codex block", body: `{"adapter":"codex","codex":{"workingDir":"/workspace"}}`},
		{name: "unknown block", body: `{"adapter":"other","other":{}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, test.body)); err == nil {
				t.Fatal("non-Cursor provider configuration was accepted")
			}
		})
	}
}

func TestSelectedAdapterRequiresExplicitCursorSelector(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config
		want string
	}{
		{name: "explicit cursor", cfg: config{Adapter: "cursor", Cursor: &cursorConfig{}}, want: "cursor"},
		{name: "missing selector", cfg: config{Cursor: &cursorConfig{}}, want: ""},
		{name: "unknown selector", cfg: config{Adapter: "other", Cursor: &cursorConfig{}}, want: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectedAdapter(test.cfg); got != test.want {
				t.Fatalf("selected adapter = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProviderValidationFailsClosedForMissingOrUnknownSelector(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config
	}{
		{name: "missing cursor", cfg: config{Adapter: "cursor"}},
		{name: "missing selector", cfg: config{Cursor: &cursorConfig{}}},
		{name: "unknown selector", cfg: config{Adapter: "codex", Cursor: &cursorConfig{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateProviderConfig(test.cfg); err == nil {
				t.Fatal("unsafe provider configuration was accepted")
			}
		})
	}
	if err := validateProviderConfig(config{Adapter: "cursor", Cursor: &cursorConfig{}}); err != nil {
		t.Fatalf("exact Cursor configuration rejected: %v", err)
	}
}

func TestExplicitToolWorkspaceIsPinned(t *testing.T) {
	for _, test := range []struct {
		name, workingDir string
		wantErr          bool
	}{
		{name: "workspace", workingDir: "/workspace"},
		{name: "missing", wantErr: true},
		{name: "different absolute path", workingDir: "/tmp/workspace", wantErr: true},
		{name: "relative path", workingDir: "workspace", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateExplicitToolWorkspace("Cursor", test.workingDir)
			if (err != nil) != test.wantErr {
				t.Fatalf("validate explicit tool workspace = %v", err)
			}
		})
	}
}

func TestFilePolicyPreservesLegacyDeny(t *testing.T) {
	for _, test := range []struct {
		name, manifest string
	}{
		{name: "compact", manifest: "[]\n"},
		{name: "formatted", manifest: "[  ]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := testFilePolicy(t, "", "", test.manifest).Current(context.Background(), "node")
			if err != nil || policy.ApprovalMode != harnessadapter.ApprovalModeDeny {
				t.Fatalf("policy = %#v, %v", policy, err)
			}
		})
	}
}

func TestFilePolicyAcceptsOnlyExactCursorExplicitOnceManifest(t *testing.T) {
	policy, err := testFilePolicy(t, "cursor", "explicit_once", cursorExplicitToolManifest).Current(context.Background(), "node")
	if err != nil || policy.ApprovalMode != harnessadapter.ApprovalModeExplicitOnce || string(policy.ToolManifest) != cursorExplicitToolManifest {
		t.Fatalf("policy = %#v, %v", policy, err)
	}
}

func TestFilePolicyFailsClosedForMismatchedModeOrManifest(t *testing.T) {
	for _, test := range []struct{ name, adapter, mode, manifest string }{
		{name: "explicit missing lf", adapter: "cursor", mode: "explicit_once", manifest: strings.TrimSuffix(cursorExplicitToolManifest, "\n")},
		{name: "wrong adapter", adapter: "codex", mode: "explicit_once", manifest: cursorExplicitToolManifest},
		{name: "wrong order", adapter: "cursor", mode: "explicit_once", manifest: `[{"name":"cursor.file_change"},{"name":"cursor.command"}]` + "\n"},
		{name: "unknown tool", adapter: "cursor", mode: "explicit_once", manifest: `[{"name":"unsafe"}]`},
		{name: "deny nonempty", adapter: "cursor", mode: "deny", manifest: cursorExplicitToolManifest},
		{name: "invalid mode", adapter: "cursor", mode: "allow", manifest: "[]\n"},
		{name: "invalid json", adapter: "cursor", mode: "deny", manifest: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := testFilePolicy(t, test.adapter, test.mode, test.manifest).Current(context.Background(), "node"); err == nil {
				t.Fatal("unsafe manifest was accepted")
			}
		})
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testFilePolicy(t *testing.T, adapter, mode, manifest string) filePolicy {
	t.Helper()
	root := t.TempDir()
	contentPath, manifestPath := filepath.Join(root, "policy.txt"), filepath.Join(root, "tools.json")
	if err := os.WriteFile(contentPath, []byte("policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return filePolicy{
		contentPath: contentPath, manifestPath: manifestPath, revision: "test@1",
		approvalMode: mode, adapter: adapter,
	}
}
