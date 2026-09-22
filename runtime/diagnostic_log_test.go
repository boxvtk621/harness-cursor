package node

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/harness-cursor/internal/diagnosticlog"
)

func TestCommandErrorLogsOnlyStableCodeAndCorrelation(t *testing.T) {
	var output bytes.Buffer
	logger := diagnosticlog.New(&output)
	logger.SetNodeID("10000000-0000-4000-8000-000000000000")
	opened := &Node{config: Config{NodeID: "10000000-0000-4000-8000-000000000000", Diagnostics: logger}}
	commandID := "10000000-0000-4000-8000-000000000001"
	result := opened.commandError(errors.New("provider secret and prompt must not escape"), commandID)
	if result.HTTPStatus != 503 {
		t.Fatalf("status = %d", result.HTTPStatus)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	if !strings.Contains(logged, `"event":"command.failed"`) || !strings.Contains(logged, commandID) ||
		!strings.Contains(logged, `"reasonCode":"durable_operation_failed"`) {
		t.Fatalf("missing safe diagnostic: %s", logged)
	}
	if strings.Contains(logged, "provider secret") || strings.Contains(logged, "prompt") {
		t.Fatalf("raw error escaped into diagnostics: %s", logged)
	}
}
