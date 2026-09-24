package nodesettings

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

const SchemaID = "harness-node-settings-v2"
const ModelCatalogSchemaID = "harness-model-catalog-v2"
const MCPDocumentSchemaID = "harness-mcp-document-v2"

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
		Kind                  string `json:"kind"`
		SecretAction          string `json:"secretAction"`
		Secret                string `json:"secret,omitempty"`
		BearerTokenConfigured *bool  `json:"bearerTokenConfigured,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return io.ErrUnexpectedEOF
	}
	action := input.SecretAction
	if action == "" {
		if input.Kind == "none" {
			action = "remove"
		}
		if input.Kind == "bearer" && input.BearerTokenConfigured != nil && *input.BearerTokenConfigured {
			action = "keep"
		}
	}
	*auth = MCPAuth{Kind: input.Kind, SecretAction: action, Secret: input.Secret}
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

type MCPDocument struct {
	SchemaID string      `json:"schemaId"`
	Servers  []MCPServer `json:"servers"`
}

type Snapshot struct {
	Inference   Inference   `json:"inference"`
	MCPDocument MCPDocument `json:"mcpDocument"`
	// MCPServers accepts the v1 request shape during migration.
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
}

type Capabilities struct {
	Provider         string   `json:"provider"`
	ModelCatalog     string   `json:"modelCatalog"`
	ModelDefault     string   `json:"modelDefault"`
	SpeedDefault     string   `json:"speedDefault"`
	ReasoningDefault string   `json:"reasoningDefault"`
	MCPCheck         string   `json:"mcpCheck"`
	MCPTimeout       string   `json:"mcpTimeout"`
	NativeRestart    string   `json:"nativeRestart"`
	MCPSchema        string   `json:"mcpSchema"`
	MCPTransports    []string `json:"mcpTransports"`
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
	ID               string        `json:"id"`
	DisplayName      string        `json:"displayName"`
	ToolCalling      *bool         `json:"toolCalling,omitempty"`
	ReasoningEfforts []Mode        `json:"reasoningEfforts"`
	SpeedModes       []Mode        `json:"speedModes"`
	Combinations     []Combination `json:"combinations"`
}

type Combination struct {
	SpeedMode       *string `json:"speedMode"`
	ReasoningEffort *string `json:"reasoningEffort"`
	IsDefault       bool    `json:"isDefault,omitempty"`
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
	ID, Transport, URL, BearerToken string
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
	Path   string
}

type ValidationError struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

type MCPValidation struct {
	SchemaID string            `json:"schemaId"`
	Valid    bool              `json:"valid"`
	Errors   []ValidationError `json:"errors"`
}

type Service interface {
	Snapshot(context.Context, string) (Envelope, *APIError)
	PutDraft(context.Context, string, PutRequest) (Envelope, *APIError)
	ModelCatalog(context.Context, string) (Catalog, *APIError)
	CheckMCP(context.Context, string, MCPCheckRequest) (MCPCheck, *APIError)
	Apply(context.Context, string, CommandRequest) (Envelope, *APIError)
	Operation(context.Context, string, string) (Envelope, *APIError)
}
