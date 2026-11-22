# Normalized memory import

M4 is the first phase that can write Bee durable memory. It consumes only the
M3 normalized artifact and review report. It never reads OpenClaw files,
config, OAuth, sessions, or runtime state.

## Dry-run

```sh
bee memory import-normalized \
  --normalized generated/memory-normalized-20260819.jsonl \
  --report generated/memory-review-20260819-report.json \
  --memory-root "$HOME/.local/share/bee/memory" \
  --audit-id bee-memory-import-20260819 \
  --reason "reviewed initial Bee memory import" \
  --receipt generated/memory-import-plan-20260819.json
```

Dry-run is the default. It validates the normalized artifact, M3 report,
provenance, policy, deterministic record IDs, namespace/key conflicts, audit
metadata, and the target local memory store. It writes no memory root. The
optional receipt is a new owner-only `0600` report artifact.

## Apply

```sh
bee memory import-normalized \
  --normalized generated/memory-normalized-20260819.jsonl \
  --report generated/memory-review-20260819-report.json \
  --memory-root "$HOME/.local/share/bee/memory" \
  --audit-id bee-memory-import-20260819 \
  --reason "reviewed initial Bee memory import" \
  --receipt generated/memory-import-receipt-20260819.json \
  --apply
```

Apply reruns validation, then delegates to Bee's existing parent-owned,
conflict-refusing, atomic `LocalMemoryService.Import` path. Records are
created with the parent actor `bee-m4-import`, version 1, deterministic IDs,
explicit imported events, and full M3 provenance.

M4 currently supports only `durable_memory` targets. `identity` and
`user_profile` normalized targets fail closed until dedicated structured
schemas exist; generic content is never guessed into profile fields.

## Provenance

Imported `MemoryRecord` values retain:

- inventory ID;
- M2 candidate ID;
- M3 decision ID;
- source root label and relative path;
- source file/content hashes;
- exact source line range and Markdown heading path.

A compact source string remains for display/search compatibility, but the
structured `provenance` field is authoritative evidence.

## Conflicts and boundaries

M4 refuses existing deterministic IDs, occupied namespace/key pairs, malformed
or stale review artifacts, unsupported targets, invalid timestamps/sensitivity,
secrets, missing provenance, and missing audit ID/reason. It never overwrites
existing memory or partially applies a batch. M4 does not automatically review
candidates, modify the source workspace, enable live memory extraction, or
start vector indexing.
