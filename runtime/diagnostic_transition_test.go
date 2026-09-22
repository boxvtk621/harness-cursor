package node_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/internal/diagnosticlog"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tests/fixture"
)

func TestTerminalDiagnosticsAreExactlyOnceAndPostCommit(t *testing.T) {
	tests := []struct {
		name      string
		event     func(harnessadapter.AttemptRef) harnessadapter.Event
		wantEvent string
		rollback  bool
	}{
		{name: "terminal", wantEvent: `"event":"attempt.terminal"`, event: func(reference harnessadapter.AttemptRef) harnessadapter.Event {
			return harnessadapter.TerminalEvent{EventBase: harnessadapter.EventBase{Attempt: reference}, Outcome: harnessadapter.ReconcileCompleted, EffectStatus: "known"}
		}},
		{name: "unknown", wantEvent: `"event":"attempt.unknown"`, event: func(reference harnessadapter.AttemptRef) harnessadapter.Event {
			return harnessadapter.UnknownEvent{EventBase: harnessadapter.EventBase{Attempt: reference}, Reason: "provider_state", EffectStatus: "unknown"}
		}},
		{name: "rollback", wantEvent: `"event":"attempt.terminal"`, rollback: true, event: func(reference harnessadapter.AttemptRef) harnessadapter.Event {
			return harnessadapter.TerminalEvent{EventBase: harnessadapter.EventBase{Attempt: reference}, Outcome: harnessadapter.ReconcileCompleted, EffectStatus: "known"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := diagnosticlog.New(&output)
			config := testConfig(t.TempDir())
			config.Adapter = fixture.NewAdapter()
			config.Policies = fixture.NewPolicySource()
			config.Diagnostics = logger
			logger.SetNodeID(config.NodeID)
			opened, reference := runningAttemptWithConfig(t, config)
			if test.rollback {
				opened.SetFaultInjector(func(point node.FaultPoint) error {
					if point == node.FaultBeforeCommit {
						return errors.New("fixture rollback")
					}
					return nil
				})
			}
			err := opened.ObserveAdapterEvent(context.Background(), reference, test.event(reference))
			if test.rollback && err == nil {
				t.Fatal("rollback event unexpectedly committed")
			}
			if !test.rollback && err != nil {
				t.Fatal(err)
			}
			opened.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := logger.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			want := 1
			if test.rollback {
				want = 0
			}
			if got := strings.Count(output.String(), test.wantEvent); got != want {
				t.Fatalf("event count = %d, want %d; logs=%s", got, want, output.String())
			}
		})
	}
}
