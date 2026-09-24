package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/api"
	"github.com/boxvtk621/harness-cursor/contracts/barrier"
	"github.com/boxvtk621/harness-cursor/contracts/dialogview"
	"github.com/boxvtk621/harness-cursor/contracts/tooltimeline"
	"github.com/boxvtk621/harness-cursor/contracts/transcript-view"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/nodesettings"
	"github.com/boxvtk621/harness-cursor/providerauth"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tests/fixture"
)

const testNodeID = "20000000-0000-4000-8000-000000000001"

type enoughSpace struct{}

type authStub struct{ secret string }

func authEnvelope() providerauth.Envelope {
	return providerauth.Envelope{SchemaID: providerauth.SchemaID, NodeID: testNodeID, Revision: 1, State: providerauth.StateUnauthenticated, Capabilities: providerauth.Capabilities{Methods: []string{"secret"}, CanCheck: true, CanLogout: true}}
}
func (stub *authStub) Snapshot(_ context.Context, nodeID string) (providerauth.Envelope, *providerauth.APIError) {
	if nodeID != testNodeID {
		return providerauth.Envelope{}, providerauth.ErrNotFound
	}
	return authEnvelope(), nil
}
func (stub *authStub) Operation(context.Context, string, string) (providerauth.Envelope, *providerauth.APIError) {
	return providerauth.Envelope{}, providerauth.ErrNotFound
}
func (stub *authStub) Start(_ context.Context, input providerauth.StartRequest) (providerauth.Envelope, *providerauth.APIError) {
	stub.secret = input.Secret
	envelope := authEnvelope()
	envelope.Operation = &providerauth.Operation{OperationID: "30000000-0000-4000-8000-000000000001", CommandID: input.CommandID, Method: input.Method, Status: providerauth.OperationPending, CreatedAt: "2026-09-21T16:00:00Z", UpdatedAt: "2026-09-21T16:00:00Z"}
	return envelope, nil
}
func (stub *authStub) Check(context.Context, providerauth.CommandRequest) (providerauth.Envelope, *providerauth.APIError) {
	return authEnvelope(), nil
}
func (stub *authStub) Cancel(context.Context, string, providerauth.CommandRequest) (providerauth.Envelope, *providerauth.APIError) {
	return authEnvelope(), nil
}
func (stub *authStub) Logout(context.Context, providerauth.CommandRequest) (providerauth.Envelope, *providerauth.APIError) {
	return authEnvelope(), nil
}

func (enoughSpace) Measure(string) (node.SpaceInfo, error) {
	return node.SpaceInfo{FreeBytes: 16 << 30, TotalBytes: 64 << 30}, nil
}

func TestProviderAuthRoutesAreStrictAndSecretIsWriteOnly(t *testing.T) {
	authority, err := node.Open(context.Background(), node.Config{DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1, Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: enoughSpace{}, ManualDispatchForTesting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	auth := &authStub{}
	handler, err := server.New(server.Config{NodeID: testNodeID, ProviderAuth: auth}, authority)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	response, err := http.Get(endpoint.URL + "/v1/provider-auth?nodeId=" + testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"schemaId":"harness-provider-auth-v1"`) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status=%d headers=%v body=%s", response.StatusCode, response.Header, raw)
	}
	response, err = http.Get(endpoint.URL + "/v1/provider-auth")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing nodeId status=%d", response.StatusCode)
	}
	body := `{"nodeId":"` + testNodeID + `","commandId":"40000000-0000-4000-8000-000000000001","method":"secret","secret":"write-only-value"}`
	response, err = http.Post(endpoint.URL+"/v1/provider-auth/operations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || strings.Contains(string(raw), "write-only-value") || auth.secret != "write-only-value" {
		t.Fatalf("POST status=%d body=%s captured=%q", response.StatusCode, raw, auth.secret)
	}
	response, err = http.Post(endpoint.URL+"/v1/provider-auth/operations", "application/json", strings.NewReader(strings.TrimSuffix(body, "}")+`,"unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", response.StatusCode)
	}
}

func TestNodeSettingsRoutesAreStrictCASAndSecretIsWriteOnly(t *testing.T) {
	authority, err := node.Open(context.Background(), node.Config{DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1, Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: enoughSpace{}, ManualDispatchForTesting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	settings, err := nodesettings.Open(testNodeID, t.TempDir(), "1.0.31", "initial-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.New(server.Config{NodeID: testNodeID, NodeSettings: settings}, authority)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()

	response, err := http.Get(endpoint.URL + "/v1/nodes/" + testNodeID + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"schemaId":"harness-node-settings-v2"`) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET settings status=%d body=%s", response.StatusCode, raw)
	}

	body := `{"expectedRevision":1,"draft":{"mcpServers":[{"id":"docs","name":"Docs","enabled":true,"transport":"streamable_http","url":"http://127.0.0.1:1/mcp","timeoutMs":5000,"auth":{"kind":"bearer","secretAction":"replace","secret":"write-only"}}],"inference":{"modelId":null,"speedMode":null,"reasoningEffort":null}}}`
	request, _ := http.NewRequest(http.MethodPut, endpoint.URL+"/v1/nodes/"+testNodeID+"/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(raw), "write-only") || !strings.Contains(string(raw), `"bearerTokenConfigured":true`) || !strings.Contains(string(raw), `"modelId":null`) {
		t.Fatalf("PUT settings status=%d body=%s", response.StatusCode, raw)
	}

	request, _ = http.NewRequest(http.MethodPut, endpoint.URL+"/v1/nodes/"+testNodeID+"/settings", strings.NewReader(strings.TrimSuffix(body, "}")+`,"unknown":true}`))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", response.StatusCode)
	}

	outputField := strings.Replace(body, `"secretAction":"replace"`, `"bearerTokenConfigured":true,"secretAction":"replace"`, 1)
	request, _ = http.NewRequest(http.MethodPut, endpoint.URL+"/v1/nodes/"+testNodeID+"/settings", strings.NewReader(outputField))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale auth update status=%d", response.StatusCode)
	}

	validationBody := `{"mcpDocument":{"schemaId":"harness-mcp-document-v2","servers":[{"id":"docs","name":"Docs","enabled":true,"transport":"stdio","url":"https://example.test/mcp","timeoutMs":30000,"auth":{"kind":"none","secretAction":"remove"}}]}}`
	response, err = http.Post(endpoint.URL+"/v1/nodes/"+testNodeID+"/settings/mcp-validate", "application/json", strings.NewReader(validationBody))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"valid":false`) || !strings.Contains(string(raw), `"path":"/mcpDocument/servers/0/transport"`) {
		t.Fatalf("validation status=%d body=%s", response.StatusCode, raw)
	}

	checkBody := `{"expectedRevision":2,"mcpServerId":"docs"}`
	response, err = http.Post(endpoint.URL+"/v1/nodes/"+testNodeID+"/settings/mcp-checks", "application/json", strings.NewReader(checkBody))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"mcpServerId":"docs"`) {
		t.Fatalf("MCP check status=%d body=%s", response.StatusCode, raw)
	}

	apply := `{"commandId":"apply-1","expectedRevision":2,"targetRevision":2}`
	response, err = http.Post(endpoint.URL+"/v1/nodes/"+testNodeID+"/settings/apply", "application/json", strings.NewReader(apply))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"code":"settings_unavailable"`) {
		t.Fatalf("apply status=%d body=%s", response.StatusCode, raw)
	}
}

func TestRealTLSCommandsAndReadsWithoutInboundAuthorization(t *testing.T) {
	ca, caKey, caPool := certificateAuthority(t)
	serverCertificate, _ := signedCertificate(t, ca, caKey, "localhost", false)
	authority, err := node.Open(context.Background(), node.Config{
		DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1,
		Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: enoughSpace{}, ManualDispatchForTesting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	handler, err := server.New(server.Config{NodeID: testNodeID}, authority)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewUnstartedServer(handler)
	endpoint.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate}, MinVersion: tls.VersionTLS13,
	}
	endpoint.StartTLS()
	defer endpoint.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
	}}}
	operatorClient := client

	identity := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/identity", "", "1-1")
	validateResponse(t, identity, http.StatusOK, "nodeIdentity")
	var expected harnessprotocol.NodeIdentity
	if err := json.Unmarshal(identity[2:], &expected); err != nil {
		t.Fatal(err)
	}
	holdRequest, _ := json.Marshal(harnessbarrier.InstallRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: "server-proof-1",
		NodeID: testNodeID, ExpectedEpoch: expected.IdentityEpoch, BindingGeneration: expected.RegistryVersion,
		Scope: harnessbarrier.Scope{Kind: "node"}, ExpectedScopeRevision: 0,
	})
	wrongRole := request(t, client, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/administration/holds", string(holdRequest), "1-1")
	if status := int(wrongRole[0])<<8 | int(wrongRole[1]); status != http.StatusCreated {
		t.Fatalf("admin request without operator identity status=%d", status)
	}
	holdResponse := request(t, operatorClient, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/administration/holds", string(holdRequest), "1-1")
	if status := int(holdResponse[0])<<8 | int(holdResponse[1]); (status != http.StatusCreated && status != http.StatusOK) || harnessbarrier.Validate("holdReceipt", holdResponse[2:]) != nil {
		t.Fatalf("administrative hold endpoint failed: status=%d body=%s", status, holdResponse[2:])
	}
	var hold harnessbarrier.HoldReceipt
	_ = json.Unmarshal(holdResponse[2:], &hold)
	proofResponse := request(t, operatorClient, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/administration/holds/server-proof-1/proof", "", "1-1")
	if status := int(proofResponse[0])<<8 | int(proofResponse[1]); status != http.StatusOK || harnessbarrier.Validate("quiescenceProof", proofResponse[2:]) != nil {
		t.Fatalf("administrative proof endpoint failed: status=%d body=%s", status, proofResponse[2:])
	}
	releaseRequest, _ := json.Marshal(harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: hold.OperationID,
		NodeID: hold.NodeID, ExpectedEpoch: hold.Epoch, BindingGeneration: hold.BindingGeneration, Scope: hold.Scope,
		HoldVersion: hold.HoldVersion, ExpectedScopeRevision: hold.ScopeRevision, Action: "release",
	})
	releaseResponse := request(t, operatorClient, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/administration/holds/server-proof-1/release", string(releaseRequest), "1-1")
	if status := int(releaseResponse[0])<<8 | int(releaseResponse[1]); status != http.StatusCreated || harnessbarrier.Validate("releaseReceipt", releaseResponse[2:]) != nil {
		t.Fatalf("administrative release endpoint failed: status=%d body=%s", status, releaseResponse[2:])
	}
	create := `{"protocolVersion":1,"schemaId":"harness-wire-v2","commandId":"10000000-0000-4000-8000-000000000201","kind":"dialog.create","target":{"nodeId":"` + testNodeID + `"},"expected":{"registryVersion":1},"payload":{}}`
	accepted := requestExpected(t, client, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands", create, "1-1", &expected)
	validateResponse(t, accepted, http.StatusAccepted, "receipt")
	stale := expected
	stale.IdentityEpoch++
	rejected := requestExpected(t, client, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands", create, "1-1", &stale)
	validateResponse(t, rejected, http.StatusConflict, "error")
	status := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands/10000000-0000-4000-8000-000000000201", "", "1-1")
	validateResponse(t, status, http.StatusOK, "commandStatus")
	snapshot := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/snapshot", "", "1-1")
	validateResponse(t, snapshot, http.StatusOK, "snapshot")
	ready := request(t, client, http.MethodGet, endpoint.URL+"/health/ready", "", "1-1")
	validateResponse(t, ready, http.StatusOK, "healthReady")
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/snapshot", "", "1-2"), http.StatusOK, "snapshot")
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/identity", "", ""), http.StatusOK, "nodeIdentity")
	if heartbeat := request(t, client, http.MethodGet, endpoint.URL+"/v1/executor/heartbeat", "", ""); int(heartbeat[0])<<8|int(heartbeat[1]) != http.StatusOK {
		t.Fatalf("heartbeat failed: %s", heartbeat[2:])
	}
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/10000000-0000-4000-8000-000000000000/snapshot", "", ""), http.StatusNotFound, "error")

	var createReceipt harnessprotocol.Receipt
	if err := json.Unmarshal(accepted[2:], &createReceipt); err != nil {
		t.Fatal(err)
	}
	var created harnessprotocol.DialogCreateReferences
	if err := json.Unmarshal(createReceipt.References, &created); err != nil {
		t.Fatal(err)
	}
	enqueue := `{"protocolVersion":1,"schemaId":"harness-wire-v2","commandId":"10000000-0000-4000-8000-000000000202","kind":"message.enqueue","target":{"nodeId":"` + testNodeID + `","dialogId":"` + created.DialogID + `"},"expected":{"dialogVersion":1},"payload":{"text":"safe text"}}`
	enqueued := requestExpected(t, client, http.MethodPost, endpoint.URL+"/v1/nodes/"+testNodeID+"/commands", enqueue, "1-1", &expected)
	validateResponse(t, enqueued, http.StatusAccepted, "receipt")
	var enqueueReceipt harnessprotocol.Receipt
	if err := json.Unmarshal(enqueued[2:], &enqueueReceipt); err != nil {
		t.Fatal(err)
	}
	var message harnessprotocol.MessageEnqueueReferences
	if err := json.Unmarshal(enqueueReceipt.References, &message); err != nil {
		t.Fatal(err)
	}
	activity := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/dialogs?view=activity&limit=10", "", "1-1")
	var activityPage dialogview.Page
	if status := int(activity[0])<<8 | int(activity[1]); status != http.StatusOK || json.Unmarshal(activity[2:], &activityPage) != nil || len(activityPage.Items) != 1 || activityPage.Items[0].State != "queued" {
		t.Fatalf("dialog activity endpoint failed: status=%d body=%s", status, activity[2:])
	}
	readDialog := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/dialogs/"+created.DialogID, "", "1-1")
	var dialogRead dialogview.Read
	if status := int(readDialog[0])<<8 | int(readDialog[1]); status != http.StatusOK || json.Unmarshal(readDialog[2:], &dialogRead) != nil || dialogRead.Dialog.DialogID != created.DialogID {
		t.Fatalf("dialog read endpoint failed: status=%d body=%s", status, readDialog[2:])
	}
	latest := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/dialogs/"+created.DialogID+"/messages?order=latest&limit=10", "", "1-1")
	validateResponse(t, latest, http.StatusOK, "historyPage")
	filtered := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/requests?dialogId="+created.DialogID+"&limit=10", "", "1-1")
	validateResponse(t, filtered, http.StatusOK, "requestPage")
	dispatched, err := authority.DispatchNext(context.Background())
	if err != nil || dispatched.AttemptID == "" {
		t.Fatalf("dispatch failed: %+v err=%v", dispatched, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var current harnessprotocol.Snapshot
		result := authority.Snapshot(context.Background(), node.TrustContext{TransportNodeID: testNodeID})
		if result.HTTPStatus == 200 && json.Unmarshal(result.Body, &current) == nil && current.ActiveAttempt != nil && current.ActiveAttempt.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attempt did not become running")
		}
		time.Sleep(time.Millisecond)
	}
	reference := harnessadapter.AttemptRef{NodeID: testNodeID, DialogID: created.DialogID, RequestID: message.RequestID, AttemptID: dispatched.AttemptID, Generation: 1}
	callID := "40000000-0000-4000-8000-000000000090"
	toolContent := harnessprotocol.SafeContent{Kind: "inline", Content: "safe", Redaction: "none"}
	if err := authority.ObserveAdapterEvent(context.Background(), reference, harnessadapter.ToolStartedEvent{EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: callID, ToolName: "fixture.echo", ActionHash: strings.Repeat("a", 64), Input: toolContent}); err != nil {
		t.Fatal(err)
	}
	if err := authority.ObserveAdapterEvent(context.Background(), reference, harnessadapter.ToolCompletedEvent{EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: callID, Status: "succeeded", Result: toolContent, EffectStatus: "none"}); err != nil {
		t.Fatal(err)
	}
	tools := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/attempts/"+dispatched.AttemptID+"/tool-calls?limit=10", "", "1-1")
	var toolPage tooltimeline.Page
	if status := int(tools[0])<<8 | int(tools[1]); status != http.StatusOK || json.Unmarshal(tools[2:], &toolPage) != nil || len(toolPage.Items) != 1 || toolPage.Items[0].ToolCallID != callID {
		t.Fatalf("tool timeline endpoint failed: status=%d body=%s", status, tools[2:])
	}
	tool := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/attempts/"+dispatched.AttemptID+"/tool-calls/"+callID+"?limit=10", "", "1-1")
	var toolRead tooltimeline.DetailRead
	if status := int(tool[0])<<8 | int(tool[1]); status != http.StatusOK || json.Unmarshal(tool[2:], &toolRead) != nil || toolRead.ToolCall.ToolCallID != callID || toolRead.ToolCall.Result == nil {
		t.Fatalf("tool detail endpoint failed: status=%d body=%s", status, tool[2:])
	}
	messageID := "40000000-0000-4000-8000-000000000091"
	fullText := strings.Repeat("x", transcriptview.MaximumPreview+17)
	if err := authority.ObserveAdapterEvent(context.Background(), reference, harnessadapter.AssistantMessageEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, MessageID: messageID,
		Content:      harnessprotocol.SafeContent{Kind: "inline", Content: fullText[:transcriptview.MaximumPreview], Redaction: "none", Truncated: true},
		FinishReason: "complete", FullText: &fullText,
	}); err != nil {
		t.Fatal(err)
	}
	query := url.Values{
		"dialogId": {created.DialogID}, "attemptId": {dispatched.AttemptID}, "sourceKind": {"assistant_message"},
		"sourceId": {messageID}, "sourceIndex": {"0"}, "sourceStream": {"none"},
	}.Encode()
	resolved := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/texts/resolve?"+query, "", "1-1")
	if status := int(resolved[0])<<8 | int(resolved[1]); status != http.StatusOK {
		t.Fatalf("safe text status=%d body=%s", status, resolved[2:])
	}
	manifest, err := transcriptview.Decode(resolved[2:])
	if err != nil || manifest.NodeID != testNodeID || manifest.DialogID != created.DialogID || manifest.AttemptID != dispatched.AttemptID ||
		manifest.Source.ID != messageID || !manifest.Complete || manifest.SizeBytes != int64(len(fullText)) {
		t.Fatalf("safe text endpoint returned wrong source: %+v err=%v", manifest, err)
	}
	chunk := manifest.Chunks[0]
	chunkQuery := url.Values{
		"dialogId": {created.DialogID}, "attemptId": {dispatched.AttemptID}, "sourceKind": {"assistant_message"},
		"sourceId": {messageID}, "sourceIndex": {"0"}, "sourceStream": {"none"}, "artifactId": {chunk.ArtifactID},
		"sizeBytes": {strconv.FormatInt(chunk.SizeBytes, 10)}, "sha256": {chunk.SHA256},
	}.Encode()
	chunkURL := endpoint.URL + "/v1/nodes/" + testNodeID + "/texts/" + manifest.TextID + "/chunks/0?" + chunkQuery
	chunkRequest, err := http.NewRequest(http.MethodGet, chunkURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	chunkResponse, err := client.Do(chunkRequest)
	if err != nil {
		t.Fatal(err)
	}
	chunkBody, err := io.ReadAll(chunkResponse.Body)
	chunkResponse.Body.Close()
	if err != nil || chunkResponse.StatusCode != http.StatusOK || string(chunkBody) != fullText ||
		chunkResponse.Header.Get(transcriptview.TextIDHeader) != manifest.TextID ||
		chunkResponse.Header.Get(transcriptview.ArtifactIDHeader) != chunk.ArtifactID ||
		chunkResponse.Header.Get(transcriptview.ChunkSHA256Header) != chunk.SHA256 {
		t.Fatalf("exact safe text chunk failed: status=%d headers=%v bytes=%d err=%v", chunkResponse.StatusCode, chunkResponse.Header, len(chunkBody), err)
	}
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/artifacts/"+chunk.ArtifactID+"/metadata", "", "1-1"), http.StatusNotFound, "error")
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/artifacts/"+chunk.ArtifactID, "", "1-1"), http.StatusNotFound, "error")
	wrongChunkQuery := strings.Replace(chunkQuery, chunk.SHA256, strings.Repeat("0", 64), 1)
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/texts/"+manifest.TextID+"/chunks/0?"+wrongChunkQuery, "", "1-1"), http.StatusNotFound, "error")
	invalidQuery := strings.Replace(query, "sourceStream=none", "sourceStream=stdout", 1)
	validateResponse(t, request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/texts/resolve?"+invalidQuery, "", "1-1"), http.StatusBadRequest, "error")
	foreignActorResolved := request(t, client, http.MethodGet, endpoint.URL+"/v1/nodes/"+testNodeID+"/texts/resolve?"+query, "", "1-2")
	if status := int(foreignActorResolved[0])<<8 | int(foreignActorResolved[1]); status != http.StatusOK {
		t.Fatalf("safe text without actor authorization status=%d body=%s", status, foreignActorResolved[2:])
	}
	if _, err := transcriptview.Decode(foreignActorResolved[2:]); err != nil {
		t.Fatalf("safe text without actor authorization is invalid: %v", err)
	}

	withoutCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: caPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}}}
	requestValue, _ := http.NewRequest(http.MethodGet, endpoint.URL+"/health/live", nil)
	response, err := withoutCertificate.Do(requestValue)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated private request rejected: status=%v err=%v", response, err)
	}
	response.Body.Close()
}

func request(t *testing.T, client *http.Client, method, url, body, actor string) []byte {
	return requestExpected(t, client, method, url, body, actor, nil)
}

func requestExpected(t *testing.T, client *http.Client, method, url, body, actor string, expected *harnessprotocol.NodeIdentity) []byte {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if expected != nil {
		request.Header.Set(harnessprotocol.ExpectedNodeIDHeader, expected.NodeID)
		request.Header.Set(harnessprotocol.ExpectedRegistryHeader, strconv.FormatInt(expected.RegistryVersion, 10))
		request.Header.Set(harnessprotocol.ExpectedEpochHeader, strconv.FormatInt(expected.IdentityEpoch, 10))
		request.Header.Set(harnessprotocol.ExpectedAdapterKindHeader, expected.Adapter.Kind)
		request.Header.Set(harnessprotocol.ExpectedAdapterVersionHeader, expected.Adapter.Version)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{byte(response.StatusCode >> 8), byte(response.StatusCode)}, content...)
}

func validateResponse(t *testing.T, encoded []byte, status int, wireType string) {
	t.Helper()
	actualStatus := int(encoded[0])<<8 | int(encoded[1])
	body := encoded[2:]
	if actualStatus != status {
		t.Fatalf("status=%d want=%d body=%s", actualStatus, status, body)
	}
	if err := harnessprotocol.Validate(wireType, body); err != nil {
		t.Fatalf("invalid %s: %v body=%s", wireType, err, body)
	}
}

func certificateAuthority(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return certificate, key, pool
}

func signedCertificate(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, name string, client bool) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if client {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		template.DNSNames = nil
	}
	template.ExtKeyUsage = usage
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: leaf}, leaf
}
