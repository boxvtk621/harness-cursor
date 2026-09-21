- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Make Cursor native-mapping updates transactional in memory as well as on disk.
  evidence: `putIntent`, `activate`, and `terminal` mutate `store.contents` before `writeLocked` succeeds, so a pre-rename failure leaves the live process ahead of durable state; this is inherited from source snapshot `d5edfb2`.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Define durable handling for a failed terminal mapping write.
  evidence: `finishRuntime` publishes the terminal result and ignores `store.terminal` failure, allowing restart state to remain active; the behavior predates the repository extraction.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Add a bounded, idempotency-safe retention policy for terminal Cursor attempts.
  evidence: Attempts are never removed from the live map or `native-mapping.json`; safe pruning requires an explicit retry and recovery retention contract.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Bound and semantically validate the private Cursor native-mapping file.
  evidence: Startup uses unbounded `os.ReadFile` and validates only schema version/non-nil maps; limits and key/reference/state invariants require migration-compatible design.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Decide and enforce a minimal environment allowlist for the Cursor worker.
  evidence: `exec.Cmd.Env` is unset, so the worker inherits every Harness variable; production environment inventory is needed to establish whether unrelated credentials are exposed.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Determine whether the pinned Cursor SDK needs process-group termination.
  evidence: The bridge kills only its direct Node child; inspect the pinned SDK process tree before adding platform-specific group-kill semantics.
- source_spec: `C:\Users\boxvt\Documents\HomeLab\harness-cursor\_bmad-output\implementation-artifacts\spec-hl-299-harness-cursor-repository.md`
  summary: Move all authority validation ahead of provider effects.
  evidence: The provider starts before `node.Open` validates node identity and the durable data directory; a complete fix needs authority/provider lifecycle separation and is inherited from the source design.
