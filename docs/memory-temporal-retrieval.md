# Temporal memory retrieval

M5 hardens Bee's deterministic lexical retrieval with explicit currentness and
time bounds. Vector/QMD remains optional and is not part of this gate.

## Record status

Durable records may carry:

- `active` — current eligible memory;
- `historical` — retained context that is not current;
- `superseded` — replaced by another record, with `superseded_by` pointing to
  the replacement;
- expiration remains derived from `expires_at`.

Legacy records with no status remain compatible and are treated as `active`.

## Query bounds

`MemoryQuery` supports:

- `current_only` — only active, non-expired records;
- `as_of` — conservative point-in-time query;
- `since` — inclusive `updated_at` lower bound;
- `before` — exclusive `updated_at` upper bound.

The CLI exposes these through `bee memory search`:

```sh
bee memory search \
  --query "provider routing" \
  --current-only \
  --as-of 2026-08-19T21:00:00Z
```

```sh
bee memory search \
  --query "provider routing" \
  --since 2026-08-17T00:00:00Z \
  --before 2026-08-20T00:00:00Z
```

An as-of query excludes records created or updated after the requested time.
This is deliberately conservative: the current local event replay does not
reconstruct historical versions of a record updated after that time. A later
M5 enhancement may replay event history into an exact historical snapshot.

Superseded/historical records remain retrievable for historical queries but are
excluded by `current_only`. Expired records remain excluded by normal policy.

For one-off local inspection, `bee memory list|show|search` supports the read-only overrides `--retention-days N` (nonnegative; `0` disables age retention) and `--include-sensitive`. Existing secret/credential validation is unchanged, and these flags are not accepted by import/export.

## Acceptance

The M5 tests prove current filtering, as-of filtering, since/before bounds,
superseded historical visibility, expiration behavior, deterministic lexical
ranking, sensitivity policy, and transient-context separation. No provider,
QMD service, or external vector index is required.

Only after this lexical/temporal gate passes should Bee add an optional vector
adapter. Vector failures must degrade to lexical results and must never make
semantic search mandatory.
