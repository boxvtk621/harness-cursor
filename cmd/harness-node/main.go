package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/adapters/cursor"
	harnessserver "github.com/boxvtk621/harness-cursor/api"
	"github.com/boxvtk621/harness-cursor/internal/diagnosticlog"
	"github.com/boxvtk621/harness-cursor/nodesettings"
	"github.com/boxvtk621/harness-cursor/providerauth"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tools"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	cursorExplicitToolManifest = "[{\"name\":\"cursor.command\"},{\"name\":\"cursor.file_change\"}]\n"
	toolRunnerExecutable       = "/harness-tool-runner"
)

type config struct {
	Listen                   string        `json:"listen"`
	NodeID                   string        `json:"nodeId"`
	OwnerID                  string        `json:"ownerId"`
	DataDir                  string        `json:"dataDir"`
	RegistryVersion          int64         `json:"registryVersion"`
	CertificateFile          string        `json:"certificateFile"`
	KeyFile                  string        `json:"keyFile"`
	PolicyFile               string        `json:"policyFile"`
	ToolManifestFile         string        `json:"toolManifestFile"`
	PolicyRevision           string        `json:"policyRevision"`
	ApprovalMode             string        `json:"approvalMode,omitempty"`
	ManualDispatchForTesting bool          `json:"manualDispatchForTesting,omitempty"`
	Adapter                  string        `json:"adapter"`
	Cursor                   *cursorConfig `json:"cursor,omitempty"`
}

type cursorConfig struct {
	NodeExecutable      string `json:"nodeExecutable"`
	WorkerEntrypoint    string `json:"workerEntrypoint"`
	StateDir            string `json:"stateDir"`
	WorkingDir          string `json:"workingDir"`
	APIKeyFile          string `json:"apiKeyFile,omitempty"`
	CredentialsDir      string `json:"credentialsDir,omitempty"`
	AuthProbeEntrypoint string `json:"authProbeEntrypoint,omitempty"`
	Model               string `json:"model"`
}

type providerAdapter interface {
	harnessadapter.Adapter
	Close() error
}

func main() {
	path := flag.String("config", "", "path to the node JSON configuration")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 || os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "HARNESS_CONFIG_INVALID")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	diagnostics := diagnosticlog.New(os.Stderr)
	err := serve(ctx, *path, diagnostics)
	if err != nil {
		diagnostics.Emit(diagnosticlog.LevelError, diagnosticlog.ComponentService, diagnosticlog.EventServiceFailed, diagnosticlog.Fields{Reason: "start_or_serve_failed"})
	}
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	_ = diagnostics.Shutdown(shutdown)
	shutdownCancel()
	if err != nil {
		// Provider errors and configuration can contain credentials or prompts.
		fmt.Fprintln(os.Stderr, "HARNESS_START_OR_SERVE_FAILED")
		os.Exit(1)
	}
}

func boundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, errors.New("file exceeds limit")
	}
	return content, nil
}

func loadConfig(path string) (config, error) {
	raw, err := boundedFile(path, 64<<10)
	if err != nil {
		return config{}, err
	}
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return config{}, errors.New("multiple configuration values")
	}
	return cfg, nil
}

func serve(ctx context.Context, path string, diagnostics *diagnosticlog.Logger) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := validateProviderConfig(cfg); err != nil {
		return err
	}
	if diagnostics == nil {
		diagnostics = diagnosticlog.Disabled()
	}
	diagnostics.SetNodeID(cfg.NodeID)
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil || port == "" {
		return errors.New("explicit listen IP and port required")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertificateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	adapterKind := selectedAdapter(cfg)
	policies := filePolicy{contentPath: cfg.PolicyFile, manifestPath: cfg.ToolManifestFile, revision: cfg.PolicyRevision, approvalMode: cfg.ApprovalMode, adapter: adapterKind}
	policy, err := policies.Current(ctx, cfg.NodeID)
	if err != nil {
		return err
	}
	artifacts := node.NewArtifactIngress()
	adapter, auth, authBackend, err := openProviderRuntime(ctx, cfg, artifacts, policy, diagnostics)
	if err != nil {
		return err
	}
	defer adapter.Close()
	// Establish provider truth before the node action loop can admit queued work.
	// A transient probe failure deliberately leaves auth unknown and readiness
	// blocked while still allowing the private auth API to recover it later.
	if err := auth.Refresh(ctx); err != nil {
		return err
	}
	authority, err := node.Open(ctx, node.Config{
		DataDir: cfg.DataDir, NodeID: cfg.NodeID, OwnerID: cfg.OwnerID,
		RegistryVersion: cfg.RegistryVersion, Adapter: adapter, Policies: policies, Artifacts: artifacts, ProviderAuth: auth,
		ManualDispatchForTesting: cfg.ManualDispatchForTesting, Diagnostics: diagnostics,
	})
	if err != nil {
		return err
	}
	defer authority.Close()
	authBackend.SetBusy(authority.Busy)
	auth.SetTransitionGate(authority.BeginProviderAuthTransition)
	initialModel := cfg.Cursor.Model
	if initialModel == "" {
		initialModel = "composer-2.5"
	}
	settings, err := nodesettings.Open(cfg.NodeID, cfg.Cursor.StateDir, harnessadapter.CursorSDKVersion, initialModel, adapter.(nodesettings.CatalogSource))
	if err != nil {
		return err
	}
	handler, err := harnessserver.New(harnessserver.Config{NodeID: cfg.NodeID, ProviderAuth: auth, NodeSettings: settings, Diagnostics: diagnostics}, authority)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.Listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		// SSE outlives individual HTTP commands and can span long agent runs.
		WriteTimeout: 0, ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}},
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	fmt.Fprintln(os.Stdout, "HARNESS_STARTING")
	diagnostics.Emit(diagnosticlog.LevelInfo, diagnosticlog.ComponentService, diagnosticlog.EventServiceStarting, diagnosticlog.Fields{})
	err = server.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		diagnostics.Emit(diagnosticlog.LevelInfo, diagnosticlog.ComponentService, diagnosticlog.EventServiceStopped, diagnosticlog.Fields{})
		return nil
	}
	return err
}

func openProviderRuntime(ctx context.Context, cfg config, artifacts node.ArtifactSink, policy harnessadapter.PolicySnapshot, diagnostics *diagnosticlog.Logger) (providerAdapter, *providerauth.Manager, *cursor.AuthBackend, error) {
	if err := validateProviderConfig(cfg); err != nil {
		return nil, nil, nil, err
	}
	switch selectedAdapter(cfg) {
	case string(harnessadapter.KindCursor):
		var runner toolrunner.Runner
		if policy.ApprovalMode == harnessadapter.ApprovalModeExplicitOnce {
			if err := validateExplicitToolWorkspace("Cursor", cfg.Cursor.WorkingDir); err != nil {
				return nil, nil, nil, err
			}
			var err error
			runner, err = newToolRunner(ctx)
			if err != nil {
				return nil, nil, nil, err
			}
		}
		managed, err := cursor.NewManaged(cursor.Config{
			NodeExecutable: cfg.Cursor.NodeExecutable, WorkerEntrypoint: cfg.Cursor.WorkerEntrypoint,
			StateDir: cfg.Cursor.StateDir, WorkingDir: cfg.Cursor.WorkingDir, Model: cfg.Cursor.Model,
			OperationTimeout: 30 * time.Second, MaxFrameBytes: 8 << 20, ToolRunner: runner, Diagnostics: diagnostics,
		}, artifacts)
		if err != nil {
			return nil, nil, nil, err
		}
		credentialsDir := cfg.Cursor.CredentialsDir
		if credentialsDir == "" {
			credentialsDir = filepath.Join(cfg.Cursor.StateDir, "credentials")
		}
		if err := validateCredentialIsolation(credentialsDir, cfg.Cursor.WorkingDir); err != nil {
			_ = managed.Close()
			return nil, nil, nil, err
		}
		probeEntrypoint := cfg.Cursor.AuthProbeEntrypoint
		if probeEntrypoint == "" {
			probeEntrypoint = filepath.Join(filepath.Dir(cfg.Cursor.WorkerEntrypoint), "auth_probe.mjs")
		}
		backend, err := cursor.NewAuthBackend(managed, filepath.Join(credentialsDir, "cursor.key"), cfg.Cursor.APIKeyFile, cfg.Cursor.NodeExecutable, probeEntrypoint)
		if err != nil {
			_ = managed.Close()
			return nil, nil, nil, err
		}
		auth, err := providerauth.Open(ctx, cfg.NodeID, credentialsDir, backend)
		if err != nil {
			_ = managed.Close()
			return nil, nil, nil, err
		}
		return managed, auth, backend, nil
	default:
		return nil, nil, nil, errors.New("valid adapter selector is required")
	}
}

func pathsOverlap(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	left, right = filepath.Clean(left), filepath.Clean(right)
	within := func(base, candidate string) bool {
		relative, err := filepath.Rel(base, candidate)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(left, right) || within(right, left)
}

func validateCredentialIsolation(credentialsDir, workingDir string) error {
	if !filepath.IsAbs(credentialsDir) || workingDir != "" && !filepath.IsAbs(workingDir) {
		return errors.New("Cursor credential and configured tool workspace paths must be absolute")
	}
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		return errors.New("Cursor credential directory is unavailable")
	}
	info, err := os.Lstat(credentialsDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Cursor credential directory is invalid")
	}
	canonicalCredentials, err := filepath.EvalSymlinks(credentialsDir)
	if err != nil {
		return errors.New("Cursor credential directory cannot be resolved")
	}
	if workingDir == "" {
		return nil
	}
	canonicalWorking, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return errors.New("Cursor tool workspace cannot be resolved")
	}
	if pathsOverlap(canonicalCredentials, canonicalWorking) {
		return errors.New("Cursor credential and tool workspace paths overlap")
	}
	return nil
}

func validateExplicitToolWorkspace(adapter, workingDir string) error {
	if workingDir != "/workspace" {
		return fmt.Errorf("%s explicit tool workspace is invalid", adapter)
	}
	return nil
}

func newToolRunner(ctx context.Context) (toolrunner.Runner, error) {
	return toolrunner.NewHelper(ctx, toolrunner.Config{
		Executable: toolRunnerExecutable,
		SystemReadRoots: []string{
			"/usr", "/etc/ld.so.cache", "/etc/ssl/certs", "/dev/null", "/dev/urandom",
		},
	})
}

func selectedAdapter(cfg config) string {
	return cfg.Adapter
}

func validateProviderConfig(cfg config) error {
	if selectedAdapter(cfg) != string(harnessadapter.KindCursor) || cfg.Cursor == nil {
		return errors.New("exact Cursor adapter configuration is required")
	}
	if cfg.ManualDispatchForTesting &&
		(cfg.PolicyRevision != "hl304-fixture@1" || effectiveApprovalMode(cfg.ApprovalMode) != harnessadapter.ApprovalModeDeny || cfg.Cursor.Model != "fixture-no-provider-call") {
		return errors.New("manual dispatch is restricted to the HL-304 no-provider fixture")
	}
	return nil
}

func effectiveApprovalMode(mode string) string {
	if mode == "" {
		return harnessadapter.ApprovalModeDeny
	}
	return mode
}

type filePolicy struct {
	contentPath, manifestPath, revision string
	approvalMode                        string
	adapter                             string
}

func (source filePolicy) Current(ctx context.Context, _ string) (harnessadapter.PolicySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	content, err := boundedFile(source.contentPath, 64<<10)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	manifest, err := boundedFile(source.manifestPath, 64<<10)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	approvalMode := source.approvalMode
	if approvalMode == "" {
		approvalMode = harnessadapter.ApprovalModeDeny
	}
	switch approvalMode {
	case harnessadapter.ApprovalModeDeny:
		var tools []json.RawMessage
		if json.Unmarshal(manifest, &tools) != nil || tools == nil || len(tools) != 0 {
			return harnessadapter.PolicySnapshot{}, errors.New("deny policy requires an empty tool manifest")
		}
	case harnessadapter.ApprovalModeExplicitOnce:
		expected := ""
		switch source.adapter {
		case string(harnessadapter.KindCursor):
			expected = cursorExplicitToolManifest
		}
		if expected == "" || !bytes.Equal(manifest, []byte(expected)) {
			return harnessadapter.PolicySnapshot{}, errors.New("explicit_once policy requires the exact adapter tool manifest")
		}
	default:
		return harnessadapter.PolicySnapshot{}, errors.New("approval mode is invalid")
	}
	contentHash, manifestHash := sha256.Sum256(content), sha256.Sum256(manifest)
	policy := harnessadapter.PolicySnapshot{
		Revision: source.revision, Content: content, ToolManifest: manifest,
		ContentHash: hex.EncodeToString(contentHash[:]), ToolManifestHash: hex.EncodeToString(manifestHash[:]),
		ApprovalMode: approvalMode,
	}
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return harnessadapter.PreparePolicySnapshot(policy)
}
