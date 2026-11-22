# Bounded memory retrieval

`LocalMemoryStore.RetrieveRanked` is the additive ranked-retrieval boundary. Existing `MemoryRetriever`, `MemoryStore`, and exact `Retrieve` callers remain compatible.

## Request and bounds

The method accepts the existing actor-scoped `MemoryQuery` plus `MemoryRetrievalOptions`:

- `MaxCandidates` limits the deterministic post-filter candidate window;
- `MaxResults` limits returned hits;
- `MaxBytes` limits encoded ranked-hit bytes;
- `MaxQueryTextBytes` limits query text independently of the existing contract maximum;
- `MaxExplanationBytes` and `MaxReasonBytes` cap transient diagnostic strings;
- `MaxPerNamespace` and `Diversity` optionally prevent one namespace from occupying every result.

Zero values use safe policy/query limits. Values above policy or absolute limits are rejected rather than silently widened. Result records remain `MemoryRecord` values; explanations, scores, and diagnostics are transient.

## Filter-before-score privacy

Before any ranking work, the local backend rejects deleted/tombstoned records, expired records, records outside the requested namespace/key/subject, and records disallowed by sensitivity policy. Lexical-only retrieval also rejects nonmatching text before scoring. Hybrid retrieval may retain authorized non-lexical records for semantic scoring, but allowed record IDs are pushed into filtered vector search. Child queries cannot request sensitive records, even when the parent policy permits sensitive storage.

## Lexical score

The deterministic scorer examines only bounded durable fields:

1. key (highest weight);
2. sorted tag keys/values;
3. summary;
4. content;
5. source.

Token coverage and exact phrase presence are weighted by field. Confidence and recency are added as bounded components. The final `MemoryScore` and each component are finite and in `[0,1]`. Ties are stable by descending score, descending `UpdatedAt`, then ascending record ID.

All authorized lexical matches are scored before the candidate limit is applied, so an older exact match cannot be displaced solely by newer unrelated records. `Total` reports all post-filter matches; `Diagnostics.Candidates` reports the bounded ranked candidate window, and `Truncated` identifies candidate/result/byte/diversity loss.

## Optional semantic seam

`MemoryEmbedder` and `MemoryVectorIndex` are optional. The core works with neither. When both are explicitly supplied, only the query text is sent to the embedder by default (`AllowMemoryText == false`); durable record text is not exported by the local core. Provider errors, context timeouts, invalid vectors, unknown/duplicate/invalid vector hits, or unavailable components set `Diagnostics.Degraded` with a bounded reason and return lexical hits instead.

The core does not call network services or require QMD. The optional assistant semantic configuration owns a local, coalesced background refresher and live file index; lexical retrieval remains authoritative during refresh or failure. Event-log state is replayed when its file metadata changes and otherwise served from the validated in-process cache. `RebuildMemoryIndex` is an explicit replay/checkpoint seam.

## Transient projection

`RetrieveRankedContext` projects only bounded record fields and the scalar score into `MemoryContextPacket`. Ranking explanations and provider diagnostics do not become durable memory, session history, or prompt instructions. Automatic prompt assembly remains deferred.

## Deferred work

Semantic search remains optional, and proposal approval, automatic compaction, irreversible purge, and remote synchronization remain explicit or deferred.
