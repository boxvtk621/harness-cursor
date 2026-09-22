// Package providerauth owns the provider-neutral authentication workflow.
package providerauth

import (
	"context"
	"net/http"
)

const SchemaID = "harness-provider-auth-v1"

type State string

const (
	StateUnknown                  State = "unknown"
	StateUnauthenticated          State = "unauthenticated"
	StateAuthenticated            State = "authenticated"
	StateReauthenticationRequired State = "reauthentication_required"
)

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationCancelled OperationStatus = "cancelled"
	OperationExpired   OperationStatus = "expired"
)

type Capabilities struct {
	Methods   []string `json:"methods"`
	CanCheck  bool     `json:"canCheck"`
	CanLogout bool     `json:"canLogout"`
}

type Operation struct {
	OperationID     string          `json:"operationId"`
	CommandID       string          `json:"commandId"`
	Method          string          `json:"method"`
	Status          OperationStatus `json:"status"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	ReasonCode      *string         `json:"reasonCode"`
	VerificationURL *string         `json:"verificationUrl"`
	UserCode        *string         `json:"userCode"`
	ExpiresAt       *string         `json:"expiresAt"`
	TimeoutAt       *string         `json:"timeoutAt"`
}

type Envelope struct {
	SchemaID     string       `json:"schemaId"`
	NodeID       string       `json:"nodeId"`
	Revision     int64        `json:"revision"`
	State        State        `json:"state"`
	CheckedAt    *string      `json:"checkedAt"`
	ReasonCode   *string      `json:"reasonCode"`
	Capabilities Capabilities `json:"capabilities"`
	Operation    *Operation   `json:"operation"`
}

type StartRequest struct {
	NodeID    string `json:"nodeId"`
	CommandID string `json:"commandId"`
	Method    string `json:"method"`
	Secret    string `json:"secret"`
}

type CommandRequest struct {
	NodeID    string `json:"nodeId"`
	CommandID string `json:"commandId"`
}

type APIError struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
}

func Error(status int, code string) *APIError { return &APIError{Status: status, Code: code} }

var (
	ErrInvalidRequest      = Error(http.StatusBadRequest, "invalid_request")
	ErrNotFound            = Error(http.StatusNotFound, "not_found")
	ErrPendingOperation    = Error(http.StatusConflict, "pending_operation")
	ErrBusy                = Error(http.StatusConflict, "busy")
	ErrIDConflict          = Error(http.StatusConflict, "id_conflict")
	ErrUnsupportedMethod   = Error(http.StatusUnprocessableEntity, "unsupported_method")
	ErrInvalidSecret       = Error(http.StatusUnprocessableEntity, "invalid_secret")
	ErrProviderUnavailable = Error(http.StatusServiceUnavailable, "provider_unavailable")
)

type Service interface {
	Snapshot(context.Context, string) (Envelope, *APIError)
	Operation(context.Context, string, string) (Envelope, *APIError)
	Start(context.Context, StartRequest) (Envelope, *APIError)
	Check(context.Context, CommandRequest) (Envelope, *APIError)
	Cancel(context.Context, string, CommandRequest) (Envelope, *APIError)
	Logout(context.Context, CommandRequest) (Envelope, *APIError)
}

type ReadinessGate interface {
	ProviderAuthReady() bool
}
