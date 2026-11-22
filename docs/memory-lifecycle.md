# Memory lifecycle

Phase 7D exposes a focused, provider-neutral `MemoryService` without adding a channel, prompt assembler, background worker, or CLI command set. The local implementation is `LocalMemoryService`.

## Parent-owned writes

`RememberRequest`, `EditMemoryRequest`, and `ForgetRequest` require a valid parent actor, an audit ID, and a non-empty bounded reason. The service validates policy and sensitivity before appending a `MemoryEvent` through `MemoryStore`:

- remember creates version 1;
- edit requires the caller's expected version and creates the next version;
- forget appends `MemoryEventDeleted`, which is a tombstone and does not remove the event history.

The general assistant's `memory_remember` tool reuses this lifecycle service; it
does not write event files directly or expand the `MemoryService` interface. It
accepts only `key`, `content`, `reason`, and an optional bounded `summary`.
Composition binds the parent actor, while the tool forces the global
`durable_memory` namespace, public/default sensitivity, active status, and
explicit mode. It is create-only: exact namespace/key preflight plus a
deterministic record ID make retries and races fail closed rather than silently
overwriting a record. The executor controls whether the approved tool call may
commit immediately (trusted mode), requires the existing approval hook
(prompted mode), or is unavailable (locked mode).

Child actors cannot call these mutations. A stale version returns `ErrMemoryConflict`; the service attempts an audit-only parent conflict event without changing the record.

## Bounded lifecycle views

`List`, `Show`, and `Search` are durable-memory views, separate from `MemoryContextPacket` and session context. Search delegates to `RetrieveRanked`, retaining the existing actor, namespace, subject, expiry, deletion, sensitivity, candidate, result, byte, and semantic-provider bounds. Display bounds cap records, bytes, content, tags, and reasons. Sensitive content is omitted/redacted unless the request explicitly opts in and the actor is authorized. Secret-shaped content is rejected at contract validation and is never a valid export or display value.

## Proposal review

Children can submit only `MemoryProposal` values to the isolated proposal log. They never append a durable record. Parent/system actors can list or show pending proposals, reject a proposal with an auditable review entry, or approve it. Approval copies the candidate into a new parent-created `MemoryRecord`, appends a new parent-owned create event with proposal provenance, and records the decision. Automatic extraction and automatic approval are intentionally out of scope.

## Export/import

`MemoryExport` keeps identity, user profiles, durable memory, proposals, and proposal reviews in separate categories. `MemoryExportJSON` produces deterministic JSON by sorting category arrays. It excludes transient context, secrets, locks, and repository metadata.

Export/import requires parent/system authorization plus an audit reason. Import accepts a typed export or JSON payload and supports dry-run. It validates the complete payload before applying anything, reports malformed data and ID/version conflicts, and never silently overwrites existing identity, profiles, memory records, or proposals. Apply uses parent-owned snapshot writes and memory events. The local seam is intentionally conservative; production backup/restore orchestration remains deferred.

## Deferred work

Automatic proposal approval, background cleanup, retention workers, compaction, irreversible purge, and remote synchronization remain out of scope. The local CLI exposes bounded lifecycle reads, audited proposal review, import/export, and diagnostics without changing ownership rules.

## Runtime context lifecycle

Identity and user-profile snapshots are loaded fresh at the start of each user turn. Durable-memory search is explicit: the model-facing `memory_search` tool uses ranked retrieval when called, while prompt-based per-turn recall is available only through the explicit `AgentOptions.AutoRecallMemory` library option. Any resulting packet is transient, not a lifecycle write, not a session event, and not included in raw traces. `ChildMemoryProjection` provides the only child context handoff: the parent bounds namespaces, sensitivity, count, and bytes before the child receives a copied read-only packet.

## `bee memory search --json`

`--json` swaps the full search result for a small, stable machine contract,
for scripts and context bridges:
`{"results": [{"path", "score", "snippet", "citation", "corpus", "key",
"content", "summary", "tags", "sensitivity", "source", "update_date"}]}`.
Every key is always present: missing values are `""`, and `tags` is a sorted
`key=value` list (`[]` when there are none). `path` and `key` are the record
key, `corpus` is the namespace, `score` is the total ranking score,
`citation` and `source` are the record source, `snippet` is the summary (or
the content if there is no summary), and `update_date` is RFC3339 UTC. At
most 20 results; `content`, `summary`, `snippet`, `citation` and `source` are
cut at 2000 characters each. No hits gives `{"results":[]}` with exit 0; a
nonzero exit means the search itself failed. `--json` is an output encoding
only: sensitive records stay out unless `--include-sensitive` is passed, and
redacted hits are dropped rather than emitted as empty entries. Output without
`--json` is unchanged.
