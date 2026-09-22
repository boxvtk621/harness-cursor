package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/internal/diagnosticlog"
	"github.com/boxvtk621/harness-cursor/providerauth"
	node "github.com/boxvtk621/harness-cursor/runtime"
)

func TestDiagnosticAuthAndReadinessLogTransitionsWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := diagnosticlog.New(&output)
	logger.SetNodeID("10000000-0000-4000-8000-000000000001")
	server := &Server{config: Config{NodeID: "10000000-0000-4000-8000-000000000001", Diagnostics: logger}}
	secretURL, userCode := "https://provider.invalid/?secret=canary", "SECRET-CANARY"
	envelope := providerauth.Envelope{NodeID: server.config.NodeID, State: providerauth.StateUnauthenticated, Operation: &providerauth.Operation{
		OperationID: "20000000-0000-4000-8000-000000000002", CommandID: "30000000-0000-4000-8000-000000000003",
		Method: "secret", Status: providerauth.OperationPending, VerificationURL: &secretURL, UserCode: &userCode,
	}}
	server.logAuthEnvelope(envelope, nil, "operation", "", envelope.Operation.OperationID)
	snapshot := envelope
	snapshot.Operation = nil
	server.logAuthEnvelope(snapshot, nil, "snapshot", "", "")
	server.logAuthEnvelope(envelope, nil, "operation", "", envelope.Operation.OperationID)
	for index := 0; index < 65; index++ {
		other := envelope
		operation := *envelope.Operation
		operation.OperationID = fmt.Sprintf("operation-%d", index)
		other.Operation = &operation
		server.logAuthEnvelope(other, nil, "operation", "", operation.OperationID)
	}
	server.logAuthEnvelope(envelope, nil, "operation", "", envelope.Operation.OperationID)
	envelope.Operation.Status = providerauth.OperationSucceeded
	server.logAuthEnvelope(envelope, nil, "operation", "", envelope.Operation.OperationID)
	health, _ := json.Marshal(harnessprotocol.HealthReady{Readiness: "blocked", BlockedReasons: []string{"auth_unavailable"}})
	server.logReadiness(node.Result{HTTPStatus: 503, Body: health})
	server.logReadiness(node.Result{HTTPStatus: 503, Body: health})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	if strings.Count(logged, `"event":"provider.auth_started"`) != 66 || strings.Count(logged, `"event":"readiness.changed"`) != 1 ||
		!strings.Contains(logged, `"event":"provider.auth_completed"`) {
		t.Fatalf("unexpected transitions: %s", logged)
	}
	if strings.Contains(logged, "SECRET-CANARY") || strings.Contains(logged, "provider.invalid") {
		t.Fatalf("secret escaped: %s", logged)
	}
}
