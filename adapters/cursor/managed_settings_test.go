package cursor

import (
	"context"
	"testing"

	"github.com/boxvtk621/harness-cursor/nodesettings"
)

func TestManagedSettingsRestartAndRollback(t *testing.T) {
	managed, err := NewManaged(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	managed.ConfigureApplied(nodesettings.RuntimeConfig{ModelID: "first-model"})
	prior, err := managed.Prepare("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := managed.Swap(prior); err != nil {
		t.Fatal(err)
	}
	settings := nodesettings.RuntimeConfig{ModelID: "next-model", Params: map[string]string{"fast": "true"}, MCPServers: []nodesettings.RuntimeMCPServer{{ID: "docs", URL: "https://example.test/mcp", BearerToken: "bearer"}}}
	if rollbackFailed, err := managed.RestartSettings(context.Background(), settings); err != nil || rollbackFailed {
		t.Fatalf("restart: rollback=%v error=%v", rollbackFailed, err)
	}
	if !prior.closed || managed.current == prior || managed.current.config.Model != "next-model" || managed.current.config.ModelParams[0].Value != "true" || managed.current.config.MCPServers["docs"].Headers["Authorization"] != "Bearer bearer" {
		t.Fatal("replacement did not retain native settings")
	}
	previous := managed.current
	bad := nodesettings.RuntimeConfig{ModelID: "reject-model"}
	if rollbackFailed, err := managed.RestartSettings(context.Background(), bad); err == nil || rollbackFailed {
		t.Fatalf("rollback: rollback=%v error=%v", rollbackFailed, err)
	}
	if !previous.closed || managed.current == nil || managed.current.config.Model != "next-model" || managed.current.config.MCPServers["docs"].URL == "" {
		t.Fatal("previous configuration was not restored")
	}
}

func TestManagedBootstrapUsesAppliedSnapshotAfterDraftEdit(t *testing.T) {
	const nodeID = "10000000-0000-4000-8000-000000000001"
	config := fakeConfig(t)
	settings, err := nodesettings.Open(nodeID, config.StateDir, "1.0.31", "first-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := nodesettings.Snapshot{Inference: nodesettings.Inference{ModelID: func() *string { value := "draft-only-model"; return &value }()}, MCPServers: []nodesettings.MCPServer{}}
	if _, issue := settings.PutDraft(context.Background(), nodeID, nodesettings.PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatal(issue)
	}
	reopened, err := nodesettings.Open(nodeID, config.StateDir, "1.0.31", "ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := NewManaged(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()
	managed.ConfigureApplied(reopened.AppliedRuntimeConfig())
	adapter, err := managed.Prepare("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := managed.Swap(adapter); err != nil {
		t.Fatal(err)
	}
	if managed.current.config.Model != "first-model" {
		t.Fatalf("draft was activated on bootstrap: %s", managed.current.config.Model)
	}
}
