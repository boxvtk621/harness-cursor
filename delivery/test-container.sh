#!/bin/sh
set -eu

# Git Bash rewrites Linux container paths unless Docker arguments are exempted.
if test -n "${MSYSTEM:-}"; then
  docker() { MSYS_NO_PATHCONV=1 command docker "$@"; }
  subject_prefix=//
else
  subject_prefix=/
fi

image=${1:?image name is required}
tmp=$(mktemp -d)
config="$tmp/config"
config_copy_source="$config"
if test -n "${MSYSTEM:-}"; then
  config_copy_source=$(cygpath -w "$config")
fi
volume="harness-cursor-smoke-$$"
network="harness-cursor-smoke-$$"
container=""
helper=""
cleanup() {
  test -z "$container" || docker rm -f "$container" >/dev/null 2>&1 || true
  test -z "$helper" || docker rm -f "$helper" >/dev/null 2>&1 || true
  docker volume rm -f "$volume" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

user=$(docker image inspect --format '{{.Config.User}}' "$image")
entrypoint=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image")
source_label=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$image")
test "$user" = "10001:10001"
test "$entrypoint" = '["/harness-node"]'
test "$source_label" = "https://github.com/boxvtk621/harness-cursor"

mkdir "$config"
openssl req -x509 -newkey ed25519 -nodes -days 1 -subj "${subject_prefix}CN=harness-smoke-ca" -keyout "$tmp/ca.key" -out "$tmp/ca.crt" >/dev/null 2>&1
openssl req -newkey ed25519 -nodes -subj "${subject_prefix}CN=harness-smoke-server" -keyout "$tmp/server.key" -out "$tmp/server.csr" >/dev/null 2>&1
printf 'subjectAltName=IP:127.0.0.1,DNS:localhost,DNS:harness-smoke\nextendedKeyUsage=serverAuth\n' >"$tmp/server.ext"
openssl x509 -req -days 1 -in "$tmp/server.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial -extfile "$tmp/server.ext" -out "$tmp/server.crt" >/dev/null 2>&1
cp "$tmp/ca.crt" "$tmp/server.crt" "$tmp/server.key" "$config/"
printf 'smoke policy\n' >"$config/policy.txt"
printf '[]\n' >"$config/tools-deny.json"
printf '[{"name":"cursor.command"},{"name":"cursor.file_change"}]\n' >"$config/tools-explicit.json"
printf 'smoke-api-key' >"$config/cursor.key"
write_config() {
  name=$1
  manifest=$2
  approval=$3
  workspace=$4
  cat >"$config/$name" <<EOF
{
  "listen": "0.0.0.0:8443",
  "nodeId": "11111111-1111-4111-8111-111111111111",
  "ownerId": "22222222-2222-4222-8222-222222222222",
  "dataDir": "/state/harness",
  "registryVersion": 1,
  "certificateFile": "/config/server.crt",
  "keyFile": "/config/server.key",
  "policyFile": "/config/policy.txt",
  "toolManifestFile": "/config/$manifest",
  "policyRevision": "smoke-$approval@1",
  "approvalMode": "$approval",
  "adapter": "cursor",
  "cursor": {
    "nodeExecutable": "/usr/local/bin/node",
    "workerEntrypoint": "/opt/worker/worker.mjs",
    "stateDir": "/state/cursor",
    "workingDir": "$workspace",
    "apiKeyFile": "/config/cursor.key",
    "model": "smoke-model"
  }
}
EOF
}
write_config node-deny.json tools-deny.json deny ""
write_config node-explicit.json tools-explicit.json explicit_once /workspace

docker volume create "$volume" >/dev/null
docker network create "$network" >/dev/null
helper=$(docker create --user 0 --entrypoint /bin/sh --mount "type=volume,source=$volume,target=/config" "$image" -c true)
docker cp "$config_copy_source/." "$helper:/config/"
docker rm "$helper" >/dev/null
helper=""
docker run --rm --user 0 --entrypoint /bin/sh --mount "type=volume,source=$volume,target=/config" "$image" -c 'chown -R 10001:10001 /config && chmod 0600 /config/*.key && chmod 0644 /config/*.crt /config/*.json /config/*.txt' >/dev/null

stop_container() {
  docker stop "$container" >/dev/null
  docker rm -f "$container" >/dev/null 2>&1 || true
  container=""
}

wait_starting() {
  for _ in $(seq 1 30); do
    if docker logs "$container" 2>&1 | grep -qx 'HARNESS_STARTING'; then
      return
    fi
    if ! docker inspect --format '{{.State.Running}}' "$container" | grep -qx true; then
      docker logs "$container" >&2
      return 1
    fi
    sleep 1
  done
  docker logs "$container" >&2
  return 1
}

valid_readiness() {
  printf '%s' "$1" | grep -q '"readiness":"blocked"' &&
    printf '%s' "$1" | grep -q '"blockedReasons":\["policy_unavailable"\]' &&
    printf '%s' "$1" | grep -q '"kind":"cursor"' &&
    printf '%s' "$1" | grep -q '"version":"1.0.31"' &&
    printf '%s' "$1" | grep -q '"schemaSHA256":"5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"'
}

wait_readiness() {
  for _ in $(seq 1 30); do
    ready=$(docker run --rm --network "$network" --mount "type=volume,source=$volume,target=/config,readonly" curlimages/curl:8.16.0 --fail --silent --cacert /config/ca.crt https://harness-smoke:8443/health/ready 2>/dev/null || true)
    if test -n "$ready" && valid_readiness "$ready"; then
      return
    fi
    if ! docker inspect --format '{{.State.Running}}' "$container" | grep -qx true; then
      docker logs "$container" >&2
      return 1
    fi
    sleep 1
  done
  docker logs "$container" >&2
  return 1
}

container=$(docker run --detach --read-only --user 10001:10001 --pids-limit 128 --network "$network" --network-alias harness-smoke --tmpfs /state:rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=10001,mode=0700 --mount "type=volume,source=$volume,target=/config,readonly" "$image" --config /config/node-deny.json)
wait_starting
wait_readiness
stop_container

# explicit_once constructs and self-tests the packaged /harness-tool-runner.
container=$(docker run --detach --read-only --user 10001:10001 --pids-limit 128 --memory 512m --network "$network" --network-alias harness-smoke --tmpfs /state:rw,nosuid,nodev,noexec,size=64m,uid=10001,gid=10001,mode=0700 --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m,uid=10001,gid=10001,mode=0700 --tmpfs /workspace:rw,nosuid,nodev,noexec,size=16m,uid=10001,gid=10001,mode=0700 --mount "type=volume,source=$volume,target=/config,readonly" "$image" --config /config/node-explicit.json)
wait_starting
wait_readiness
stop_container

set +e
output=$(docker run --rm --read-only --user 10001:10001 "$image" --config /missing 2>&1)
status=$?
set -e
test "$status" -ne 0
test "$output" = "HARNESS_START_OR_SERVE_FAILED"
