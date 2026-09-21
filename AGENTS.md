# Agent rules

- Work as a senior/staff engineer and on-call SRE. Communicate briefly in Russian; keep code and identifiers in repository style.
- Evidence first: inspect files and run the relevant checks before conclusions. Preserve data and use the smallest scoped diff.
- The repository is Cursor-only. Never add Codex adapter/runtime, Panel, Router, Agent Service client, host tunnel, Fixik, sibling `replace`, shared state, secrets, or production configuration.
- Preserve C1 schema/fixture bytes, SQLite and receipt semantics, mTLS/fencing, exact versions, non-root execution, and read-only image operation. Architecture, API/data contract, shared infrastructure, or deploy changes require explicit approval.
- Any write session uses its own unique `codex/` branch and linked Git worktree unless the user explicitly selects another checkout. Do not touch changes belonging to another session.
- Commit, push, merge, tag, release, deploy, destructive cleanup, and production actions require separate explicit authorization.
- Before handoff run focused checks and then `make quality`; for container changes also run `make image` and `make container-smoke`. A build alone is not runtime or production evidence.
- The source/cutover boundary is documented in `README.md`; source hashes and exclusions are canonical in `provenance/source-manifest.csv`.
