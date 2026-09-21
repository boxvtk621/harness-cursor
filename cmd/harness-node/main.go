package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/boxvtk621/harness-cursor/adapters/contract"
	"github.com/boxvtk621/harness-cursor/adapters/cursor"
	harnessserver "github.com/boxvtk621/harness-cursor/api"
	"github.com/boxvtk621/harness-cursor/runtime"
	"github.com/boxvtk621/harness-cursor/tools"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	cursorExplicitToolManifest = "[{\"name\":\"cursor.command\"},{\"name\":\"cursor.file_change\"}]\n"
	toolRunnerExecutable       = "/harness-tool-runner"
)

type config struct {
	Listen                    string        `json:"listen"`
	NodeID                    string        `json:"nodeId"`
	OwnerID                   string        `json:"ownerId"`
	DataDir                   string        `json:"dataDir"`
	RegistryVersion           int64         `json:"registryVersion"`
	CertificateFile           string        `json:"certificateFile"`
	KeyFile                   string        `json:"keyFile"`
	ClientCAFile              string        `json:"clientCAFile"`
	GatewayCertificateSHA256  string        `json:"gatewayCertificateSHA256"`
	OperatorCertificateSHA256 string        `json:"operatorCertificateSHA256"`
	PolicyFile                string        `json:"policyFile"`
	ToolManifestFile          string        `json:"toolManifestFile"`
	PolicyRevision            string        `json:"policyRevision"`
	ApprovalMode              string        `json:"approvalMode,omitempty"`
	Adapter                   string        `json:"adapter"`
	Cursor                    *cursorConfig `json:"cursor,omitempty"`
}

type cursorConfig struct {
	NodeExecutable   string `json:"nodeExecutable"`
	WorkerEntrypoint string `json:"workerEntrypoint"`
	StateDir         string `json:"stateDir"`
	WorkingDir       string `json:"workingDir"`
	APIKeyFile       string `json:"apiKeyFile"`
	Model            string `json:"model"`
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
	if err := serve(ctx, *path); err != nil {
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

func serve(ctx context.Context, path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if err := validateProviderConfig(cfg); err != nil {
		return err
	}
	if cfg.OperatorCertificateSHA256 == "" || cfg.OperatorCertificateSHA256 == cfg.GatewayCertificateSHA256 {
		return errors.New("distinct execution and operator certificate pins are required")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil || port == "" {
		return errors.New("explicit listen IP and port required")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertificateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	caPEM, err := boundedFile(cfg.ClientCAFile, 64<<10)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("client CA is invalid")
	}
	adapterKind := selectedAdapter(cfg)
	policies := filePolicy{contentPath: cfg.PolicyFile, manifestPath: cfg.ToolManifestFile, revision: cfg.PolicyRevision, approvalMode: cfg.ApprovalMode, adapter: adapterKind}
	policy, err := policies.Current(ctx, cfg.NodeID)
	if err != nil {
		return err
	}
	artifacts := node.NewArtifactIngress()
	adapter, err := openProviderAdapter(ctx, cfg, artifacts, policy)
	if err != nil {
		return err
	}
	defer adapter.Close()
	authority, err := node.Open(ctx, node.Config{
		DataDir: cfg.DataDir, NodeID: cfg.NodeID, OwnerID: cfg.OwnerID,
		RegistryVersion: cfg.RegistryVersion, Adapter: adapter, Policies: policies, Artifacts: artifacts,
	})
	if err != nil {
		return err
	}
	defer authority.Close()
	handler, err := harnessserver.New(harnessserver.Config{
		NodeID: cfg.NodeID, GatewayCertificateSHA256: cfg.GatewayCertificateSHA256,
		OperatorCertificateSHA256: cfg.OperatorCertificateSHA256,
	}, authority)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.Listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		// SSE outlives individual HTTP commands and can span long agent runs.
		WriteTimeout: 0, ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
			ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert},
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
	err = server.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func openProviderAdapter(ctx context.Context, cfg config, artifacts node.ArtifactSink, policy harnessadapter.PolicySnapshot) (providerAdapter, error) {
	if err := validateProviderConfig(cfg); err != nil {
		return nil, err
	}
	switch selectedAdapter(cfg) {
	case string(harnessadapter.KindCursor):
		var runner toolrunner.Runner
		if policy.ApprovalMode == harnessadapter.ApprovalModeExplicitOnce {
			if err := validateExplicitToolWorkspace("Cursor", cfg.Cursor.WorkingDir); err != nil {
				return nil, err
			}
			var err error
			runner, err = newToolRunner(ctx)
			if err != nil {
				return nil, err
			}
		}
		secretInfo, err := os.Lstat(cfg.Cursor.APIKeyFile)
		if err != nil || !secretInfo.Mode().IsRegular() || secretInfo.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("private Cursor key file required")
		}
		key, err := boundedFile(cfg.Cursor.APIKeyFile, 4096)
		if err != nil {
			return nil, err
		}
		apiKey := strings.TrimSuffix(strings.TrimSuffix(string(key), "\n"), "\r")
		if apiKey == "" || strings.ContainsAny(apiKey, "\r\n\x00") {
			return nil, errors.New("Cursor key is invalid")
		}
		return cursor.New(cursor.Config{
			NodeExecutable: cfg.Cursor.NodeExecutable, WorkerEntrypoint: cfg.Cursor.WorkerEntrypoint,
			StateDir: cfg.Cursor.StateDir, WorkingDir: cfg.Cursor.WorkingDir, APIKey: apiKey, Model: cfg.Cursor.Model,
			OperationTimeout: 30 * time.Second, MaxFrameBytes: 8 << 20, ToolRunner: runner,
		}, artifacts)
	default:
		return nil, errors.New("valid adapter selector is required")
	}
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
	return nil
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
