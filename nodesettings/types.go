package nodesettings

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

const SchemaID = "harness-node-settings-v1"

type Inference struct {
	ModelID         *string `json:"modelId"`
	SpeedMode       *string `json:"speedMode"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

type MCPAuth struct {
	Kind                  string `json:"kind"`
	BearerTokenConfigured bool   `json:"bearerTokenConfigured"`
	SecretAction          string `json:"secretAction,omitempty"`
	Secret                string `json:"secret,omitempty"`
}

func (auth *MCPAuth) UnmarshalJSON(raw []byte) error {
	var input struct {
		Kind         string `json:"kind"`
		SecretAction string `json:"secretAction"`
		Secret       string `json:"secret,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return io.ErrUnexpectedEOF
	}
	*auth = MCPAuth{Kind: input.Kind, SecretAction: input.SecretAction, Secret: input.Secret}
	return nil
}

type MCPServer struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Enabled   bool    `json:"enabled"`
	Transport string  `json:"transport"`
	URL       string  `json:"url"`
	TimeoutMS int     `json:"timeoutMs"`
	Auth      MCPAuth `json:"auth"`
}

type Snapshot struct {
	MCPServers []MCPServer `json:"mcpServers"`
	Inference  Inference   `json:"inference"`
}

type Capabilities struct {
	Provider      string `json:"provider"`
	ModelCatalog  string `json:"modelCatalog"`
	ModelDefault  string `json:"modelDefault"`
	MCPCheck      string `json:"mcpCheck"`
	MCPTimeout    string `json:"mcpTimeout"`
	NativeRestart string `json:"nativeRestart"`
}

type Observation struct {
	Kind       string `json:"kind"`
	State      string `json:"state"`
	ReasonCode string `json:"reasonCode,omitempty"`
}

type Operation struct {
	OperationID      string `json:"operationId"`
	CommandID        string `json:"commandId"`
	TargetRevision   int64  `json:"targetRevision"`
	PreviousRevision int64  `json:"previousRevision"`
	Status           string `json:"status"`
	Phase            string `json:"phase"`
	ReasonCode       string `json:"reasonCode,omitempty"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

type Envelope struct {
	SchemaID        string        `json:"schemaId"`
	NodeID          string        `json:"nodeId"`
	DraftRevision   int64         `json:"draftRevision"`
	AppliedRevision int64         `json:"appliedRevision"`
	Draft           Snapshot      `json:"draft"`
	Applied         Snapshot      `json:"applied"`
	Capabilities    Capabilities  `json:"capabilities"`
	Operation       *Operation    `json:"operation,omitempty"`
	Observations    []Observation `json:"observations,omitempty"`
}

type PutRequest struct {
	ExpectedRevision int64    `json:"expectedRevision"`
	Draft            Snapshot `json:"draft"`
}

type CommandRequest struct {
	CommandID        string `json:"commandId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	TargetRevision   int64  `json:"targetRevision"`
}

type MCPCheckRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	MCPServerID      string `json:"mcpServerId"`
}

type MCPCheck struct {
	SchemaID    string `json:"schemaId"`
	NodeID      string `json:"nodeId"`
	MCPServerID string `json:"mcpServerId"`
	CheckedAt   string `json:"checkedAt"`
	State       string `json:"state"`
	ReasonCode  string `json:"reasonCode,omitempty"`
	ToolsCount  *int   `json:"toolsCount,omitempty"`
}

type Mode struct {
	ID        string `json:"id"`
	IsDefault bool   `json:"isDefault,omitempty"`
}

type Model struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ToolCalling      *bool  `json:"toolCalling,omitempty"`
	ReasoningEfforts []Mode `json:"reasoningEfforts"`
	SpeedModes       []Mode `json:"speedModes"`
}

type Catalog struct {
	SchemaID        string  `json:"schemaId"`
	NodeID          string  `json:"nodeId"`
	CatalogRevision string  `json:"catalogRevision"`
	RuntimeVersion  string  `json:"runtimeVersion"`
	FetchedAt       string  `json:"fetchedAt"`
	State           string  `json:"state"`
	Models          []Model `json:"models"`
	NextCursor      string  `json:"nextCursor,omitempty"`
	ReasonCode      string  `json:"reasonCode,omitempty"`
}

type NativeParameter struct {
	ID     string
	Values []string
}

type NativeVariant struct {
	Params    map[string]string
	IsDefault bool
}

type NativeModel struct {
	ID          string
	DisplayName string
	Parameters  []NativeParameter
	Variants    []NativeVariant
}

// RuntimeConfig is the private, node-wide snapshot supplied to the native
// child process. It must never be returned by a public API.
type RuntimeConfig struct {
	ModelID    string
	Params     map[string]string
	MCPServers []RuntimeMCPServer
}

type RuntimeMCPServer struct {
	ID, URL, BearerToken string
}

type RuntimeApplier interface {
	RestartSettings(context.Context, RuntimeConfig) (rollbackFailed bool, err error)
}

type CatalogSource interface {
	Models(context.Context) ([]NativeModel, string, error)
}

type APIError struct {
	Status int
	Code   string
}

type Service interface {
	Snapshot(context.Context, string) (Envelope, *APIError)
	PutDraft(context.Context, string, PutRequest) (Envelope, *APIError)
	ModelCatalog(context.Context, string) (Catalog, *APIError)
	CheckMCP(context.Context, string, MCPCheckRequest) (MCPCheck, *APIError)
	Apply(context.Context, string, CommandRequest) (Envelope, *APIError)
	Operation(context.Context, string, string) (Envelope, *APIError)
}
