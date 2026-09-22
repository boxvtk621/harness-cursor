package node_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tests/fixture"
)

type authGate struct{ ready atomic.Bool }

func (gate *authGate) ProviderAuthReady() bool { return gate.ready.Load() }

type providerAuthSpace struct{}

func (providerAuthSpace) Measure(string) (node.SpaceInfo, error) {
	return node.SpaceInfo{FreeBytes: 16 << 30, TotalBytes: 64 << 30}, nil
}

func TestProviderAuthGateBlocksReadinessAndDispatch(t *testing.T) {
	gate := &authGate{}
	opened, err := node.Open(context.Background(), node.Config{DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1, Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: providerAuthSpace{}, ManualDispatchForTesting: true, ProviderAuth: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	dialogID := createDialog(t, context.Background(), opened, "61000000-0000-4000-8000-000000000001")
	enqueue(t, context.Background(), opened, "61000000-0000-4000-8000-000000000002", dialogID, "must wait for auth", 1)
	if result, err := opened.DispatchNext(context.Background()); err != nil || result.Outcome != "blocked" || result.AttemptID != "" {
		t.Fatalf("dispatch=%+v err=%v", result, err)
	}
	result := opened.HealthReady(context.Background(), nodeTrust())
	var health harnessprotocol.HealthReady
	if result.HTTPStatus != 200 || json.Unmarshal(result.Body, &health) != nil || health.Readiness != "blocked" || !containsString(health.BlockedReasons, "auth_unavailable") {
		t.Fatalf("health=%s", result.Body)
	}
	gate.ready.Store(true)
	if dispatched, err := opened.DispatchNext(context.Background()); err != nil || dispatched.AttemptID == "" {
		t.Fatalf("authorized dispatch=%+v err=%v", dispatched, err)
	}
}

func TestAuthTransitionCannotOvertakeNativeStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gate := &authGate{}
	gate.ready.Store(true)
	startGate := make(chan struct{})
	startCalled := make(chan struct{}, 1)
	adapter := fixture.NewAdapter()
	adapter.StartGate = startGate
	adapter.StartCalled = startCalled
	opened, err := node.Open(ctx, node.Config{DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1, Adapter: adapter, Policies: fixture.NewPolicySource(), Space: providerAuthSpace{}, ManualDispatchForTesting: true, ProviderAuth: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	dialogID := createDialog(t, ctx, opened, "61000000-0000-4000-8000-000000000011")
	enqueue(t, ctx, opened, "61000000-0000-4000-8000-000000000012", dialogID, "serialize auth", 1)
	if dispatched, err := opened.DispatchNext(ctx); err != nil || dispatched.AttemptID == "" {
		t.Fatalf("dispatch=%+v err=%v", dispatched, err)
	}
	select {
	case <-startCalled:
	case <-ctx.Done():
		t.Fatal("native start was not reached")
	}
	type transitionResult struct {
		release func()
		busy    bool
	}
	transition := make(chan transitionResult, 1)
	go func() {
		release, busy := opened.BeginProviderAuthTransition()
		transition <- transitionResult{release: release, busy: busy}
	}()
	select {
	case result := <-transition:
		result.release()
		t.Fatal("auth transition overtook an in-flight native start")
	case <-time.After(20 * time.Millisecond):
	}
	close(startGate)
	select {
	case result := <-transition:
		defer result.release()
		if !result.busy {
			t.Fatal("transition did not observe the durable active attempt")
		}
	case <-ctx.Done():
		t.Fatal("auth transition did not resume after native start")
	}
}

func TestAuthTransitionBlocksDispatchCommitWhenReadinessCloses(t *testing.T) {
	gate := &authGate{}
	gate.ready.Store(true)
	opened, err := node.Open(context.Background(), node.Config{DataDir: t.TempDir(), NodeID: testNodeID, OwnerID: "1-1", RegistryVersion: 1, Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: providerAuthSpace{}, ManualDispatchForTesting: true, ProviderAuth: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	dialogID := createDialog(t, context.Background(), opened, "61000000-0000-4000-8000-000000000021")
	enqueue(t, context.Background(), opened, "61000000-0000-4000-8000-000000000022", dialogID, "must not commit", 1)
	release, busy := opened.BeginProviderAuthTransition()
	if busy {
		release()
		t.Fatal("fresh node unexpectedly busy")
	}
	dispatched := make(chan node.DispatchResult, 1)
	go func() {
		result, _ := opened.DispatchNext(context.Background())
		dispatched <- result
	}()
	select {
	case <-dispatched:
		release()
		t.Fatal("dispatch crossed auth transition barrier")
	case <-time.After(20 * time.Millisecond):
	}
	gate.ready.Store(false)
	release()
	if result := <-dispatched; result.Outcome != "blocked" || result.AttemptID != "" {
		t.Fatalf("dispatch committed after auth closed: %+v", result)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
