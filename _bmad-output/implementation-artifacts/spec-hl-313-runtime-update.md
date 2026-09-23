---
title: 'HL-313: update Harness agent runtimes'
type: 'chore'
created: '2026-09-22'
status: 'done'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'edecb79ffb7b5f06cd51e99110c47c7392a1e7ba'
context: []
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Harness Cursor and Harness Codex must run current stable, explicitly pinned agent runtimes without weakening their version, protocol, security, state, or receipt fences. Official metadata on 2026-09-22 shows Cursor already current at `@cursor/sdk 1.0.31`, while Codex can move from `@openai/codex 0.153.4` to stable `0.155.1`.

**Approach:** Preserve Cursor at `1.0.31` with native/package evidence and no synthetic bump. Upgrade Codex to `0.155.1`, reconcile generated schemas and strict adapters, then prove both images with zero-turn native probes, mocks, isolated legacy state, and required repository gates.

## Boundaries & Constraints

**Always:** Work only in the two HL-313 worktrees from Cursor `edecb79ffb7b5f06cd51e99110c47c7392a1e7ba` and Codex `8d272057bac35a6af9c366a32222176ef82e0cb3`; keep exact fences; separate native evidence from mocks; preserve auth, IDs/history, receipts, tool/event ordering, approvals, cancel/reconcile, image hardening and isolated state. Record sources, downgrade risk and HL-312 findings.

**Never:** Do not implement HL-312, infer Cursor CLI/Codex Desktop capabilities, touch live volumes or `localhost:18707`, copy secrets, make model turns, install in running containers, deploy, commit, merge, push, or clean up.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Cursor latest | registry latest=`1.0.31` | source remains unchanged; image reports `1.0.31` | fail on any manifest/runtime mismatch |
| Codex upgrade | clean build with `0.155.1` | CLI, identity, schemas and fixtures agree | strict failure on stale fence or protocol drift |
| Legacy restart | copied test state/auth fixtures | same IDs/history/auth and no duplicate receipts/events | stop; document migration and downgrade limits |
| Native capability probe | zero-turn app-server/SDK session | report real model/MCP/effort/speed surfaces | mark unsupported/unavailable; never substitute static catalogs |

</frozen-after-approval>

## Code Map

- `adapters/cursor/worker/{package.json,package-lock.json,worker.mjs}`, `adapters/contract/adapter.go` -- `1.0.31` fence, store/create/resume/tools/auth surfaces.
- `delivery/{Dockerfile.cursor,test-container.sh}`, `contracts/` -- Cursor image and contract proof; provider behavior is partly mocked.
- Codex `adapters/codex/runtime/package{,-lock}.json`, `Dockerfile`, `internal/{harnessadapter,harnessprotocol}` and `api/` -- dependency, executable, identity and generated contract fences.
- Codex `adapters/codex/{session,adapter,events,provider_auth,store}.go` -- strict protocol, MCP isolation, auth/state/tools; `dynamicTools` is start-only today.
- Codex `scripts/smoke.py` and tests -- zero-turn native evidence versus helper-backed evidence.

## Tasks & Acceptance

**Execution:**
- [x] Cursor package/image fences -- prove official latest and built runtime are both `1.0.31`; make no version-only diff.
- [x] Codex runtime package/lock and every exact version fence -- update to `0.155.1` through an ephemeral Docker dependency workflow; regenerate schemas/fixtures and adjust only verified protocol changes.
- [x] Native probes/tests -- compare app-server schemas; exercise zero-turn initialize, start/resume, MCP isolation, model/list, effort/speed and version; retain mocks for tools, approvals, cancel/reconcile and duplicates.
- [x] Isolated state evidence -- reopen legacy fixtures/copies across restart/recreate; document auth/state compatibility and rollback risk.
- [x] YouTrack HL-313/HL-312 evidence -- record current→target, sources, protocol findings and unsupported capabilities without implementing HL-312.

**Acceptance Criteria:**
- Given the built images, when runtime version probes run, then Cursor reports `1.0.31` and Codex reports `0.155.1`, matching all manifests and public identities.
- Given old isolated state and auth fixtures, when each candidate restarts/recreates, then IDs/history/receipts remain stable and no tool/event is duplicated.
- Given native zero-turn probes, when capabilities are queried, then model/MCP/speed/reasoning support is reported from the pinned runtime and mock results are labelled separately.
- Given each repository's required gates, when `make quality`, image build, and Cursor `make container-smoke` / Codex `make smoke` run in Docker, then all pass without model calls.

## Implementation Notes

Planning gate: no intent gap and no irreversible live action. Medium footprint: Cursor evidence only unless a regression is found; Codex package, fences, generated contracts, docs and probes/tests.

Implemented Codex `0.155.1` as an exact package, lockfile, executable, identity, documentation and generated-contract pin. Cursor remains at verified-latest `1.0.31`; its only implementation changes synchronize the shared peer Codex identity and wire-schema digest. Both repositories now use wire schema SHA-256 `5013223c4f77ac7424a486d7223c5321eb7f0e9054fb036e48d6e7748fae1889`.

The Codex smoke probe now obtains native zero-turn evidence for `model/list`, reasoning efforts, service/additional speed tiers, MCP isolation and thread policy. A zero-turn thread has no rollout, so immediate native `thread/resume` reports `no rollout found`; this limitation is preserved in evidence instead of being treated as a successful old-thread resume. Existing helper-backed tests remain the evidence for Harness resume/tool/approval/cancel/reconcile behavior.

An isolated legacy mapping/auth fixture is mounted into disposable volumes. Startup migrates provider auth v1 to v2, restart and recreate preserve node identity plus dialog/attempt records, keep receipt count at zero and advance only process generation. No live state, provider volume, secret or model turn was used. Downgrade remains a backup/restore concern rather than an assumed image rollback.

Evidence was recorded in YouTrack comments HL-313 Comments-7-1022 and HL-312 Comments-7-1023. The HL-312 note explicitly leaves the old-thread MCP A→B test unresolved pending an authorized real rollout/model-turn experiment or a verified native MCP path.

Independent review tightened the native smoke without changing production behavior: exact user-agent semver, fully paginated and validated model capabilities, deterministic selection of a catalog model for zero-turn policy checks, exact discrimination of the no-rollout resume response, a cumulative RPC timeout, and first-load comparison of durable map keys and values. The corrected smoke passed with selected model `gpt-6-astra`; the previously hard-coded probe model `gpt-5.2-codex` was absent from the real 0.155.1 catalog. Follow-up evidence is in HL-313 Comments-7-1025 and HL-312 Comments-7-1024.

## Spec Change Log

## Review Triage Log

| ID | Verdict / route | Verified evidence |
|---|---|---|
| B1 | false / reject | Codex `0.153.4` already used mapping schema v2; the fixture tests cross-runtime compatibility, not a nonexistent mapping-schema migration. |
| B2 | low / reject | The fixture is hand-authored, but strict decoding plus store persistence tests cover the complete schema; capturing a live old image would add machinery without a demonstrated differing serialization. |
| B3 | medium / patch | The first container read accepts any one attempt/dialog and then uses that value as its later baseline, so first-start field corruption could pass. |
| B4 | medium / patch | `Object.values` drops the durable map keys, so a changed composite attempt key or dialog key would not be detected. |
| B5 | false / reject | Auth state/revision may legitimately change after `account/read`; dedicated provider-auth restart/migration tests validate full operations and replay rather than freezing transient observation fields in container smoke. |
| B6 | false / reject | `TestProviderAuthMigratesV1ReceiptsAndReplaysSameCommand` creates non-empty real receipts, downgrades them to v1, reopens, migrates and verifies replay identity. |
| B7 | false / reject | Restart-stable history, receipt replay and duplicate suppression are covered by focused runtime/integration tests; the native container probe intentionally performs zero turns and cannot create provider history/events. |
| B8 | false / reject | Active/reconciling persistence is exercised by mapping-store, dispatch-crash and reconcile tests; the container fixture's terminal state is specifically the no-model compatibility case. |
| B9 | medium / patch | The broad `catch` converts policy-validation failures, timeouts and unrelated RPC errors into an accepted unsupported-resume result. |
| B10 | medium / patch | Python only checks that `threadResume` is a dictionary, so it does not constrain the accepted zero-rollout failure or successful-policy result. |
| B11 | medium / patch | `model/list` reads one page and ignores `nextCursor`, so capability evidence can be incomplete. |
| B12 | medium / patch | Capability evidence accepts malformed reasoning/service/speed entries and can publish null or duplicate identifiers as verified data. |
| B13 | low / patch | The probe does not connect its configured model to the returned catalog; a direct presence assertion is cheap and prevents unrelated non-empty catalogs from satisfying evidence. |
| B14 | low / patch | `userAgent.includes('0.155.1')` also accepts `0.155.10`; an exact parsed version check is a direct correction. |
| B15 | false / reject | Generated schema hashes pin all bytes, the upgrade delta was reviewed as additive, and strict adapter/contract suites exercise consumed RPC shapes; a second semantic allowlist is not required by the intent. |
| B16 | false / reject | The Make wrappers are unavailable on this Windows host, but their exact Docker recipes were executed and the deviation is explicitly disclosed; no gate command was omitted. |
| E1 | low / patch | Same verified exact-version weakness as B14; grouped with the user-agent correction. |
| E2 | medium / patch | Same verified pagination gap as B11; grouped with model-catalog pagination. |
| E3 | medium / patch | Same verified malformed-capability gap as B12; grouped with capability validation. |
| E4 | medium / patch | Same verified broad-resume-catch defect as B9; grouped with resume result validation. |
| E5 | false / reject | The native response exposes the checked thread/policy/sandbox fields, while cwd and the remaining requested controls are not echoed response fields; their absence cannot be validated as proposed. |
| E6 | medium / patch | Six sequential RPC phases each have a 10-second bound while the outer process has only 20 seconds, creating a reachable false timeout despite individually healthy calls. |
| E7 | medium / patch | Same verified durable-key gap as B4; grouped with first-load state comparison. |
| E8 | false / reject | Same claim as B5; the dedicated auth suite, not a transient account observation snapshot, verifies these fields. |
| E9 | false / reject | Same claim as B6; the existing v1 receipt migration/replay test supplies the requested non-empty evidence. |
| E10 | false / reject | Fresh empty mapping initialization remains covered by `openMappingStore`/store tests; replacing the container's empty fixture did not remove that behavior's verification. |
| E11 | false / reject | Cursor stayed on `1.0.31`; its real `JsonlLocalAgentStore` test preserves agent/run/checkpoint data and resumes after legacy workspace migration, while container smoke still verifies the unchanged packaged runtime. |
| V1 | medium / patch | The verification-gap demonstration confirms that a resumed thread with weakened network policy throws inside the broad catch and still yields a passing dictionary. |

## Design Notes

`0.155.1` is stable; `0.157.0-alpha.8` is excluded. Relevant changes include additive app-server fields/RPCs, model-catalog auth scoping, existing-thread plugin refresh, reasoning persistence and MCP status/auth changes. Map verified additions without relaxing strict safety.

## Verification

**Commands:**
- Cursor Docker gates: `make quality`, `make image`, `make container-smoke`.
- Codex Docker gates: `make quality`, `make image`, `make smoke`.
- Candidate-native schema/version/state probes in disposable containers -- expected: zero turns, no secrets, exact versions and stable legacy state.

**Results:** PASS for both image builds; Cursor container smoke and worker tests (12/12); Codex enhanced native/container smoke; both contract, boundary and provenance suites; Docker `gofmt`, `go vet` and `go test -race ./...`; physical ENOSPC; Codex deterministic double-build comparison; and `git diff --check` in both worktrees. On Windows, the Make targets were executed as their equivalent Docker recipes because `make` is unavailable; LF-normalized disposable Docker copies were used for Go checks because the checkout uses CRLF.

Post-review focused verification: corrected Codex smoke PASS; exact changed-file provenance PASS with destination SHA-256 `c358261aa9fca3ee3f7b95a6eb28b52eaaf190d0022ad33a0fca02397d271aae`; changed-file `git diff --check` PASS. No broader source file changed during review.
