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
	"time"
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
	if envelope.DraftRevision != 2 || envelope.AppliedRevision != 1 || envelope.Draft.MCPDocument.Servers[0].Auth.Secret != "" || !envelope.Draft.MCPDocument.Servers[0].Auth.BearerTokenConfigured {
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
	if issue != nil || loaded.DraftRevision != 2 || !loaded.Draft.MCPDocument.Servers[0].Auth.BearerTokenConfigured || loaded.Draft.MCPDocument.Servers[0].Auth.Secret != "" {
		t.Fatalf("reopened=%+v issue=%+v", loaded, issue)
	}
}

func TestCatalogMapsOnlyExplicitSemanticParameters(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "initial", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, issue := manager.ModelCatalog(context.Background(), testNodeID)
	if issue != nil || catalog.SchemaID != ModelCatalogSchemaID || catalog.State != "fresh" || catalog.CatalogRevision != "sdk-revision" || len(catalog.Models) != 1 {
		t.Fatalf("catalog=%+v issue=%+v", catalog, issue)
	}
	model := catalog.Models[0]
	if len(model.SpeedModes) != 1 || model.SpeedModes[0].ID != "off" || len(model.ReasoningEfforts) != 1 || model.ReasoningEfforts[0].ID != "high" {
		t.Fatalf("model mapping=%+v", model)
	}
}

func TestUnavailableCatalogUsesCatalogSchema(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, issue := manager.ModelCatalog(context.Background(), testNodeID)
	if issue != nil || catalog.SchemaID != ModelCatalogSchemaID || catalog.State != "unavailable" || catalog.ReasonCode != "provider_auth_required" {
		t.Fatalf("catalog=%+v issue=%+v", catalog, issue)
	}
}

type runtimeStub struct {
	mu    sync.Mutex
	calls []RuntimeConfig
	fail  bool
}

func (stub *runtimeStub) RestartSettings(_ context.Context, config RuntimeConfig) (bool, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls = append(stub.calls, config)
	return false, nil
}

func waitOperation(t *testing.T, manager *Manager, id string) Envelope {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		value, issue := manager.Operation(context.Background(), testNodeID, id)
		if issue != nil {
			t.Fatal(issue)
		}
		if value.Operation.Status != "pending" {
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("apply did not finish")
	return Envelope{}
}

func TestApplyIsDurableIdempotentAndUsesPrivateSettings(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	stub := &runtimeStub{}
	gate := &sync.Mutex{}
	manager.BindRuntime(func() (func(), bool) { gate.Lock(); return gate.Unlock, false }, func() bool { return false }, stub)
	draft := Snapshot{Inference: Inference{ModelID: textPointer("actual-model"), SpeedMode: textPointer("false"), ReasoningEffort: textPointer("high")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 30000, Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "secret"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatal(issue)
	}
	envelope, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil || envelope.Operation == nil || envelope.Operation.Status != "pending" || envelope.AppliedRevision != 1 {
		t.Fatalf("envelope=%+v issue=%+v", envelope, issue)
	}
	finished := waitOperation(t, manager, envelope.Operation.OperationID)
	if finished.Operation.Status != "succeeded" || finished.AppliedRevision != 2 || finished.Applied.MCPDocument.Servers[0].Auth.Secret != "" {
		t.Fatalf("finished=%+v", finished)
	}
	readback, issue := manager.Snapshot(context.Background(), testNodeID)
	if issue != nil || readback.Operation == nil || readback.Operation.OperationID != envelope.Operation.OperationID || readback.Capabilities.ModelDefault != "unsupported" {
		t.Fatalf("latest operation was not discoverable after a lost ACK: %+v issue=%+v", readback, issue)
	}
	stub.mu.Lock()
	if len(stub.calls) != 1 || stub.calls[0].Params["fast"] != "false" || stub.calls[0].Params["reasoning_effort"] != "high" || stub.calls[0].MCPServers[0].BearerToken != "secret" {
		t.Fatalf("calls=%+v", stub.calls)
	}
	stub.mu.Unlock()
	replay, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil || replay.Operation.OperationID != envelope.Operation.OperationID {
		t.Fatalf("replay=%+v issue=%+v", replay, issue)
	}
	if !uuidPattern.MatchString(envelope.Operation.OperationID) {
		t.Fatalf("operation ID is not UUID-D: %q", envelope.Operation.OperationID)
	}
	if _, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 2, TargetRevision: 1}); issue == nil || issue.Code != "command_conflict" {
		t.Fatalf("mismatched replay issue=%+v", issue)
	}
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	replay, issue = reopened.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "apply-1", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil || replay.Operation.OperationID != envelope.Operation.OperationID {
		t.Fatalf("durable replay=%+v issue=%+v", replay, issue)
	}
}

func TestApplyWaitsForLifecycleGateAndTimesOutWithoutRestart(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	stub := &runtimeStub{}
	gate := &sync.Mutex{}
	gate.Lock() // Existing provider-auth transition owns the same gate.
	manager.BindRuntime(func() (func(), bool) { gate.Lock(); return gate.Unlock, true }, func() bool { return true }, stub)
	manager.applyTimeout = 30 * time.Millisecond
	accepted, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "timeout", ExpectedRevision: 1, TargetRevision: 1})
	if issue != nil {
		t.Fatal(issue)
	}
	time.Sleep(20 * time.Millisecond)
	stub.mu.Lock()
	calls := len(stub.calls)
	stub.mu.Unlock()
	if calls != 0 {
		t.Fatal("restart crossed provider-auth gate")
	}
	gate.Unlock()
	final := waitOperation(t, manager, accepted.Operation.OperationID)
	if final.Operation.Status != "failed" || final.Operation.ReasonCode != "drain_timeout" || final.AppliedRevision != 1 {
		t.Fatalf("timeout=%+v", final)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls) != 0 {
		t.Fatal("busy attempt was interrupted")
	}
}

func TestPendingApplyRecoveryNeverActivatesDraft(t *testing.T) {
	directory := t.TempDir()
	manager, err := Open(testNodeID, directory, "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.state.DraftRevision = 2
	manager.state.Draft.Inference.ModelID = textPointer("new-model")
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	operation := Operation{OperationID: randomID(), CommandID: "interrupted", TargetRevision: 2, PreviousRevision: 1, Status: "pending", Phase: "restarting", CreatedAt: stamp, UpdatedAt: stamp}
	manager.state.Operations["interrupted"] = operationRecord{Operation: operation, ExpectedRevision: 2, TargetRevision: 2, Target: manager.state.Draft}
	if err := manager.persistLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	read, issue := reopened.Operation(context.Background(), testNodeID, operation.OperationID)
	if issue != nil || read.Operation.Status != "failed" || read.Operation.ReasonCode != "apply_interrupted" || read.AppliedRevision != 1 || *read.Applied.Inference.ModelID != "actual-model" {
		t.Fatalf("recovered=%+v issue=%+v", read, issue)
	}
	replay, issue := reopened.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "interrupted", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil || replay.Operation.OperationID != operation.OperationID || replay.Operation.Status != "failed" {
		t.Fatalf("replay=%+v issue=%+v", replay, issue)
	}
}

func TestLegacyOperationWithoutTargetPreservesDevStateAndReplay(t *testing.T) {
	for _, status := range []string{"failed", "pending"} {
		t.Run(status, func(t *testing.T) {
			directory := t.TempDir()
			manager, err := Open(testNodeID, directory, "1.0.31", "composer-2.5", nil)
			if err != nil {
				t.Fatal(err)
			}
			draft := Snapshot{Inference: Inference{ModelID: textPointer("grok-4.7")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 1000, Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "preserved-secret"}}}}
			if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
				t.Fatal(issue)
			}
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			operation := Operation{OperationID: randomID(), CommandID: "old-command", TargetRevision: 2, PreviousRevision: 1, Status: status, Phase: "preflight", ReasonCode: "managed_restart_coordination_unavailable", CreatedAt: stamp, UpdatedAt: stamp}
			manager.mu.Lock()
			manager.state.Operations["old-command"] = operationRecord{Operation: operation, ExpectedRevision: 2, TargetRevision: 2}
			if err := manager.persistLocked(); err != nil {
				manager.mu.Unlock()
				t.Fatal(err)
			}
			manager.mu.Unlock()
			raw, err := os.ReadFile(manager.path)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]any
			if err := json.Unmarshal(raw, &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy["operations"].(map[string]any)["old-command"].(map[string]any), "target")
			raw, err = json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manager.path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(testNodeID, directory, "1.0.31", "ignored", nil)
			if err != nil {
				t.Fatal(err)
			}
			read, issue := reopened.Snapshot(context.Background(), testNodeID)
			if issue != nil || read.DraftRevision != 2 || read.AppliedRevision != 1 || *read.Applied.Inference.ModelID != "composer-2.5" || !read.Draft.MCPDocument.Servers[0].Auth.BearerTokenConfigured {
				t.Fatalf("legacy read=%+v issue=%+v", read, issue)
			}
			if read.Operation == nil || read.Operation.OperationID != operation.OperationID || read.Operation.Status != "failed" {
				t.Fatalf("legacy operation=%+v", read.Operation)
			}
			if status == "pending" && read.Operation.ReasonCode != "apply_interrupted" {
				t.Fatalf("pending legacy was not failed closed: %+v", read.Operation)
			}
			replay, issue := reopened.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "old-command", ExpectedRevision: 2, TargetRevision: 2})
			if issue != nil || replay.Operation.OperationID != operation.OperationID {
				t.Fatalf("legacy replay=%+v issue=%+v", replay, issue)
			}
			private, err := os.ReadFile(reopened.path)
			if err != nil || !strings.Contains(string(private), "preserved-secret") {
				t.Fatalf("secret was lost: %v", err)
			}
		})
	}
}

func TestNullModelFailsWithSpecificUnsupportedReason(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	stub := &runtimeStub{}
	manager.BindRuntime(func() (func(), bool) { return func() {}, false }, func() bool { return false }, stub)
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: Snapshot{MCPServers: []MCPServer{}, Inference: Inference{}}}); issue != nil {
		t.Fatal(issue)
	}
	accepted, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "default", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil {
		t.Fatal(issue)
	}
	final := waitOperation(t, manager, accepted.Operation.OperationID)
	if final.Operation.ReasonCode != "model_default_unsupported" || final.AppliedRevision != 1 {
		t.Fatalf("default=%+v", final)
	}
}

func TestNativeMCPRejectsUnmappableTimeout(t *testing.T) {
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "actual-model", catalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	stub := &runtimeStub{}
	manager.BindRuntime(func() (func(), bool) { return func() {}, false }, func() bool { return false }, stub)
	draft := Snapshot{Inference: Inference{ModelID: textPointer("actual-model")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "https://example.test/mcp", TimeoutMS: 5000, Auth: MCPAuth{Kind: "none", SecretAction: "remove"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatal(issue)
	}
	accepted, issue := manager.Apply(context.Background(), testNodeID, CommandRequest{CommandID: "timeout-unsupported", ExpectedRevision: 2, TargetRevision: 2})
	if issue != nil {
		t.Fatal(issue)
	}
	final := waitOperation(t, manager, accepted.Operation.OperationID)
	if final.Operation.ReasonCode != "mcp_timeout_unsupported" || final.AppliedRevision != 1 {
		t.Fatalf("timeout=%+v", final)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls) != 0 {
		t.Fatal("unsupported timeout reached native worker")
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
	draft := Snapshot{Inference: Inference{ModelID: textPointer("initial")}, MCPServers: []MCPServer{{ID: "docs", Name: "Docs", Enabled: true, Transport: "streamable_http", URL: "http://127.0.0.1:1/mcp", TimeoutMS: 1000, Auth: MCPAuth{Kind: "none", SecretAction: "remove"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatalf("put issue=%+v", issue)
	}
	if _, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 1, MCPServerID: "docs"}); issue == nil || issue.Code != "revision_conflict" {
		t.Fatalf("stale check issue=%+v", issue)
	}
	check, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 2, MCPServerID: "docs"})
	if issue != nil || check.MCPServerID != "docs" || check.State != "unavailable" {
		t.Fatalf("check=%+v issue=%+v", check, issue)
	}
}

func TestMCPCheckHandshakesAndListsToolsWithoutSDK(t *testing.T) {
	methods := []string{}
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fixture-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var rpc struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(request.Body).Decode(&rpc) != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		methods = append(methods, rpc.Method)
		if rpc.Method == "notifications/initialized" {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		result := any(map[string]any{"tools": []map[string]string{{"name": "fixture_a"}, {"name": "fixture_b"}}})
		if rpc.Method == "initialize" {
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer remote.Close()
	manager, err := Open(testNodeID, t.TempDir(), "1.0.31", "actual-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := Snapshot{Inference: Inference{ModelID: textPointer("actual-model")}, MCPServers: []MCPServer{{ID: "fixture", Name: "Fixture", Enabled: true, Transport: "streamable_http", URL: remote.URL, TimeoutMS: 5000, Auth: MCPAuth{Kind: "bearer", SecretAction: "replace", Secret: "fixture-secret"}}}}
	if _, issue := manager.PutDraft(context.Background(), testNodeID, PutRequest{ExpectedRevision: 1, Draft: draft}); issue != nil {
		t.Fatal(issue)
	}
	check, issue := manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 2, MCPServerID: "fixture"})
	if issue != nil || check.State != "connected" || check.ToolsCount == nil || *check.ToolsCount != 2 {
		t.Fatalf("check=%+v issue=%+v", check, issue)
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("methods=%v", methods)
	}
	remote.Close()
	check, issue = manager.CheckMCP(context.Background(), testNodeID, MCPCheckRequest{ExpectedRevision: 2, MCPServerID: "fixture"})
	if issue != nil || check.State != "unavailable" || check.ReasonCode != "mcp_connect_failed" {
		t.Fatalf("unavailable=%+v issue=%+v", check, issue)
	}
}

func TestMCPCheckReadsSingleSSEFrameWithoutWaitingForStreamClose(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(request.Body).Decode(&rpc)
		switch rpc.Method {
		case "initialize":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"fixture_b\"}]}}\n\n"))
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		}
	}))
	defer remote.Close()
	count, reason := probeMCP(context.Background(), privateMCP{URL: remote.URL, TimeoutMS: 1000})
	if reason != "" || count != 1 {
		t.Fatalf("SSE count=%d reason=%s", count, reason)
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
