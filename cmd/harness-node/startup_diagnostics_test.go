package main

import (
	"errors"
	"testing"
)

func TestStartupFailureReasonUsesOnlyFixedStageCode(t *testing.T) {
	const sensitive = "credential-and-provider-response-must-not-appear"
	err := withStartupStage("provider_auth_refresh_failed", errors.New(sensitive))
	if got := startupFailureReason(err); got != "provider_auth_refresh_failed" {
		t.Fatalf("reason = %q", got)
	}
	if got := startupFailureReason(errors.New(sensitive)); got != "start_or_serve_failed" {
		t.Fatalf("untagged reason = %q", got)
	}
	if got := startupFailureReason(withStartupStage(sensitive, errors.New("private"))); got != "start_or_serve_failed" {
		t.Fatalf("unrecognized stage reason = %q", got)
	}
}
