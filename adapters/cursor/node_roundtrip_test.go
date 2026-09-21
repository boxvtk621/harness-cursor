package cursor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tools"
)

const cursorNodeTestID = "10000000-0000-4000-8000-000000000001"

type cursorPolicySource struct {
	policy harnessadapter.PolicySnapshot
}

func (source cursorPolicySource) Current(context.Context, string) (harnessadapter.PolicySnapshot, error) {
	return source.policy, nil
}

type cursorSpaceProbe struct{}

func (cursorSpaceProbe) Measure(string) (node.SpaceInfo, error) {
	return node.SpaceInfo{FreeBytes: node.MinimumFreeBytes + node.ControlReserveBytes, TotalBytes: 2 * node.MinimumFreeBytes}, nil
}

func TestExplicitApprovalRoundTripsOperatorVersionOneToAdapterVersionTwo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := &fakeRunner{
		requests: make(chan toolrunner.Request, 1),
		result:   toolrunner.Result{Success: true, Output: []byte("changed")},
	}
	config := fakeConfig(t)
	config.ToolRunner = runner
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	opened, err := node.Open(ctx, node.Config{
		DataDir: t.TempDir(), NodeID: cursorNodeTestID, OwnerID: "1-1", RegistryVersion: 1,
		Adapter: adapter, Policies: cursorPolicySource{policy: explicitPolicy()}, Space: cursorSpaceProbe{},
		ManualDispatchForTesting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	trust := node.TrustContext{TransportNodeID: cursorNodeTestID}

	created := cursorNodeReceipt(t, opened.SubmitCommand(ctx, trust, cursorNodeCommand(t,
		"11000000-0000-4000-8000-000000000001", "dialog.create",
		map[string]any{"nodeId": cursorNodeTestID}, map[string]any{"registryVersion": 1}, map[string]any{})), 202)
	var dialog harnessprotocol.DialogCreateReferences
	if err := json.Unmarshal(created.References, &dialog); err != nil {
		t.Fatal(err)
	}
	cursorNodeReceipt(t, opened.SubmitCommand(ctx, trust, cursorNodeCommand(t,
		"11000000-0000-4000-8000-000000000002", "message.enqueue",
		map[string]any{"nodeId": cursorNodeTestID, "dialogId": dialog.DialogID}, map[string]any{"dialogVersion": 1}, map[string]any{"text": "file-tool"})), 202)
	dispatched, err := opened.DispatchNext(ctx)
	if err != nil || dispatched.AttemptID == "" {
		t.Fatalf("dispatch = %#v, %v", dispatched, err)
	}

	event := waitCursorNodeEvent(t, ctx, opened, trust, dispatched.AttemptID, "approval.requested")
	var requested harnessprotocol.ApprovalRequestedPayload
	if err := json.Unmarshal(event.Payload, &requested); err != nil {
		t.Fatal(err)
	}
	if requested.ApprovalVersion != 1 {
		t.Fatalf("operator-facing approval version = %d, want 1", requested.ApprovalVersion)
	}
	cursorNodeReceipt(t, opened.SubmitCommand(ctx, trust, cursorNodeCommand(t,
		"11000000-0000-4000-8000-000000000003", "approval.respond",
		map[string]any{"nodeId": cursorNodeTestID, "approvalId": requested.ApprovalID, "attemptId": dispatched.AttemptID},
		map[string]any{"approvalVersion": requested.ApprovalVersion, "attemptGeneration": 1},
		map[string]any{"decision": "allow_once", "actionHash": requested.ActionHash})), 202)

	select {
	case request := <-runner.requests:
		if request.Kind != toolrunner.KindFileChange {
			t.Fatalf("runner request = %#v", request)
		}
	case <-ctx.Done():
		t.Fatal("node approval did not reach the adapter and runner")
	}
	resolved := waitCursorNodeEvent(t, ctx, opened, trust, dispatched.AttemptID, "approval.resolved")
	var resolution harnessprotocol.ApprovalResolvedPayload
	if err := json.Unmarshal(resolved.Payload, &resolution); err != nil {
		t.Fatal(err)
	}
	if resolution.ApprovalVersion != 2 || resolution.Decision != "allow_once" {
		t.Fatalf("approval resolution = %#v", resolution)
	}
}

func cursorNodeCommand(t *testing.T, commandID, kind string, target, expected, payload any) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"protocolVersion": harnessprotocol.ProtocolVersion, "schemaId": harnessprotocol.SchemaID,
		"commandId": commandID, "kind": kind, "target": target, "expected": expected, "payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func cursorNodeReceipt(t *testing.T, result node.Result, expectedStatus int) harnessprotocol.Receipt {
	t.Helper()
	if result.HTTPStatus != expectedStatus {
		t.Fatalf("command status = %d, want %d: %s", result.HTTPStatus, expectedStatus, result.Body)
	}
	var receipt harnessprotocol.Receipt
	if err := json.Unmarshal(result.Body, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func waitCursorNodeEvent(t *testing.T, ctx context.Context, opened *node.Node, trust node.TrustContext, attemptID, eventType string) harnessprotocol.EventEnvelope {
	t.Helper()
	for ctx.Err() == nil {
		result := opened.AttemptEvents(ctx, trust, attemptID, 0, 100)
		if result.HTTPStatus == 200 {
			var page struct {
				Items []harnessprotocol.EventEnvelope `json:"items"`
			}
			if json.Unmarshal(result.Body, &page) == nil {
				for _, event := range page.Items {
					if event.Type == eventType {
						return event
					}
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %s was not observed", eventType)
	return harnessprotocol.EventEnvelope{}
}
