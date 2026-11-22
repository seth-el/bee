# Optional local semantic retrieval

M6 adds an opt-in local semantic layer over the M5 lexical/temporal baseline.
Semantic retrieval is never required for Bee memory to work.

## Local embedder

Bee speaks the OpenAI-compatible `/v1/embeddings` contract. The default local
endpoint is:

```text
http://127.0.0.1:8090
```

with model `Qwen3-Embedding-0.6B`. Rebuild, verification, status, and ordinary
assistant retrieval read the `semantic` settings from `BEE_HOME/config.json`
when present. `rebuild` also accepts explicit `--config`, `--endpoint`,
`--model`, `--api-key-file`, `--timeout-seconds`, `--memory-root`, and
`--index-root` overrides. Query embedding requests are bounded and
time-limited; API keys, if ever needed, are explicit and never logged.

## Derived private index

Rebuild the file-backed vector index explicitly:

```sh
bee memory semantic rebuild
```

The derived index lives under:

```text
~/.bee/state/memory-semantic/
├── manifest.json
└── vectors.jsonl
```

The current format binds the manifest and vector generation with a checksum
and stores a SHA-256 of each record's embedded fields. An incremental rebuild
reuses an unchanged record's vector, embeds only new or changed records, and
drops vectors for records no longer present.
Indexes created by older Bee versions fail closed and require an explicit
`bee memory semantic rebuild` after upgrade.

It stores record IDs and vectors only—not memory text. The manifest records the
memory `events.jsonl` SHA-256, embedding model, dimensions, record count, and
build time. Rebuild is atomic and repeatable; deleting the index does not delete
durable memory.

Inspect or verify it:

```sh
bee memory semantic status
bee memory semantic verify
```

Verification fails when the memory event log changed after the index was built.
Semantic search degrades to lexical results while a replacement generation is
being built.

## Hybrid search

CLI semantic search remains explicit:

```sh
bee memory search \
  --query "what did we decide about mountain travel?" \
  --semantic
```

The normal assistant can opt into the same local path through its config. The
feature is disabled by default. After the initial explicit rebuild, an enabled
assistant starts a coalescing refresh after successful memory mutations.
Retrieval uses a live index handle, so an atomically published generation
becomes available without restarting Bee. Manual rebuild remains available for
initial creation, operations, and repair.
The `memory_search` tool uses the configured hybrid options when the model
explicitly calls it. Ordinary assistant turns do not search or inject memory
automatically; library callers may separately opt into prompt-based recall:

```json
{
  "semantic": {
    "enabled": true,
    "endpoint": "http://127.0.0.1:8090",
    "model": "Qwen3-Embedding-0.6B",
    "index_root": "state/memory-semantic"
  }
}
```

Relative `semantic.index_root` and `semantic.api_key_file` values are resolved
beneath `BEE_HOME`, so the same private config can move between hosts. Rebuild
uses the configured endpoint, model, key file, timeout, memory root, and index
root; command-line values override only the supplied fields.

A missing, malformed, stale, root-mismatched, or model-mismatched index keeps
the assistant usable and produces bounded degraded diagnostics while lexical
retrieval remains authoritative. Bee never uploads durable memory to
any cloud provider.

M5 authorization and temporal filters run before semantic scoring, and the
allowed record IDs are pushed into the file-vector search rather than searching
the global index and discarding unauthorized results afterward:
namespace, actor, sensitivity, currentness, as-of, since/before, and expiry
still control admission. Exact lexical matches remain weighted more heavily
than semantic similarity. If the index, local embedding service, vector result,
or timeout fails, Bee returns lexical results with bounded degraded diagnostics.

When the config opt-in is absent, the normal search path remains
lexical-only. M6 never contacts cloud providers, never sends durable memory
externally, and never turns search results into new memory records.

## Acceptance

M6 tests cover embeddings endpoint shape and bounds, cosine index ranking,
incremental vector reuse, live generation reload, candidate filtering,
stale-index detection, non-keyword semantic retrieval, lexical fallback,
invalid/unknown vector safety, and absence of memory text in the derived index.
