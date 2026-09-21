---
title: 'HL-299: standalone repository for Harness Cursor'
type: 'refactor'
created: '2026-09-21'
status: 'done'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '89d24811028b1ca0937894b0a4be9c05bbebb180'
context: []
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Cursor Harness существует только внутри `homelab-telegram-panel`: сборка зависит от monorepo paths, общий binary содержит Codex, а отдельный репозиторий `harness-cursor` пуст. Это мешает независимо собирать, проверять и выпускать Cursor-ноду.

**Approach:** Перенести проверенный Cursor dependency closure в `harness-cursor/main` как самостоятельный Go/Node/container project, сохранив C1 wire, SQLite и security semantics. Source monorepo остаётся read-only; его удаление/cutover выполняется отдельной интеграцией после зелёного standalone candidate.

## Boundaries & Constraints

**Always:** Authority — HL-299@1; source snapshot `d5edfb20f358bb0ce243d07840b49da8566ab0ca` (base `9736f8c74e2f242cbe63dda3b0c74b42bc8d04d9`), target base `89d24811028b1ca0937894b0a4be9c05bbebb180`. Новый module path — `github.com/boxvtk621/harness-cursor`. Копируются Cursor adapter/worker, Harness runtime/API/contracts/tools, нужные fixtures/tests и Cursor image. JSON schema IDs/bytes, receipts, SQLite migrations, mTLS/fencing, exact versions и non-root/read-only runtime сохраняются. Каждый перенесённый файл отражается в provenance manifest с source path/hash.

**Never:** Не переносить Codex adapter/runtime, Panel, Router, Agent Service client, host tunnel, Fixik, secrets, DB/state или production config. Не добавлять sibling/local `replace`, общий core-repository, новый API/wire/data contract, dependency upgrade или provider call. Не менять source monorepo. Commit/push/tag/release/deploy запрещены без отдельной команды.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Cursor startup | exact Cursor config + mTLS files | cursor-only node starts with preserved IDs/readiness | missing/invalid fields fail before effects |
| Wrong provider | Codex/unknown adapter config | process rejects configuration | no fallback or provider process |
| Restart | existing supported SQLite state | reopen/recovery preserves queue/history/generation | newer/invalid schema fails closed |
| Build context | repository contains VCS/env/state files | only allowlisted source enters image context | CI fails on forbidden path |

</frozen-after-approval>

## Code Map

- `harness/{runtime,api,contracts,tools}` at source snapshot -- execution, HTTP/wire contracts, SQLite authority and sandbox; relocate to same top-level roles with imports rewritten only.
- `harness/adapters/{contract,cursor}` -- provider-neutral adapter contract plus Cursor implementation/Node worker; exclude sibling `codex`.
- `harness/cmd/{harness-node,harness-tool-runner}` -- make `harness-node` cursor-only and remove Codex config/selection branches.
- `internal/strictjson` and `agent-service/contracts/logical-delete` -- copy minimal byte-identical validation/DTO dependencies locally as `internal/strictjson` and `contracts/logical-delete`; exclude Agent Service client and cross-component integration test.
- `harness/delivery/Dockerfile.cursor*` -- flatten to repository build context, update OCI source, preserve pinned images/user/entrypoint.
- `.github/workflows/quality.yml`, `Makefile`, `README.md`, `AGENTS.md` -- standalone quality/build/context checks and operator/developer boundary.
- `provenance/source-manifest.csv` -- source commit/path/destination/SHA-256 and exclusion rationale.

## Tasks & Acceptance

**Execution:**
- [x] `provenance/source-manifest.csv` -- enumerate selected and explicitly excluded source files; require unmatched/ambiguous zero.
- [x] Go packages and `go.mod/go.sum` -- copy closure, rename imports/module, localize only required contracts, remove monorepo replacements.
- [x] `cmd/harness-node` and tests -- retain Cursor path only; add fail-closed unknown/Codex configuration tests.
- [x] Cursor worker and contracts -- preserve lockfile/schema/fixture bytes and run existing negative vectors.
- [x] Container/CI/docs -- create allowlisted build, standalone gates and exact source/cutover notes.

**Acceptance Criteria:**
- Given the target tree, when dependency and architecture scans run, then no Codex/Panel/Router/Agent Service/Fixik import, asset or sibling replace is reachable.
- Given the exact candidate, when Go fmt/vet/race/tests, Node worker tests, schema parity, standalone build and Cursor image smoke run, then all pass without skips.
- Given existing supported SQLite fixtures and invalid provider/config cases, when the binary starts or reopens, then preserved recovery succeeds and unsafe inputs fail before effects.
- Given the manifest, when hashes are recomputed from the pinned source snapshot, then every included file matches and exclusions are explicit.

## Implementation Notes

- Extracted the Cursor dependency closure into module `github.com/boxvtk621/harness-cursor`; source snapshot remained read-only.
- Added source-aware provenance, parsed-import architecture guards, pinned CI actions/images, isolated physical-ENOSPC coverage, and deny/explicit-once container smoke.
- Review fixed all in-scope findings. Seven pre-existing runtime/design risks are recorded in `deferred-work.md`; no known in-scope defect remains.

## Spec Change Log

## Review Triage Log

| ID | Verdict | Route | Evidence |
|---|---|---|---|
| VG-1 | medium | patch | `delivery/test-container.sh` starts only `approvalMode: deny`; therefore the packaged `/harness-tool-runner` never passes the production `NewHelper`/`SelfTest` path. |
| VG-2 | medium | patch | `make quality` invokes provenance without `--source-repo`, so CI cannot enumerate the pinned source tree or recompute its blob hashes. |
| VG-3 | high | patch | `tools/helper_runner.go` creates its self-test directory under the default `/tmp`, while the documented read-only image supplies no writable `/tmp`; `explicit_once` startup consequently fails unless the runtime contract adds a private tmpfs. |
| BH-1 | medium | defer | `mappingStore` mutates memory before the atomic write succeeds; this behavior is byte-identical at source snapshot `d5edfb2`, so the extraction did not introduce it and a transactional redesign is deferred. |
| BH-2 | medium | defer | `finishRuntime` discards the durable terminal-write error, which can leave an active mapping after a published terminal; this is pre-existing source behavior and needs a dedicated recovery design. |
| BH-3 | medium | defer | Terminal attempts remain in both memory and `native-mapping.json`; retention is unbounded, but it is pre-existing and safe pruning needs an idempotency/retention decision outside this extraction. |
| BH-4 | medium | defer | `native-mapping.json` is read without a byte limit; the copied source has the same availability risk and should gain a separately designed state-file bound. |
| BH-5 | medium | defer | Mapping decode validates schema/maps but not key/reference/state invariants; the private file still can fail unsafely when corrupted, but the issue predates extraction and requires a migration-compatible validator. |
| BH-6 | maybe-false | defer | The worker inherits the Harness environment. Exposure requires deployment evidence that unrelated secrets are present; settle by inventorying the production env and defining a worker env allowlist. |
| BH-7 | maybe-false | defer | The bridge kills only the Node process, but no child-spawning SDK path was demonstrated. Settle by tracing the pinned SDK/process tree before adding process-group behavior. |
| BH-8 | low | reject | Unknown `event` names are ignored, but the exact pinned worker emits only `tool` and `terminal`; this is unlikely in normal use and a new protocol branch is more than a direct correction. |
| BH-9 | medium | patch | The architecture scan searches for quoted fragments such as `"/panel/`, which do not match normal full Go import paths; parse imports and check forbidden path segments. |
| BH-10 | medium | patch | Same verified CI provenance gap as VG-2; source-aware verification must be part of the required workflow. |
| BH-11 | low | patch | Neither `make quality` nor the workflow runs a patch whitespace check; adding a direct diff/show check is small and protects both local staged work and CI commits. |
| BH-12 | low | patch | `HARNESS_STARTING` is printed before `ListenAndServeTLS`, followed by one curl; retry readiness to a bounded deadline to remove the real startup race. |
| BH-13 | medium | patch | Checkout/setup actions use mutable major tags while images and packages are pinned; replace them with the verified official tag commit SHAs. |
| BH-14 | low | patch | README omits a complete runnable JSON, certificate-pin derivation, `explicit_once` manifest, and required tmpfs/mount contract; a direct documentation expansion makes standalone operation reproducible. |
| EH-1 | medium | patch | Even source-aware provenance does not compare each row to `mapSource`, so a valid source can be falsely excluded or remapped; verify destination, disposition, and rationale against the mapping policy. |
| EH-2 | medium | patch | Same verified source-checkout/CI provenance gap as VG-2 and BH-10. |
| EH-3 | low | patch | `parseCSV` accepts an unterminated final quoted field because it never records a closing quote; add an explicit closed-field guard. |
| EH-4 | medium | patch | Same verified architecture-import matcher defect as BH-9, including external `/fixik/` paths. |
| EH-5 | false | reject | The smoke container is launched with `--rm`; `docker stop` removes it, and the trap also removes any still-named container and the config volume. |
| EH-6 | low | patch | The rewritten `.gitignore` dropped standard Go test, coverage, workspace, and editor patterns; restoring them is a direct repository-hygiene correction. |
| EH-7 | medium | defer | Core node validation occurs after provider startup, but the ordering is inherited from the pinned source. A full pre-effect fix needs authority/provider lifecycle redesign rather than extraction-only edits. |
| EH-8 | medium | patch | `go test -race ./...` intentionally skips the physical ENOSPC test; add the required isolated 32 MiB tmpfs invocation so the claimed no-omission gate is actually exercised. |

## Design Notes

Snapshot extraction is intentionally phase one. Keeping source untouched until a published standalone image passes parity avoids breaking Panel deployment while two provider repositories are being established. Shared code is copied rather than factored into a third repository; versioned wire/schema parity is the compatibility boundary.

## Verification

**Commands:**
- `gofmt -l .; go vet ./...; go test -race ./...` -- no output/errors and all packages pass.
- `npm ci --ignore-scripts && npm test` in Cursor worker -- pinned worker suite passes.
- `go list -deps ./cmd/harness-node` plus repository scans -- only local/std/third-party dependencies; forbidden components absent.
- contract checker/generator parity commands -- tracked schema/fixtures unchanged.
- `docker build` with Cursor Dockerfile and container smoke -- image starts non-root/read-only and reports exact Cursor readiness.
- `git diff --check` -- clean patch formatting.

**Result (2026-09-21):**
- Linux pinned Go image: fmt, readonly dependency list, forbidden dependency scan, vet, `go test -race ./...`, architecture test and both CGO-free builds passed.
- Isolated 32 MiB tmpfs: `TestPhysicalFullDiskPreservesAdmissionAndStopReserve` reached real ENOSPC and passed recovery/reserve assertions.
- Pinned Node image: Cursor worker 12/12, Harness schema vectors 90/90, history/transcript checks, generator byte parity and provenance unit tests 4/4 passed.
- Source-aware provenance recomputed 205 source files with 131 included and zero unmatched/ambiguous rows.
- Cursor image rebuilt from the deny-first context; non-root/read-only mTLS readiness passed in both `deny` and `explicit_once`, including the packaged helper self-test and invalid-config fail-closed check.
- `git diff --check`, staged check, commit check and synthetic all-tree index check passed.
