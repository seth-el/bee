# Local memory backend

Bee's Phase 7B/7C backend is deliberately local, inspectable, and standard-library-only. Phase 7C adds ranked retrieval in `memory-retrieval.md`; this document focuses on persistence and replay.

## Storage root and files

Callers must provide a dedicated root to `NewLocalMemoryStore`, `NewLocalMemoryProposalSink`, `NewLocalIdentityStore`, or `NewLocalUserProfileStore`. Bee does not infer a path from the repository and rejects roots that overlap a detected Git repository/worktree, contain symlinked path components, or have group/other permissions. The root is created private (`0700`); files are private (`0600`).

The durable-memory store uses:

- `events.jsonl`: append-only parent-owned `MemoryEvent` records, including tombstones and audit-only conflicts;
- `proposals.jsonl`: validated child `MemoryProposal` values, never durable record mutations;
- `proposal_reviews.jsonl`: auditable parent/system approval or rejection decisions;
- `identity.json`: atomic identity snapshot;
- `profiles/<id>.json`: atomic user-profile snapshots.

`LocalMemoryService` is the lifecycle seam over these stores. It does not add a second persistence path or mix identity/profile data into durable memory.

Identity/profile snapshots never share the event log. Snapshot writes use a temporary file in the same directory, `fsync`, `rename`, and directory sync.

## Replay and writes

Startup replays the complete event log. Later reads and writes reuse the validated in-memory state while the event file size and modification time are unchanged; cross-process changes invalidate that cache and trigger a full fail-closed replay. Every replayed line must be bounded, complete JSONL, and pass `MemoryEvent.ValidateForStore(policy)`. Duplicate event IDs, malformed/truncated/oversized lines, invalid versions, and impossible transitions fail closed. Created records begin at version 1; updates require the current expected version and an increasing new version; deletes create in-memory tombstones and never erase audit history. Conflict events are audit-only.

The process uses a per-root mutex plus an exclusive lock file containing bounded metadata. Existing lock files—including malformed, stale, or live-looking metadata—are treated as held. No automatic stale-lock removal is attempted; an acquired lock must be explicitly released.

Child proposals are validated and appended only to `proposals.jsonl`. Listing/showing pending proposals is parent/system-only. Approving one requires an independent parent-owned, explicit `MemoryEvent` whose record is created by the parent and carries proposal provenance; rejecting one appends an auditable review decision. There is no proposal path that mutates durable records directly.

## Retrieval

`Retrieve` validates actor, policy, filters, and limits before reading the in-memory replay index. It excludes tombstones, expired records, and sensitive records unless explicitly authorized. Namespace, key, and subject selectors are exact. Text uses bounded case-insensitive token/exact matching only. Ordering is deterministic: match score, newest update, record ID, then key. Results and `RetrieveContext` projections are independently validated against record and byte bounds.

`MemoryService` provides bounded lifecycle list/show/search views and deterministic category-separated export/import. Import validates identity, profiles, memory, proposals, reviews, malformed payloads, duplicate IDs, and existing-state conflicts before applying anything; dry-run performs the same checks without mutation. Existing IDs are never silently overwritten. Export excludes secrets, transient context, locks, and repository metadata. No mandatory embeddings, QMD, automatic approval, prompt assembly, channel integration, cleanup worker, automatic compaction, or irreversible purge is included. Optional semantic indexing is incremental, atomically published, and automatically refreshed only when explicitly enabled; it never replaces lexical fallback. Maintenance is explicit: callers may inspect replay diagnostics, call `RebuildMemoryIndex`, run `bee memory doctor`, and manage backups outside the store.
