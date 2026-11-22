# Read-only memory candidates

M2 turns an M1 inventory manifest into bounded, reviewable candidate sections.
It is not a durable-memory import and does not modify OpenClaw or Bee memory.

## Command

```sh
bee memory candidates \
  --inventory generated/memory-inventory-20260819.json \
  --output generated/memory-candidates-20260819.jsonl \
  --report generated/memory-candidates-20260819-report.json
```

Without `--output`, the command emits a JSON object containing the report and
candidate array to stdout. With `--output`, `--report` is required. Both
artifacts are new owner-only `0600` files and refuse overwrite.

By default M2 processes only M1-included Markdown/plain-text files classified as
`curated_long_term`, `curated_topic`, or `daily_note`. Archives require
`--include-archives`; source types can be explicitly selected with
`--source-types`.

## Safety and provenance

Before reading a source, M2 locates it through the inventory root and relative
path, rejects path escapes/symlinks, and recomputes the file SHA-256. A changed
or missing source is reported as stale and produces no candidates.

Each candidate carries:

- inventory ID and extraction batch provenance;
- root label and relative source path;
- source file hash and normalized-content hash;
- exact line start/end;
- Markdown heading hierarchy;
- suggested category, namespace, key, summary, sensitivity, and confidence;
- review flags and explicit status.

Candidate status is separate from durable memory state:

- `candidate` — deterministic extraction produced a plausible section;
- `review_required` — volatile, historical, todo, or uncertain language needs review;
- `duplicate` — exact normalized-content duplicate of an earlier candidate;
- `rejected` — section was unsafe or exceeded a bound.

Secret-shaped sections are rejected with empty `content`; the offending text is
never serialized into candidate JSONL. M2 does not call a model, infer current
truth, merge conflicts, approve records, or create `MemoryRecord` values.

## Deterministic extraction

Markdown headings produce sections with heading paths and exact source lines.
Non-heading text is split into bounded non-empty paragraph blocks. Classification
is conservative and heuristic-only: project decisions, preferences, identity,
person/pet facts, infrastructure, todos, historical notes, and general text.
Infrastructure and historical/todo/uncertain material receives review flags.

Exact normalized-content duplicates are grouped. Similar or contradictory text
is not silently merged; it remains separate for M3 review.

Default bounds are 2,000 files, 5,000 sections, 32 KiB per section, and 64 MiB
of candidate content. All bounds are explicit and reject invalid widening.

## Boundary

M3 is the next phase: human review and normalization of candidate categories,
keys, timestamps, statuses, sensitivity, confidence, supersession, and conflicts.
Only a later approved M4 import can create parent-owned durable memory records.
