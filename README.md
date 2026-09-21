# Harness Cursor

Standalone Cursor Harness node for the HomeLab Panel C1 private wire. This repository owns the Cursor adapter, durable SQLite execution authority, private mTLS API, tool sandbox helper, protocol artifacts, and the Cursor-only container image.

The extraction source is `homelab-telegram-panel` commit `d5edfb20f358bb0ce243d07840b49da8566ab0ca` (base `9736f8c74e2f242cbe63dda3b0c74b42bc8d04d9`). The target repository started at `89d24811028b1ca0937894b0a4be9c05bbebb180`. [The provenance manifest](provenance/source-manifest.csv) records every considered source file, its SHA-256, destination, or explicit exclusion.

## Boundaries

- The executable supports only `adapter: "cursor"`; Codex and unknown provider configuration fail closed before a provider process starts.
- C1 schemas, fixtures, receipts, SQLite migrations, mTLS pins, fencing, versions, and recovery semantics remain compatible with the pinned source snapshot.
- Panel, Router, Agent Service clients, host tunnels, Fixik, provider secrets, databases, state, and production configuration are not part of this repository.
- There is no sibling `replace`, shared database, shared volume, or build dependency on another checkout.

Removing the Cursor code from the source monorepo, changing Panel deployment, publishing an image, and production cutover are separate integration operations. A green standalone candidate is a prerequisite, not authorization for those actions.

## Layout

- `adapters/cursor` — Go adapter and pinned Node worker.
- `runtime`, `api`, `contracts`, `tools` — durable node, mTLS API, C1 artifacts, and sandbox helper.
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

The following creates both exact policy manifests and a complete explicit-once configuration. `gateway.crt` and `operator.crt` must be distinct client certificates signed by `ca.crt`; `server.crt`/`server.key` are the node's server identity.

```sh
mkdir -p config
printf 'non-empty policy\n' >config/policy.txt
printf '[]\n' >config/tools-deny.json
printf '[{"name":"cursor.command"},{"name":"cursor.file_change"}]\n' >config/tools-explicit.json
printf '%s' 'replace-with-real-cursor-key' >config/cursor.key

GATEWAY_PIN=$(openssl x509 -in config/gateway.crt -outform DER | sha256sum | cut -d ' ' -f 1)
OPERATOR_PIN=$(openssl x509 -in config/operator.crt -outform DER | sha256sum | cut -d ' ' -f 1)
cat >config/node.json <<EOF
{
  "listen": "0.0.0.0:8443",
  "nodeId": "11111111-1111-4111-8111-111111111111",
  "ownerId": "22222222-2222-4222-8222-222222222222",
  "dataDir": "/state/harness",
  "registryVersion": 1,
  "certificateFile": "/config/server.crt",
  "keyFile": "/config/server.key",
  "clientCAFile": "/config/ca.crt",
  "gatewayCertificateSHA256": "$GATEWAY_PIN",
  "operatorCertificateSHA256": "$OPERATOR_PIN",
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
    "apiKeyFile": "/config/cursor.key",
    "model": "configured-model"
  }
}
EOF
sudo chown -R 10001:10001 config
sudo chmod 0700 config
sudo chmod 0600 config/*.key
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
  --mount "type=bind,source=$PWD/config,target=/config,readonly" \
  --publish 127.0.0.1:8443:8443 \
  harness-cursor:local --config /config/node.json
```

For durable use, replace only the `/state` tmpfs with a private persistent volume pre-owned by `10001:10001`; never share it with Panel, Router, Fixik, or another Harness. `/config` stays read-only, and only `server.key` plus `cursor.key` are readable by the runtime UID. Explicit-once mode additionally requires private writable `/tmp` for the helper self-test and `/workspace` for dialog workspaces. Do not place secret bytes in JSON, Git, logs, or image layers.
