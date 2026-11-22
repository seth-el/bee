# Human review and normalization

M3 turns M2 candidates into an explicit, reviewable M4 import plan. It does not
write Bee durable memory, modify OpenClaw files, call a model, or approve
anything by default.

## Review loop

Inspect current state:

```sh
bee memory review list \
  --candidates generated/memory-candidates-20260819.jsonl \
  --decisions generated/memory-review-20260819.jsonl
```

Append one explicit decision:

```sh
bee memory review decide \
  --decisions generated/memory-review-20260819.jsonl \
  --candidate candidate-... \
  --action accept \
  --reviewer testuser \
  --reason "confirmed current project decision" \
  --target durable_memory \
  --namespace durable_memory/projects/bee \
  --key provider-routing \
  --effective-at 2026-08-19T00:00:00Z \
  --last-confirmed-at 2026-08-19T16:00:00Z \
  --sensitivity personal \
  --confidence 0.98
```

Supported actions:

- `accept` — current reviewed record;
- `reject` — never import;
- `defer` — needs more information/live verification;
- `mark_historical` — preserve as historical, not current;
- `mark_superseded` — retain with a replacement candidate;
- `replace` — explicit corrected version;
- `merge` — explicit compatible consolidation, never automatic.

Supported targets are `durable_memory`, `user_profile`, and `identity`. The
latter two are deliberate routing decisions; M3 does not collapse identity,
profile, and durable memory into one namespace.

## Normalize for M4

```sh
bee memory review normalize \
  --candidates generated/memory-candidates-20260819.jsonl \
  --decisions generated/memory-review-20260819.jsonl \
  --output generated/memory-normalized-20260819.jsonl \
  --report generated/memory-review-20260819-report.json
```

A missing decision file is valid and means zero candidates are approved. Every
candidate without an explicit decision remains `unreviewed`. Normalization
requires reviewer, reason, target, provenance, final namespace/key where
applicable, content, sensitivity, confidence, effective timestamp, and last
confirmation timestamp.

The normalizer rejects malformed candidates/decisions, secret-shaped content,
missing replacement candidates, invalid timestamps, invalid namespaces/keys,
duplicate final keys, and unresolved conflicts. It preserves source provenance,
M2 artifact hash, decision ID, status, and supersession links. Output artifacts
are owner-only `0600` files and refuse overwrite.

## Safety boundary

The normalized JSONL is **M4-ready**, not an import. It is not passed to
`LocalMemoryService.Import` automatically. M4 must perform a separate dry-run,
validate all records, require an audit ID/reason, report conflicts, and obtain
explicit approval before appending parent-owned memory events.
