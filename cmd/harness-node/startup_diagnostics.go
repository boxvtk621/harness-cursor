package main

import "errors"

// startupFailure records only a fixed stage identifier for diagnostics. The
// underlying error remains available to control flow but is never serialized.
type startupFailure struct {
	stage string
	err   error
}

func (failure *startupFailure) Error() string { return failure.err.Error() }
func (failure *startupFailure) Unwrap() error { return failure.err }

func withStartupStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &startupFailure{stage: stage, err: err}
}

func startupFailureReason(err error) string {
	var failure *startupFailure
	if !errors.As(err, &failure) {
		return "start_or_serve_failed"
	}
	switch failure.stage {
	case "config_load_failed", "config_validate_failed", "listen_config_invalid",
		"tls_certificate_load_failed", "policy_load_failed", "provider_runtime_open_failed",
		"provider_auth_refresh_failed", "runtime_open_failed", "node_settings_open_failed",
		"api_handler_open_failed", "server_listen_failed":
		return failure.stage
	default:
		return "start_or_serve_failed"
	}
}
