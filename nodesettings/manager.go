package nodesettings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maximumServers = 50

var (
	identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type privateMCP struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Transport   string `json:"transport"`
	URL         string `json:"url"`
	TimeoutMS   int    `json:"timeoutMs"`
	BearerToken string `json:"bearerToken,omitempty"`
}

type privateSnapshot struct {
	MCPServers []privateMCP `json:"mcpServers"`
	Inference  Inference    `json:"inference"`
}

type operationRecord struct {
	Operation        Operation `json:"operation"`
	ExpectedRevision int64     `json:"expectedRevision"`
	TargetRevision   int64     `json:"targetRevision"`
}

type diskState struct {
	DraftRevision   int64                      `json:"draftRevision"`
	AppliedRevision int64                      `json:"appliedRevision"`
	Draft           privateSnapshot            `json:"draft"`
	Applied         privateSnapshot            `json:"applied"`
	Operations      map[string]operationRecord `json:"operations"`
}

type Manager struct {
	nodeID, path, runtimeVersion string
	catalogSource                CatalogSource
	mu                           sync.Mutex
	state                        diskState
	now                          func() time.Time
}

func Open(nodeID, stateDir, runtimeVersion, initialModel string, source CatalogSource) (*Manager, error) {
	if nodeID == "" || !filepath.IsAbs(stateDir) || initialModel == "" {
		return nil, errors.New("node settings config is incomplete")
	}
	manager := &Manager{nodeID: nodeID, path: filepath.Join(stateDir, "node-settings.json"), runtimeVersion: runtimeVersion, catalogSource: source, now: time.Now}
	manager.state = diskState{DraftRevision: 1, AppliedRevision: 1, Operations: make(map[string]operationRecord)}
	draftModel, appliedModel := initialModel, initialModel
	manager.state.Draft.Inference.ModelID = &draftModel
	manager.state.Applied.Inference.ModelID = &appliedModel
	if raw, err := os.ReadFile(manager.path); err == nil {
		var loaded diskState
		if len(raw) > 1<<20 || decodeDiskState(raw, &loaded) != nil || !validDiskState(loaded) {
			return nil, errors.New("node settings state is invalid")
		}
		manager.state = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err := manager.persistLocked(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (manager *Manager) Snapshot(ctx context.Context, nodeID string) (Envelope, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID {
		return Envelope{}, notFound()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.envelopeLocked(nil), nil
}

func (manager *Manager) PutDraft(ctx context.Context, nodeID string, request PutRequest) (Envelope, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID {
		return Envelope{}, notFound()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if request.ExpectedRevision != manager.state.DraftRevision {
		return Envelope{}, &APIError{Status: http.StatusConflict, Code: "revision_conflict"}
	}
	draft, issue := validateDraft(request.Draft, manager.state.Draft)
	if issue != nil {
		return Envelope{}, issue
	}
	previous := manager.state.Draft
	previousRevision := manager.state.DraftRevision
	manager.state.DraftRevision++
	manager.state.Draft = draft
	if err := manager.persistLocked(); err != nil {
		manager.state.Draft, manager.state.DraftRevision = previous, previousRevision
		return Envelope{}, unavailable()
	}
	return manager.envelopeLocked(nil), nil
}

func (manager *Manager) ModelCatalog(ctx context.Context, nodeID string) (Catalog, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID {
		return Catalog{}, notFound()
	}
	stamp := manager.now().UTC().Format(time.RFC3339Nano)
	catalog := Catalog{SchemaID: SchemaID, NodeID: nodeID, RuntimeVersion: manager.runtimeVersion, FetchedAt: stamp, State: "unavailable", Models: []Model{}}
	if manager.catalogSource == nil {
		catalog.ReasonCode = "provider_auth_required"
		return catalog, nil
	}
	native, revision, err := manager.catalogSource.Models(ctx)
	if err != nil {
		catalog.ReasonCode = "catalog_unavailable"
		return catalog, nil
	}
	catalog.State, catalog.CatalogRevision = "fresh", revision
	for _, item := range native {
		model := Model{ID: item.ID, DisplayName: item.DisplayName, ReasoningEfforts: []Mode{}, SpeedModes: []Mode{}}
		catalog.Models = append(catalog.Models, model)
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ID < catalog.Models[j].ID })
	return catalog, nil
}

func (manager *Manager) CheckMCP(ctx context.Context, nodeID string, request MCPCheckRequest) (MCPCheck, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID || !identifierPattern.MatchString(request.MCPServerID) {
		return MCPCheck{}, &APIError{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if request.ExpectedRevision != manager.state.DraftRevision {
		return MCPCheck{}, &APIError{Status: http.StatusConflict, Code: "revision_conflict"}
	}
	found := false
	for _, server := range manager.state.Draft.MCPServers {
		found = found || server.ID == request.MCPServerID
	}
	if !found {
		return MCPCheck{}, notFound()
	}
	return MCPCheck{SchemaID: SchemaID, NodeID: nodeID, MCPServerID: request.MCPServerID, CheckedAt: manager.now().UTC().Format(time.RFC3339Nano), State: "unsupported", ReasonCode: "zero_turn_mcp_check_unavailable"}, nil
}

func (manager *Manager) Apply(ctx context.Context, nodeID string, request CommandRequest) (Envelope, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID || !identifierPattern.MatchString(request.CommandID) {
		return Envelope{}, &APIError{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if existing, ok := manager.state.Operations[request.CommandID]; ok {
		if existing.ExpectedRevision != request.ExpectedRevision || existing.TargetRevision != request.TargetRevision {
			return Envelope{}, &APIError{Status: http.StatusConflict, Code: "command_conflict"}
		}
		operation := existing.Operation
		return manager.envelopeLocked(&operation), nil
	}
	if request.ExpectedRevision != manager.state.DraftRevision || request.TargetRevision != manager.state.DraftRevision {
		return Envelope{}, &APIError{Status: http.StatusConflict, Code: "revision_conflict"}
	}
	now := manager.now().UTC().Format(time.RFC3339Nano)
	operation := Operation{OperationID: randomID(), CommandID: request.CommandID, TargetRevision: manager.state.DraftRevision, PreviousRevision: manager.state.AppliedRevision, Status: "failed", Phase: "preflight", ReasonCode: "managed_restart_coordination_unavailable", CreatedAt: now, UpdatedAt: now}
	manager.state.Operations[request.CommandID] = operationRecord{Operation: operation, ExpectedRevision: request.ExpectedRevision, TargetRevision: request.TargetRevision}
	if err := manager.persistLocked(); err != nil {
		delete(manager.state.Operations, request.CommandID)
		return Envelope{}, unavailable()
	}
	return manager.envelopeLocked(&operation), nil
}

func (manager *Manager) Operation(ctx context.Context, nodeID, operationID string) (Envelope, *APIError) {
	if ctx.Err() != nil || nodeID != manager.nodeID || operationID == "" {
		return Envelope{}, notFound()
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, record := range manager.state.Operations {
		if record.Operation.OperationID == operationID {
			copy := record.Operation
			return manager.envelopeLocked(&copy), nil
		}
	}
	return Envelope{}, notFound()
}

func decodeDiskState(raw []byte, target *diskState) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("multiple node settings state values")
	}
	return nil
}

func validDiskState(state diskState) bool {
	if state.DraftRevision < 1 || state.AppliedRevision < 1 || state.AppliedRevision > state.DraftRevision ||
		state.Operations == nil || len(state.Operations) > 4096 || !validPrivateSnapshot(state.Draft) || !validPrivateSnapshot(state.Applied) {
		return false
	}
	for commandID, record := range state.Operations {
		operation := record.Operation
		if !identifierPattern.MatchString(commandID) || operation.CommandID != commandID || !uuidPattern.MatchString(operation.OperationID) ||
			record.ExpectedRevision < 1 || record.TargetRevision < 1 || operation.TargetRevision != record.TargetRevision ||
			operation.PreviousRevision < 1 || !validOperationStatus(operation.Status) || !validOperationPhase(operation.Phase) ||
			operation.ReasonCode != "" && !identifierPattern.MatchString(operation.ReasonCode) ||
			operation.CreatedAt == "" || operation.UpdatedAt == "" {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, operation.CreatedAt); err != nil {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, operation.UpdatedAt); err != nil {
			return false
		}
	}
	return true
}

func validOperationStatus(value string) bool {
	switch value {
	case "pending", "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func validOperationPhase(value string) bool {
	switch value {
	case "preflight", "draining", "restarting", "verifying", "complete":
		return true
	default:
		return false
	}
}

func validPrivateSnapshot(snapshot privateSnapshot) bool {
	if snapshot.Inference.ModelID != nil && !validText(*snapshot.Inference.ModelID, 200) || len(snapshot.MCPServers) > maximumServers ||
		snapshot.Inference.SpeedMode != nil && !validText(*snapshot.Inference.SpeedMode, 100) ||
		snapshot.Inference.ReasoningEffort != nil && !validText(*snapshot.Inference.ReasoningEffort, 100) {
		return false
	}
	seen := make(map[string]bool, len(snapshot.MCPServers))
	for _, item := range snapshot.MCPServers {
		if seen[item.ID] || !validPrivateMCP(item) {
			return false
		}
		seen[item.ID] = true
	}
	return true
}

func validPrivateMCP(item privateMCP) bool {
	if !identifierPattern.MatchString(item.ID) || !validText(item.Name, 200) || item.Transport != "streamable_http" ||
		item.TimeoutMS < 100 || item.TimeoutMS > 120000 || len(item.BearerToken) > 16<<10 || strings.ContainsAny(item.BearerToken, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(item.URL)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validateDraft(input Snapshot, previous privateSnapshot) (privateSnapshot, *APIError) {
	if input.Inference.ModelID != nil && !validText(*input.Inference.ModelID, 200) || len(input.MCPServers) > maximumServers ||
		input.Inference.SpeedMode != nil && !validText(*input.Inference.SpeedMode, 100) ||
		input.Inference.ReasoningEffort != nil && !validText(*input.Inference.ReasoningEffort, 100) {
		return privateSnapshot{}, invalid()
	}
	prior := make(map[string]privateMCP, len(previous.MCPServers))
	for _, item := range previous.MCPServers {
		prior[item.ID] = item
	}
	result := privateSnapshot{Inference: input.Inference, MCPServers: make([]privateMCP, 0, len(input.MCPServers))}
	seen := make(map[string]bool, len(input.MCPServers))
	for _, item := range input.MCPServers {
		if seen[item.ID] || !identifierPattern.MatchString(item.ID) || !validText(item.Name, 200) || item.Transport != "streamable_http" || item.TimeoutMS < 100 || item.TimeoutMS > 120000 {
			return privateSnapshot{}, invalid()
		}
		parsed, err := url.Parse(item.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return privateSnapshot{}, invalid()
		}
		secret := prior[item.ID].BearerToken
		switch item.Auth.Kind {
		case "none":
			if item.Auth.Secret != "" || item.Auth.SecretAction != "remove" {
				return privateSnapshot{}, invalid()
			}
			secret = ""
		case "bearer":
			switch item.Auth.SecretAction {
			case "keep":
				if secret == "" || item.Auth.Secret != "" {
					return privateSnapshot{}, invalid()
				}
			case "replace":
				if !validText(item.Auth.Secret, 16<<10) || strings.ContainsAny(item.Auth.Secret, "\r\n") {
					return privateSnapshot{}, invalid()
				}
				secret = item.Auth.Secret
			default:
				return privateSnapshot{}, invalid()
			}
		default:
			return privateSnapshot{}, invalid()
		}
		seen[item.ID] = true
		result.MCPServers = append(result.MCPServers, privateMCP{ID: item.ID, Name: item.Name, Enabled: item.Enabled, Transport: item.Transport, URL: item.URL, TimeoutMS: item.TimeoutMS, BearerToken: secret})
	}
	sort.Slice(result.MCPServers, func(i, j int) bool { return result.MCPServers[i].ID < result.MCPServers[j].ID })
	return result, nil
}

func (manager *Manager) envelopeLocked(operation *Operation) Envelope {
	state := "unavailable"
	if manager.catalogSource != nil {
		state = "available"
	}
	return Envelope{SchemaID: SchemaID, NodeID: manager.nodeID, DraftRevision: manager.state.DraftRevision, AppliedRevision: manager.state.AppliedRevision, Draft: publicSnapshot(manager.state.Draft), Applied: publicSnapshot(manager.state.Applied), Capabilities: Capabilities{Provider: "cursor", ModelCatalog: state, MCPCheck: "unsupported", NativeRestart: "unsupported"}, Operation: operation}
}

func publicSnapshot(input privateSnapshot) Snapshot {
	output := Snapshot{Inference: input.Inference, MCPServers: make([]MCPServer, 0, len(input.MCPServers))}
	for _, item := range input.MCPServers {
		kind := "none"
		if item.BearerToken != "" {
			kind = "bearer"
		}
		output.MCPServers = append(output.MCPServers, MCPServer{ID: item.ID, Name: item.Name, Enabled: item.Enabled, Transport: item.Transport, URL: item.URL, TimeoutMS: item.TimeoutMS, Auth: MCPAuth{Kind: kind, BearerTokenConfigured: item.BearerToken != ""}})
	}
	return output
}

func (manager *Manager) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(manager.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(manager.state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(manager.path), ".node-settings-*")
	if err != nil {
		return err
	}
	name, committed := temporary.Name(), false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, manager.path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(manager.path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	_ = directory.Close()
	committed = err == nil
	return err
}

func randomID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}

func invalid() *APIError  { return &APIError{Status: http.StatusBadRequest, Code: "invalid_request"} }
func notFound() *APIError { return &APIError{Status: http.StatusNotFound, Code: "not_found"} }
func unavailable() *APIError {
	return &APIError{Status: http.StatusServiceUnavailable, Code: "settings_unavailable"}
}
