package nodesettings

import (
	"context"
	"os"
	"strings"
	"testing"
)

const testNodeID = "10000000-0000-4000-8000-000000000001"

func textPointer(value string) *string { return &value }

type catalogStub struct{}

func (catalogStub) Models(context.Context) ([]NativeModel, string, error) {
	return []NativeModel{{ID: "actual-model", DisplayName: "Actual", Parameters: []NativeParameter{{ID: "fast", Values: []string{"false", "true"}}, {ID: "reasoning_effort", Values: []string{"low", "high"}}, {ID: "vendor_private", Values: []string{"x"}}}, Variants: []NativeVariant{{Params: map[string]string{"fast": "false", "reasoning_effort": "high"}, IsDefault: true}}}}, "sdk-revision", nil
}

func TestDraftCASMasksAndPersistsBearerSecret(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "initial", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	speed := "true"
	draft := Snapshot{Inference: Inference{ModelID: textPointer("actual-model"), SpeedMode: &speed}, MCPServers: []MCPServer{{
		ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 5000,
		Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "top-secret"},
	}}}
	envelope, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft})
	if issue != nil {
		t.Fatalf("put issue=%+v", issue)
	}
	if envelope.DraftRevision != 2 || envelope.AppliedRevision != 1 || envelope.Draft.MCPServers[0].Auth.Secret != "" || !envelope.Draft.MCPServers[0].Auth.BearerTokenConfigured {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue == nil || issue.Code != "revision_conflict" {
		t.Fatalf("stale CAS issue=%+v", issue)
	}
	raw, err := os.ReadFile(manager.path)
	if err != nil || !strings.Contains(string(raw), "top-secret") {
		t.Fatalf("private secret was not durable: %v", err)
	}
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, issue := reopened.Snapshot(context.Background(), testNodeID)
	if issue != nil || loaded.DraftRevision != 2 || !loaded.Draft.MCPServers[0].Auth.BearerTokenConfigured || loaded.Draft.MCPServers[0].Auth.Secret != "" {
		t.Fatalf("reopened=%+v issue=%+v", loaded, issue)
	}
}

func TestCatalogMapsOnlyExplicitSemanticParameters(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "initial", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, issue := manager.ModelCatalog(context.Background(), testNodeID)
	if issue != nil || catalog.State != "fresh" || catalog.CatalogRevision != "sdk-revision" || len(catalog.Models) != 1 {
		t.Fatalf("catalog=%+v issue=%+v", catalog, issue)
	}
	model := catalog.Models[0]
	if len(model.SpeedModes) != 0 || len(model.ReasoningEfforts) != 0 {
		t.Fatalf("model mapping=%+v", model)
	}
}

func TestApplyFailsClosedWithoutChangingAppliedRevision(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 1, TargetRevision: 1})
	if issue != nil || envelope.Operation == nil || envelope.Operation.Status != "failed" || envelope.Operation.ReasonCode != "managed_restart_coordination_unavailable" || envelope.AppliedRevision != 1 {
		t.Fatalf("envelope=%+v issue=%+v", envelope, issue)
	}
	replay, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 1, TargetRevision: 1})
	if issue != nil || replay.Operation.OperationID != envelope.Operation.OperationID {
		t.Fatalf("replay=%+v issue=%+v", replay, issue)
	}
	if !uuidPattern.MatchString(envelope.Operation.OperationID) {
		t.Fatalf("operation ID is not UUID-D: %q", envelope.Operation.OperationID)
	}
	if _, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 1, TargetRevision: 2}); issue == nil || issue.Code != "command_conflict" {
		t.Fatalf("mismatched replay issue=%+v", issue)
	}
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, issue = reopened.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 1, TargetRevision: 1})
	if issue != nil || replay.Operation.OperationID != envelope.Operation.OperationID {
		t.Fatalf("durable replay=%+v issue=%+v", replay, issue)
	}
}

func TestLoadedStateRejectsUnknownFieldsAndInvalidOperations(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"unknown":       func(raw string) string { return strings.TrimSuffix(raw, "}") + `,"unknown":true}` },
		"invalid model": func(raw string) string { return strings.Replace(raw, `"modelId":"initial"`, `"modelId":""`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			manager, err := Open(testNodeID, directory, "1.0.31", "initial", nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(manager.path)
			if err != nil {
				t.Fatal(err)
			}
			changed := mutate(string(raw))
			if changed == string(raw) {
				t.Fatal("mutation did not change state")
			}
			if err := os.WriteFile(manager.path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(testNodeID, directory, "1.0.31", "initial", nil); err == nil {
				t.Fatalf("corrupt persisted settings were accepted: %s", changed)
			}
		})
	}
}

func TestMCPCheckUsesExactIDAndDraftCAS(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := Snapshot{Inference: Inference{ModelID: textPointer("initial")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 1000, Auth: MCPAuth{Kind: "none", SecretAction: "remove"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatalf("put issue=%+v", issue)
	}
	if _, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 1, MCPServerID: "docs"}); issue == nil || issue.Code != "revision_conflict" {
		t.Fatalf("stale check issue=%+v", issue)
	}
	check, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 2, MCPServerID: "docs"})
	if issue != nil || check.MCPServerID != "docs" || check.State != "unsupported" {
		t.Fatalf("check=%+v issue=%+v", check, issue)
	}
}

func TestSecretActionVocabularyIsExact(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := Snapshot{Inference: Inference{ModelID: textPointer("initial")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 1000, Auth: MCPAuth{Kind: "none", SecretAction: "delete"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue == nil || issue.Code != "invalid_request" {
		t.Fatalf("typo issue=%+v", issue)
	}
}
