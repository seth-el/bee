# Context kernel

Bee keeps durable session JSONL append-only and treats the provider request as
a projection. The context kernel has three distinct operations:

1. **Admission** estimates the actual messages and tool definitions, then
   reserves configured recovery and output tokens. A provider
   `context_window_tokens` value of `0` means unknown; Bee does not borrow a
   generic window or reject requests using a guessed denominator.
2. **Pruning** replaces individually oversized old tool-result bodies in the
   outgoing request with a deterministic marker the first time they enter the
   stable prefix. Calls and results remain paired and the durable transcript is
   untouched. Bee does not later rewrite additional old results merely to hit
   a target budget; it uses an explicit compaction checkpoint instead.
3. **Compaction** appends a checkpoint event. It records the source boundary,
   source and summary hashes, method, provider/profile/model, and timestamp.
   Source events remain in JSONL and can still be inspected or exported.

Turn-scoped identity, retrieved memory, project/transport instructions, remote
capability grants, and attachment context are placed after the current user
message. Historical messages and sorted tool schemas therefore remain a
byte-stable prefix when those controls change. Raw image references are sent
only for the current image turn; later turns retain durable metadata and the
bounded textual observation rather than reopening and resending image bytes.
Prompt-prefix telemetry is recorded from the final normalized provider request,
after transient landing or recovery instructions are added, and hashes the
provider-visible tool/message wire plus model/template rendering settings.
Content-free cache state (hashes, byte length, epoch, and break reason) is
stored in the durable session so continuity can be checked after restart.
`context_status` reports prefix changes and their classified reason, cache
epoch, the full request estimate, each major contributor, reserves, pruning,
and the latest provider cache-read/cache-write counters without exposing prompt
text.

The ordinary durable parent also appends one trusted `<bee-clock>` control
record at the tail of each admitted user turn. It carries second-precision
UTC/local RFC3339 timestamps, the explicit local weekday, and the configured
`web.time_zone`; it is emitted once per user turn, not once per
provider/tool-loop call. The record has its own append-only `time_sync` event,
is synthesized through the same replay path as plan controls, and is filtered
from visible chat history. The user message and clock event are one contiguous
store append, so failed admission cannot leave an orphan clock. Legacy clock
events without a weekday remain valid and replay byte-for-byte unchanged. This
keeps current time out of the immutable system prefix and preserves provider
neutrality; `current_time` remains available for explicit precision or zone
requests.

Compaction drops clock events at or before the selected checkpoint boundary,
just as it drops the associated pre-compaction messages and plan controls.
Those observations describe obsolete transcript tails; retaining them after a
summary would add stale wall-clock state. Events after the boundary replay in
their original order, producing the same provider-visible durable transcript
as the original turn. Child, storeless, and explicitly disabled agents do not
emit clock records.

Example:

```json
{
  "provider": {
    "type": "openai-compatible",
    "base_url": "http://127.0.0.1:8081",
    "model": "local",
    "context_window_tokens": 131072
  },
  "generation": { "max_tokens": 4096 },
  "context": {
    "reserve_tokens": 4096,
    "output_reserve_tokens": 4096,
    "tool_result_tokens": 1024,
    "summary_tokens": 2048,
    "auto_compact": false,
    "model_summary": true,
    "keep_recent_turns": 2
  }
}
```

`/compact` performs explicit manual compaction for the active durable session.
When `auto_compact` is enabled, Bee attempts compaction only after request
admission detects overflow; it does not compact proactively or in the
background. If no earlier completed turn can be compacted, or the compacted
request still does not fit, the provider is not called and Bee returns
`ErrContextWindowExceeded`.

Normally `generation.max_tokens` is the output reserve. When a role explicitly
omits `max_tokens`, `context.output_reserve_tokens` supplies the admission
reserve instead; zero never means that an unbounded generation is free.
`reserve_tokens: 0` deliberately removes the separate recovery reservation,
and `tool_result_tokens: 0` disables the per-result size cap. Zero
`output_reserve_tokens` and `summary_tokens` use their safe built-in fallbacks.

### Summaries

With `model_summary: true`, the main model writes the summary. The request is
the ordinary request for the history being compacted (same leading system
messages, tool definitions and template settings, so llama.cpp can reuse its
prompt cache) followed by one `<bee-compaction-request>` user message from the
harness. It asks for, in priority order: current state; every owner message,
quoted where short; decisions and standing instructions; work done (files,
commands, commits, errors and fixes); open threads; the next step. An earlier
checkpoint summary is part of that history and is carried forward. The
request may run up to 5 minutes and generate up to
`max(generation.max_tokens, 2 * summary_tokens)` tokens (room for reasoning);
the summary text itself is cut at `summary_tokens`. When the history does not
fit alongside the summary output, the oldest whole turns are left out of the
summary request (the previous checkpoint summary always stays).

If the model call fails, returns no text, or calls a tool, the deterministic
summary is used instead and the checkpoint method records
`model-failed/deterministic-extractive`; the error text is not stored. A
successful model summary records `model-summary`.

The deterministic compactor is extractive: it lists user and assistant
messages (not tool results) in order. When they exceed `summary_tokens`, it
keeps the start (a third of the room) and the end (two thirds), with a line
counting the messages omitted between them.

Callers can instead provide `ContextOptions.Summarizer`, which takes
precedence (`configured-summarizer`). Summaries are historical observations,
not authority, and never erase their source events.

### Keeping recent turns

`keep_recent_turns` (0-16, default 0) leaves the newest N completed turns
before the current one out of a compaction, so they replay verbatim after the
summary. Between turns, `/compact` treats the latest exchange as the current
turn, so it keeps N+1 exchanges. Fewer turns are kept when there is not enough
older history, and a compaction always includes at least one turn newer than
the previous checkpoint, so the previous summary is folded into the new one.
If an automatic (overflow) compaction keeping N turns still does not fit, Bee
compacts once more keeping none before giving up.
