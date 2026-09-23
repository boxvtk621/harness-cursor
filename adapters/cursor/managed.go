package cursor

import (
	"context"
	"errors"
	"sync"

	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/contracts/wire"
	"github.com/boxvtk621/harness-cursor/internal/diagnosticlog"
	"github.com/boxvtk621/harness-cursor/nodesettings"
	"github.com/boxvtk621/harness-cursor/runtime"
)

// Managed keeps the node alive while provider authentication is absent and
// atomically swaps complete SDK workers only while the runtime is idle.
type Managed struct {
	config    Config
	artifacts node.ArtifactSink
	mu        sync.RWMutex
	current   *Adapter
	closed    bool
}

func NewManaged(config Config, artifacts node.ArtifactSink) (*Managed, error) {
	config.APIKey = ""
	if config.NodeExecutable == "" || config.WorkerEntrypoint == "" || config.StateDir == "" {
		return nil, errors.New("managed cursor adapter config is incomplete")
	}
	if config.Diagnostics == nil {
		config.Diagnostics = diagnosticlog.Disabled()
	}
	return &Managed{config: config, artifacts: artifacts}, nil
}

func (managed *Managed) Prepare(secret string) (*Adapter, error) {
	managed.mu.RLock()
	config := managed.config
	managed.mu.RUnlock()
	config.APIKey = secret
	return New(config, managed.artifacts)
}

func configured(base Config, settings nodesettings.RuntimeConfig) Config {
	base.Model = settings.ModelID
	base.ModelParams = make([]ModelParam, 0, len(settings.Params))
	for id, value := range settings.Params {
		base.ModelParams = append(base.ModelParams, ModelParam{ID: id, Value: value})
	}
	base.MCPServers = make(map[string]MCPServerConfig, len(settings.MCPServers))
	for _, server := range settings.MCPServers {
		entry := MCPServerConfig{Type: "http", URL: server.URL}
		if server.BearerToken != "" {
			entry.Headers = map[string]string{"Authorization": "Bearer " + server.BearerToken}
		}
		base.MCPServers[server.ID] = entry
	}
	return base
}

// ConfigureApplied is called before provider-auth bootstrap so a recreated
// container starts only the last confirmed applied snapshot.
func (managed *Managed) ConfigureApplied(settings nodesettings.RuntimeConfig) {
	managed.mu.Lock()
	managed.config = configured(managed.config, settings)
	managed.mu.Unlock()
}

// RestartSettings runs with the runtime start gate held. The old worker is
// reaped before the replacement is initialized against the persistent store.
func (managed *Managed) RestartSettings(ctx context.Context, settings nodesettings.RuntimeConfig) (bool, error) {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed || managed.current == nil {
		return false, errors.New("provider_auth_required")
	}
	oldConfig := managed.current.config
	nextConfig := configured(oldConfig, settings)
	prior := managed.current
	managed.current = nil
	_ = prior.Close() // Kill status is expected; Close has reaped the child.
	var restartErr error
	if ctx.Err() == nil {
		candidate, err := New(nextConfig, managed.artifacts)
		if err == nil {
			models, _, catalogErr := candidate.Models(ctx)
			verified := false
			for _, model := range models {
				if model.ID == settings.ModelID {
					verified = true
				}
			}
			if catalogErr == nil && verified {
				managed.current, managed.config = candidate, configured(managed.config, settings)
				return false, nil
			}
			_ = candidate.Close()
			if catalogErr != nil {
				err = catalogErr
			} else {
				err = errors.New("model_unavailable")
			}
		}
		restartErr = err
	} else {
		restartErr = ctx.Err()
	}
	// Exactly one controlled rollback attempt. Keep the node available for
	// settings reads even if the provider cannot be brought back.
	restored, rollbackErr := New(oldConfig, managed.artifacts)
	if rollbackErr != nil {
		return true, errors.Join(restartErr, rollbackErr)
	}
	managed.current = restored
	return false, restartErr
}

func (managed *Managed) Swap(replacement *Adapter) error {
	prior, err := managed.Exchange(replacement)
	if err != nil {
		if replacement != nil {
			_ = replacement.Close()
		}
		return err
	}
	if prior != nil {
		_ = prior.Close()
	}
	return nil
}

// Exchange installs a replacement without closing the prior adapter so an
// authentication transaction can still roll back until durable success.
func (managed *Managed) Exchange(replacement *Adapter) (*Adapter, error) {
	if replacement == nil {
		return nil, errors.New("cursor replacement is nil")
	}
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		return nil, errors.New("managed cursor adapter is closed")
	}
	prior := managed.current
	managed.current = replacement
	return prior, nil
}

// Restore replaces the expected provisional adapter with its predecessor.
func (managed *Managed) Restore(expected, prior *Adapter) error {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.closed {
		return errors.New("managed cursor adapter is closed")
	}
	if managed.current != expected {
		return errors.New("cursor replacement generation changed")
	}
	managed.current = prior
	return nil
}

func (managed *Managed) Deactivate() error {
	managed.mu.Lock()
	prior := managed.current
	managed.current = nil
	managed.mu.Unlock()
	if prior != nil {
		return prior.Close()
	}
	return nil
}

func (managed *Managed) adapter() (*Adapter, error) {
	managed.mu.RLock()
	defer managed.mu.RUnlock()
	if managed.closed || managed.current == nil {
		return nil, errors.New("cursor provider authentication is unavailable")
	}
	return managed.current, nil
}

func (managed *Managed) NativeReady() bool {
	managed.mu.RLock()
	defer managed.mu.RUnlock()
	return !managed.closed && managed.current != nil
}

func (managed *Managed) Models(ctx context.Context) ([]nodesettings.NativeModel, string, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return nil, "", err
	}
	return adapter.Models(ctx)
}

func (managed *Managed) Identity(context.Context) (harnessadapter.Identity, error) {
	declared, verified := map[harnessadapter.Capability]bool{}, map[harnessadapter.Capability]bool{}
	for _, capability := range []harnessadapter.Capability{harnessadapter.CapabilityChat, harnessadapter.CapabilityEvents, harnessadapter.CapabilityToolResults, harnessadapter.CapabilityCancel, harnessadapter.CapabilitySteerAttached, harnessadapter.CapabilitySessionResume, harnessadapter.CapabilityPolicyEnforcement} {
		declared[capability], verified[capability] = true, true
	}
	return harnessadapter.Identity{Kind: harnessadapter.KindCursor, Version: harnessadapter.CursorSDKVersion, ProtocolVersion: harnessprotocol.ProtocolVersion, SchemaID: harnessprotocol.SchemaID, SchemaSHA256: harnessprotocol.SchemaSHA256, Declared: declared, Verified: verified}, nil
}

func (managed *Managed) Start(ctx context.Context, input harnessadapter.StartInput) (harnessadapter.StartResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartRejected, Failure: nodeFailure("provider_auth_required", "provider authentication is required", false)}, nil
	}
	return adapter.Start(ctx, input)
}
func (managed *Managed) Resume(ctx context.Context, input harnessadapter.ResumeInput) (harnessadapter.ResumeResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeRejected, Failure: nodeFailure("provider_auth_required", "provider authentication is required", false)}, nil
	}
	return adapter.Resume(ctx, input)
}
func (managed *Managed) Events(ctx context.Context, input harnessadapter.EventsInput) (harnessadapter.EventStream, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return nil, err
	}
	return adapter.Events(ctx, input)
}
func (managed *Managed) Steer(ctx context.Context, input harnessadapter.SteerInput) (harnessadapter.SteerResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerRejected}, nil
	}
	return adapter.Steer(ctx, input)
}
func (managed *Managed) Cancel(ctx context.Context, input harnessadapter.CancelInput) (harnessadapter.CancelResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelRejected}, nil
	}
	return adapter.Cancel(ctx, input)
}
func (managed *Managed) RespondApproval(ctx context.Context, input harnessadapter.RespondApprovalInput) (harnessadapter.ResponseResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected}, nil
	}
	return adapter.RespondApproval(ctx, input)
}
func (managed *Managed) RespondInput(ctx context.Context, input harnessadapter.RespondInputInput) (harnessadapter.ResponseResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected}, nil
	}
	return adapter.RespondInput(ctx, input)
}
func (managed *Managed) Reconcile(ctx context.Context, input harnessadapter.ReconcileInput) (harnessadapter.ReconcileResult, error) {
	adapter, err := managed.adapter()
	if err != nil {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, Failure: nodeFailure("provider_auth_required", "provider authentication is required", false)}, nil
	}
	return adapter.Reconcile(ctx, input)
}

func (managed *Managed) Close() error {
	managed.mu.Lock()
	managed.closed = true
	prior := managed.current
	managed.current = nil
	managed.mu.Unlock()
	if prior != nil {
		return prior.Close()
	}
	return nil
}
