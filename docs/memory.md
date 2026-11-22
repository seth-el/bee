# Memory contracts, local backend, bounded retrieval, and lifecycle

Phase 7A defines the contracts and policy boundary for the assistant's identity and memory. Phase 7B adds a private standard-library local backend. Phase 7C adds deterministic bounded lexical retrieval/ranking. Phase 7D adds explicit lifecycle controls while deliberately deferring mandatory embeddings/QMD, automatic extraction or approval, prompt assembly, channels, background workers, compaction, and irreversible purge.

## Four separate kinds of state

- **`IdentityProfile`** is the assistant's stable self-description: name, mission, values, boundaries, and voice.
- **`UserProfile`** is explicit user profile and preference data.
- **`MemoryRecord`** is bounded durable memory with a safe namespace/key, sensitivity classification, timestamps, confidence, version, and creating actor.
- **`MemoryContextPacket`** is transient, bounded retrieval output projected for one request. It is not a session event, a prompt template, or a durable record.

This separation prevents identity instructions, user preferences, durable memories, and transient session material from becoming one unreviewable context blob.

## Safe default policy

`DefaultMemoryPolicy()` provides:

- explicit writes enabled;
- suggested writes disabled;
- sensitive memory disabled;
- secret-shaped content rejected unconditionally;
- no age-based expiry for durable records by default; callers may set an
  explicit policy window or per-record `expires_at`;
- child access set to read-only, with bounded non-sensitive reads and proposal submission but no direct write capability;
- bounded query and transient-context record/byte limits.

Policy validation rejects invalid limits and child access modes. Callers should pass the policy through every validation boundary instead of treating zero values as permissions.

## Parent-owned writes and child proposals

Durable mutations are represented by `MemoryEvent`. `MemoryEvent.ValidateForStore` requires:

1. a valid parent actor;
2. a valid append-only event type;
3. `Explicit == true`;
4. a valid record/version or deletion/conflict payload; and
5. sensitivity approval from `MemoryPolicy`.

Child processes cannot pass this validation. Their only write-shaped contract is `MemoryProposal`, which requires a child actor, a reason, an audit ID, and a validated candidate record. A parent may inspect the proposal, apply its own policy and approval path, and then emit a new parent-owned `MemoryEvent`. `MemoryProposalSink` is intentionally separate from `MemoryStore`.

`MemoryEventConflict` and expected/new version fields make optimistic-concurrency conflicts explicit rather than silently overwriting a record.

## Queries and bounded projections

`MemoryQuery` carries an actor, optional namespace/key/subject/text selectors, sensitivity intent, and caller limits. Child queries cannot request sensitive memory. `MemoryRetrievalResult` is a bounded provider-neutral result; it is not an implementation of ranking or storage. A caller may project validated records into `MemoryContextPacket`, whose item and aggregate limits are enforced independently of session context budgets.

All public contracts expose `Validate` methods and use standard-library JSON tags. `CanonicalJSON` is a small deterministic serialization seam; `encoding/json` sorts string map keys, and round-trip tests protect compatibility. IDs and keys reject path/control characters, namespaces permit only safe slash-separated segments, timestamps and confidence are checked, and obvious credential-shaped values are rejected before storage or context projection.

## Phase 7B local backend

`NewLocalMemoryStore` persists validated parent-owned events under a caller-provided private root, replays them into a deterministic in-memory index, preserves delete tombstones, and fails closed on malformed, truncated, oversized, duplicate, or policy-invalid log data. `NewLocalIdentityStore` and `NewLocalUserProfileStore` use separate atomic private snapshots. `NewLocalMemoryProposalSink` appends validated child proposals to a separate proposal log; it cannot mutate durable records. Cross-process writer locks are conservative: ambiguous or stale lock files are not removed automatically.

Exact retrieval applies actor/policy checks, namespace/key/subject filters, expiry/deletion filtering, bounded token matching, deterministic ordering, and independent result/context bounds. `RetrieveRanked` adds `MemoryScore`, `RankedMemoryHit`, `RankedMemoryResult`, and bounded retrieval options for candidate count, result count, bytes, query text, explanation/reason lengths, and optional namespace diversity. Ranking uses weighted key/tag/summary/content/source token and phrase matching plus confidence and recency; all scores are finite and in `[0,1]`, with ties broken by score, update time, and record ID. `ProjectMemoryContext` and `RetrieveRankedContext` keep transient projections separate from durable records and session history.

The optional `MemoryEmbedder` and `MemoryVectorIndex` seams are opt-in. The local core sends no durable-memory text to an embedder unless the caller explicitly supplies that boundary and sets `AllowMemoryText`; provider errors, timeouts, invalid vectors, and invalid hits produce bounded degraded diagnostics and preserve lexical results. When semantic config is enabled, a coalesced refresher incrementally reuses unchanged vectors and atomically publishes a new generation after memory mutations; a live index handle observes it without a restart. `RebuildMemoryIndex` remains an explicit event-log consistency/replay helper.

## Phase 7D lifecycle service

`MemoryService`/`LocalMemoryService` is a focused service seam over the existing memory, proposal, identity, and profile stores. `Remember`, `Edit`, and `Forget` require a parent actor, a bounded audit ID/reason, policy-approved sensitivity, and explicit writes. They append validated parent-owned events; edits carry an expected version, and forget appends a deletion tombstone rather than destroying history. A stale edit/delete returns `ErrMemoryConflict` and emits a bounded audit-only conflict event when possible.

List/show/search are bounded lifecycle views, not session context. Search delegates to `RetrieveRanked`; list/show cap records, bytes, content, tags, and reasons. Sensitive data is omitted or redacted unless the actor explicitly has parent/system authorization and the request opts in. Child actors cannot mutate durable records. Child proposals remain in the separate proposal log; parent review can list/show pending items, reject them with an audit record, or approve one by creating a new parent-owned record/event with proposal provenance. No child proposal record is appended to the durable memory log directly.

`MemoryExport` keeps identity, user profiles, durable memory, proposals, and proposal reviews in separate categories. `MemoryExportJSON` sorts categories deterministically. `Import` accepts JSON or a typed export, validates the complete payload and all conflicts before applying anything, supports dry-run previews, refuses silent overwrite, and applies only parent-owned events/snapshots. Secrets, transient context, locks, and repository metadata are never export categories. See [memory-lifecycle.md](memory-lifecycle.md).

## Bounded post-turn extraction

Post-turn extraction is an explicit, disabled-by-default feature under `memory.automation`. When enabled, Bee sends only the current prompt, a bounded final assistant response, and bounded tool observations to a configured local child provider (or another explicitly configured local role). It requires one strict JSON object with bounded candidates; markdown, unknown fields, trailing data, secrets, empty content, unsafe namespaces, low-confidence candidates, and evidence not present in the current prompt are rejected.

`auto_save_safe` is also false by default. With it enabled, only high-confidence, direct user-supported facts, preferences, and decisions in `durable_memory` may call the parent-owned `MemoryService.Remember` path. Existing records are checked by exact namespace/key; equivalent records and equivalent review proposals are reported unchanged rather than duplicated across turns. Ambiguous, volatile, conflicting, or otherwise non-auto-save candidates use the existing child-shaped proposal lifecycle and remain separate from durable events until parent review. `bee memory proposals` exposes explicit review, and `/status` exposes content-free extraction counts. Extractor/provider failure emits a bounded result but never fails an already completed assistant turn.

Example opt-in configuration:

```json
{
  "memory": {
    "automation": {
      "enabled": true,
      "auto_save_safe": false,
      "timeout_seconds": 3,
      "max_output_bytes": 16384,
      "provider_role": "child",
      "provider_profile": "local-child-8082"
    }
  }
}
```

Keep `provider_role` at `child` and use a local provider profile (loopback, or `"local": true` on a LAN endpoint; see `docs/specialist.md`); Bee refuses manual/cloud extraction providers. Turn on `auto_save_safe` only after reviewing proposal behavior and accepting the durable-memory audit trail.

Extraction runs after the reply, not inside the run: `run_end`, the session
lease and channel replies (Telegram, the CLI) never wait for it. Its outcome
appears in `/status` ("Memory extraction"), not in the run event stream.
Shutdown waits for pending extractions, each bounded by `timeout_seconds`
(at most 30). A model server on the LAN needs more than the 3-second default.

## Deferred work

The following remain intentionally deferred:

- mandatory embeddings, QMD, or provider-backed ranking;
- loading memory into `AssembleContext` or model-specific prompts;
- deletion/retention workers, automatic compaction, irreversible purge, or automatic migration approval;
- remote/database persistence, channel integration, and backup/restore automation.

Future implementations must preserve these contracts, keep parent ownership explicit, and add operational and audit tests before integration.

## Runtime use

**Keeping recall current.** Each record in a `<bee-retrieved-memory>` packet
shows its last-updated date (`updated=YYYY-MM-DD`) and, when it is not
active, its status. A record has one of three statuses:

- `active`: recalled automatically and returned by default searches.
- `historical`: true once, no longer describing the present. It leaves
  automatic recall and default searches, and is never deleted. A direct
  question about the past ("do you remember…", "last time…") still searches
  historical records, and they reach the model labelled `status=historical`.
- `superseded`: replaced by another record (`memory_supersede`). Never
  recalled automatically, even for a direct question; the replacement
  carries the current fact.

Mark a record historical with `memory_edit` (`fields: {"status":
"historical"}`) or from the shell:

```sh
bee memory set-status --id <record id>[,<id>...] --status historical --reason "why"
```

`--status active` restores it. A status-only edit keeps the record's updated
date; any other edit stamps the current time. The nightly dreaming job has
`memory_search`, `memory_show`, `memory_edit` and `memory_supersede` so it can
retire records that the day's notes show to be out of date.

The model-facing `memory_search` tool is the normal durable-memory recall path. It defaults to current active records; callers must explicitly request `current_only:false` for historical/superseded results. The companion `memory_remember` tool is a compact, explicit create-only adapter over `MemoryService.Remember`: it writes global application durable memory under `durable_memory`, binds the parent actor in composition, rejects secrets and unsafe text, and never silently overwrites an existing namespace/key. Configured hybrid prompt recall normalizes conversational cue words before searching. Retrieved records remain transient reference data and are never persisted into session events or automatically written back to durable memory. An enabled local reranker can reorder only the already-authorized bounded candidates; failure preserves deterministic retrieval order.
