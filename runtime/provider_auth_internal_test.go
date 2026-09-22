package node

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/tests/fixture"
)

type readyAuthGate struct{ ready atomic.Bool }

func (gate *readyAuthGate) ProviderAuthReady() bool { return gate.ready.Load() }

func TestHealthReadyUsesCanonicalEmptyBlockedReasons(t *testing.T) {
	ctx := context.Background()
	const nodeID = "20000000-0000-4000-8000-000000000001"
	gate := &readyAuthGate{}
	gate.ready.Store(true)
	opened, err := Open(ctx, Config{DataDir: t.TempDir(), NodeID: nodeID, OwnerID: "1-1", RegistryVersion: 1,
		Adapter: fixture.NewAdapter(), Policies: fixture.NewPolicySource(), Space: fullSpace{}, ManualDispatchForTesting: true, ProviderAuth: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.db.ExecContext(ctx, `UPDATE node_state SET engine_readiness='ready', blocked_reasons='[]' WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	result := opened.HealthReady(ctx, TrustContext{TransportNodeID: nodeID})
	if result.HTTPStatus != http.StatusOK || !strings.Contains(string(result.Body), `"blockedReasons":[]`) {
		t.Fatalf("ready=%d %s", result.HTTPStatus, result.Body)
	}
	var health harnessprotocol.HealthReady
	if json.Unmarshal(result.Body, &health) != nil || health.Readiness != "ready" || health.BlockedReasons == nil || len(health.BlockedReasons) != 0 {
		t.Fatalf("invalid health ready: %+v", health)
	}
}
