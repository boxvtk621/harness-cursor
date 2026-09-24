package nodesettings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestV2DraftRoundTripKeepsSecretSlotAndSSE(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	document := MCPDocument{SchemaID: MCPDocumentSchemaID, Servers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "sse", URL: "https://example.test/events", TimeoutMS: 30000, Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "private"}}}}
	first, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: Snapshot{Inference: Inference{ModelID: textPointer("actual-model"), SpeedMode: textPointer("on")}, MCPDocument: document}})
	if issue != nil || first.Draft.MCPDocument.SchemaID != MCPDocumentSchemaID || first.Draft.MCPDocument.Servers[0].Auth.Secret != "" || first.Draft.MCPDocument.Servers[0].Transport != "sse" || first.Capabilities.SpeedDefault != "supported" || first.Capabilities.ReasoningDefault != "supported" {
		t.Fatalf("first=%+v issue=%+v", first, issue)
	}
	if got := runtimeConfig(manager.state.Draft); got.Params["fast"] != "true" || got.MCPServers[0].Transport != "sse" || got.MCPServers[0].BearerToken != "private" {
		t.Fatalf("native=%+v", got)
	}
	document.Servers[0].Auth = MCPAuth{Kind: "bearer", SecretAction: "keep"}
	second, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 2, Draft: Snapshot{Inference: first.Draft.Inference, MCPDocument: document}})
	if issue != nil || !second.Draft.MCPDocument.Servers[0].Auth.BearerTokenConfigured {
		t.Fatalf("second=%+v issue=%+v", second, issue)
	}
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", catalogStub{})
	if err != nil || reopened.state.Draft.MCPServers[0].BearerToken != "private" {
		t.Fatalf("reload=%v", err)
	}
}

type tupleCatalog struct{}

func (tupleCatalog) Models(context.Context) ([]NativeModel, string, error) {
	return []NativeModel{{ID: "tuple-model", DisplayName: "Tuple", Parameters: []NativeParameter{{ID: "fast", Values: []string{"false", "true"}}, {ID: "reasoning_effort", Values: []string{"low", "high"}}}, Variants: []NativeVariant{{Params: map[string]string{"fast": "false", "reasoning_effort": "low"}, IsDefault: true}, {Params: map[string]string{"fast": "true", "reasoning_effort": "low"}}, {Params: map[string]string{"fast": "false", "reasoning_effort": "high"}}, {Params: map[string]string{"fast": "true", "reasoning_effort": "high"}}}}}, "tuple-revision", nil
}

type sparseTupleCatalog struct{}

func (sparseTupleCatalog) Models(context.Context) ([]NativeModel, string, error) {
	return []NativeModel{{ID: "tuple-model", DisplayName: "Tuple", Parameters: []NativeParameter{{ID: "fast", Values: []string{"false", "true"}}, {ID: "reasoning_effort", Values: []string{"low", "high"}}}, Variants: []NativeVariant{{Params: map[string]string{"fast": "false", "reasoning_effort": "low"}, IsDefault: true}, {Params: map[string]string{"fast": "true", "reasoning_effort": "high"}}}}}, "sparse", nil
}

func TestV2RejectsUnsupportedCombinedVariant(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "tuple-model", sparseTupleCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	target := privateSnapshot{Inference: Inference{ModelID: textPointer("tuple-model"), SpeedMode: textPointer("on")}}
	if err := manager.validateRuntime(context.Background(), target); err == nil || err.Error() != "model_parameters_incompatible" {
		t.Fatalf("speed-only mismatch=%v", err)
	}
	target.Inference.ReasoningEffort = textPointer("high")
	if err := manager.validateRuntime(context.Background(), target); err != nil {
		t.Fatalf("joint variant=%v", err)
	}
}

func TestV2InferenceModesApplyAsNativeTuple(t *testing.T) {
	for _, item := range []struct {
		name                 string
		speed, effort        *string
		wantFast, wantEffort string
	}{
		{"model_only", nil, nil, "", ""},
		{"speed_only", textPointer("on"), nil, "true", ""},
		{"reasoning_only", nil, textPointer("high"), "", "high"},
		{"joint", textPointer("on"), textPointer("high"), "true", "high"},
	} {
		t.Run(item.name, func(t *testing.T) {
			manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "tuple-model", tupleCatalog{})
			if err != nil {
				t.Fatal(err)
			}
			stub := &runtimeStub{}
			gate := &sync.Mutex{}
			manager.BindRuntime(func() (func(), bool) { gate.Lock(); return gate.Unlock, false }, func() bool { return false }, stub)
			draft := Snapshot{Inference: Inference{ModelID: textPointer("tuple-model"), SpeedMode: item.speed, ReasoningEffort: item.effort}, MCPDocument: MCPDocument{SchemaID: MCPDocumentSchemaID, Servers: []MCPServer{}}}
			if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
				t.Fatal(issue)
			}
			started, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: item.name, ExpectedRevision: 2, TargetRevision: 2})
			if issue != nil {
				t.Fatal(issue)
			}
			final := waitOperation(t, manager, started.Operation.OperationID)
			if final.Operation.Status != "succeeded" || final.AppliedRevision != 2 || len(stub.calls) != 1 || stub.calls[0].Params["fast"] != item.wantFast || stub.calls[0].Params["reasoning_effort"] != item.wantEffort {
				t.Fatalf("final=%+v calls=%+v", final.Operation, stub.calls)
			}
		})
	}
}

func TestV2SSECheckUsesSavedRevisionAndBearer(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer sse-secret" {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(": ready\n\n"))
	}))
	defer remote.Close()
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	draft := Snapshot{Inference: Inference{ModelID: textPointer("actual-model")}, MCPDocument: MCPDocument{SchemaID: MCPDocumentSchemaID, Servers: []MCPServer{{ID: "sse", Name: "SSE", Enabled: true, Transport: "sse", URL: remote.URL, TimeoutMS: 5000, Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "sse-secret"}}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatal(issue)
	}
	if _, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 1, MCPServerID: "sse"}); issue == nil || issue.Code != "revision_conflict" {
		t.Fatalf("stale check=%+v", issue)
	}
	check, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 2, MCPServerID: "sse"})
	if issue != nil || check.State != "connected" || check.ToolsCount != nil {
		t.Fatalf("check=%+v issue=%+v", check, issue)
	}
}

func TestV2ValidationReportsJSONPointer(t *testing.T) {
	raw := []byte(`{"expectedRevision":1,"draft":{"inference":{"modelId":"m","speedMode":null,"reasoningEffort":null},"mcpDocument":{"schemaId":"harness-mcp-document-v2","servers":[{"id":"a","name":"A","enabled":true,"transport":"streamable_http","url":"https://example.test/mcp","timeoutMs":30000,"auth":{"kind":"none","secretAction":"remove","surprise":1}}]}}}`)
	issues := ValidatePutJSON(raw)
	if len(issues) != 1 || issues[0].Path != "/draft/mcpDocument/servers/0/auth/surprise" || issues[0].Code != "unknown_field" {
		t.Fatalf("issues=%+v", issues)
	}
	validation := ValidateMCPDocumentJSON(json.RawMessage(`{"schemaId":"harness-mcp-document-v2","servers":[{"id":"a","name":"A","enabled":true,"transport":"stdio","url":"https://example.test/mcp","timeoutMs":30000,"auth":{"kind":"none","secretAction":"remove"}}]}`))
	if validation.Valid || validation.Errors[0].Path != "/mcpDocument/servers/0/transport" {
		t.Fatalf("validation=%+v", validation)
	}
}

func TestPersistedV1HTTPAndSpeedMigrateWithoutSecretLoss(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"draftRevision":2,"appliedRevision":1,"draft":{"mcpServers":[{"id":"old","name":"Old","enabled":true,"transport":"streamable_http","url":"https://example.test/mcp","timeoutMs":30000,"bearerToken":"legacy-secret"}],"inference":{"modelId":"actual-model","speedMode":"false","reasoningEffort":null}},"applied":{"mcpServers":[],"inference":{"modelId":"actual-model","speedMode":null,"reasoningEffort":null}},"operations":{}}`
	if err := os.WriteFile(manager.path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	got, issue := reopened.Snapshot(context.Background(), testNodeID)
	if issue != nil || *got.Draft.Inference.SpeedMode != "off" || !got.Draft.MCPDocument.Servers[0].Auth.BearerTokenConfigured || got.Draft.MCPDocument.Servers[0].Auth.Secret != "" {
		t.Fatalf("migrated=%+v issue=%+v", got, issue)
	}
	private, err := os.ReadFile(reopened.path)
	if err != nil || !strings.Contains(string(private), "legacy-secret") {
		t.Fatalf("private migration=%v", err)
	}
}
