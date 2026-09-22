# Harness Cursor

Standalone Cursor Harness node for the HomeLab private wire. This repository owns the Cursor adapter, durable SQLite execution authority, private TLS API, tool sandbox helper, protocol artifacts, and the Cursor-only container image.

The extraction source is `homelab-telegram-panel` commit `d5edfb20f358bb0ce243d07840b49da8566ab0ca` (base `9736f8c74e2f242cbe63dda3b0c74b42bc8d04d9`). The target repository started at `89d24811028b1ca0937894b0a4be9c05bbebb180`. [The provenance manifest](provenance/source-manifest.csv) records every considered source file, its SHA-256, destination, or explicit exclusion.

## Boundaries

- The executable supports only `adapter: "cursor"`; Codex and unknown provider configuration fail closed before a provider process starts.
- Schemas, fixtures, receipts, SQLite migrations, fencing, versions, and recovery semantics remain compatible with the pinned source snapshot; inbound client-certificate and actor authorization have been removed.
- Panel, Router, Agent Service clients, host tunnels, Fixik, shared provider secrets, databases, state, and production configuration are not part of this repository. The node owns its private provider-auth volume.
- There is no sibling `replace`, shared database, shared volume, or build dependency on another checkout.

Removing the Cursor code from the source monorepo, changing Panel deployment, publishing an image, and production cutover are separate integration operations. A green standalone candidate is a prerequisite, not authorization for those actions.

## Layout

- `adapters/cursor` — Go adapter and pinned Node worker.
- `runtime`, `api`, `contracts`, `tools` — durable node, private TLS API, wire artifacts, and sandbox helper.
- `cmd/harness-node`, `cmd/harness-tool-runner` — Cursor-only executables.
- `delivery/Dockerfile.cursor` — pinned, non-root image.
- `provenance/source-manifest.csv` — source/destination hash inventory.

## Verification

Linux with Go 1.26.5, Node 24.18.0, npm, and Docker:

```sh
git clone https://github.com/boxvtk621/homelab-telegram-panel .provenance-source/homelab-telegram-panel
git -C .provenance-source/homelab-telegram-panel checkout d5edfb20f358bb0ce243d07840b49da8566ab0ca
make quality SOURCE_REPO="$PWD/.provenance-source/homelab-telegram-panel"
make image IMAGE=harness-cursor:local VERSION=local
make container-smoke IMAGE=harness-cursor:local
```

`make quality` requires the pinned source checkout and runs formatting, patch whitespace, dependency and boundary scans, `go vet`, all Go race tests, the isolated 32 MiB physical-ENOSPC test, Cursor worker tests, contract negative vectors/generator parity, source-aware provenance verification, and standalone builds without skips. The GitHub workflow checks out that private source commit with the read-only `HOMELAB_SOURCE_READ_TOKEN` repository secret; pull requests without that secret cannot satisfy the required provenance gate.

## Runtime configuration

The following creates both exact policy manifests and a complete explicit-once configuration. `server.crt`/`server.key` are the node's server identity. The private listener does not request or authorize client certificates and accepts no actor header.

```sh
mkdir -p config
printf 'non-empty policy\n' >config/policy.txt
printf '[]\n' >config/tools-deny.json
printf '[{"name":"cursor.command"},{"name":"cursor.file_change"}]\n' >config/tools-explicit.json

cat >config/node.json <<EOF
{
  "listen": "0.0.0.0:8443",
  "nodeId": "11111111-1111-4111-8111-111111111111",
  "ownerId": "22222222-2222-4222-8222-222222222222",
  "dataDir": "/state/harness",
  "registryVersion": 1,
  "certificateFile": "/config/server.crt",
  "keyFile": "/config/server.key",
  "policyFile": "/config/policy.txt",
  "toolManifestFile": "/config/tools-explicit.json",
  "policyRevision": "local@1",
  "approvalMode": "explicit_once",
  "adapter": "cursor",
  "cursor": {
    "nodeExecutable": "/usr/local/bin/node",
    "workerEntrypoint": "/opt/worker/worker.mjs",
    "stateDir": "/state/cursor",
    "workingDir": "/workspace",
    "credentialsDir": "/provider-auth",
    "authProbeEntrypoint": "/opt/worker/auth_probe.mjs",
    "model": "configured-model"
  }
}
EOF
sudo chown -R 10001:10001 config
sudo chmod 0700 config
sudo chmod 0644 config/*.crt config/*.json config/*.txt
```

The manifest bytes are policy input: keep the final LF and do not reformat them. For deny mode, set `approvalMode` to `deny`, `toolManifestFile` to `/config/tools-deny.json`, and `cursor.workingDir` to the empty string. For `explicit_once`, the manifest, `/workspace` path, and packaged `/harness-tool-runner` are mandatory.

This bounded, ephemeral run demonstrates the non-root/read-only mount contract:

```sh
docker run --rm --read-only --user 10001:10001 \
  --pids-limit 128 --memory 512m \
  --tmpfs /state:rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=10001,mode=0700 \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m,uid=10001,gid=10001,mode=0700 \
  --tmpfs /workspace:rw,nosuid,nodev,noexec,size=16m,uid=10001,gid=10001,mode=0700 \
  --tmpfs /provider-auth:rw,nosuid,nodev,noexec,size=4m,uid=10001,gid=10001,mode=0700 \
  --mount "type=bind,source=$PWD/config,target=/config,readonly" \
  --publish 127.0.0.1:8443:8443 \
  harness-cursor:local --config /config/node.json
```

For durable use, replace `/state` with a persistent volume and mount a second private persistent volume at `/provider-auth`, both pre-owned by `10001:10001`; never share either with Panel, Router, Fixik, another Harness, or the tool runner. `/config` stays read-only and only `server.key` is secret there. Explicit-once mode additionally requires private writable `/tmp` for the helper self-test and `/workspace` for dialog workspaces. Do not place provider secret bytes in JSON configuration, Git, logs, or image layers.

### Diagnostic logs

The node writes bounded JSON Lines diagnostics to stderr using schema `harness.console.v1`. The existing `HARNESS_STARTING` stdout sentinel is unchanged. Cursor worker stdout, the auth-probe stdout, and tool-runner stdout are private protocols and never carry diagnostic records.

Diagnostics contain only allowlisted lifecycle event names, mandatory `nodeId`, per-process `bootId`, stable reason/error codes, UUID correlations, counters, durations, and booleans. They never contain prompts, messages, policy text, file content or paths, tool arguments or output, provider responses, credentials, device codes, or raw exception strings. Logging is best-effort: producers use a 256-entry non-blocking queue, each record is at most 4 KiB, and dropped or failed writes are summarized when the sink becomes available. Logs are operational evidence only; durable Harness state and receipts remain the source of truth.

The shared catalogue covers service and recovery lifecycle, runtime open/recovery/close, provider process ready/exit, accepted/rejected/deduplicated commands, attempt dispatch/start/terminal/unknown, tool start/completion, provider-auth and readiness states observed at API boundaries, and bounded logger-loss summaries. These API events report observed state and are not a second authoritative state machine. Poll reads, streaming deltas, prompts, provider frames, tool output, and other high-frequency or sensitive data are deliberately excluded.

## Provider authentication

The provider-neutral API is described by `contracts/provider-auth-v1.schema.json`. Cursor declares only the `secret` method. `POST /v1/provider-auth/operations` accepts the write-only secret, validates it with the pinned SDK's `Cursor.me()` account call, and supplies it to the SDK only as `CURSOR_API_KEY`. This check does not create an agent or make a model request. `GET /v1/provider-auth?nodeId=<uuid>` never returns the secret or an account identifier.

The node writes `/provider-auth/cursor.key` and `/provider-auth/state.json` atomically with owner-only permissions. File presence produces `unknown`, not `authenticated`; startup and explicit checks require a provider readback. A network failure produces `unknown` and keeps the credential, so a transient outage is not reported as revocation. Logout is local and removes this node's credential; it does not claim to revoke every provider session.

The provider-auth ledger retains up to 4,096 command receipts and their referenced operations without eviction. Every known `commandId` remains replayable and can never start a second login; after the bound is reached, new auth commands fail closed with `busy` until an operator replaces the node's private auth volume under an explicit maintenance procedure. There is no automatic forget or replay window.

`apiKeyFile` is optional and supported only as a legacy first-boot seed. Once auth state exists, it is never imported again, so logout remains effective across restarts. New deployments should omit it and use the write-only API.
