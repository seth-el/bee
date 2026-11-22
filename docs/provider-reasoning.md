# Provider reasoning telemetry

Bee keeps OpenAI-compatible `reasoning_content` separate from visible
assistant `content`. Non-stream responses and SSE streams parse the two fields
independently; interleaved tool fragments remain tool calls and are never
executed after a failed/incomplete stream. Existing `Provider` implementations
remain source-compatible through the optional `RichStreamingProvider` adapter.

Each provider invocation gets an ordered step within the foreground `run_id`.
A private `reasoning` session event is appended after every provider response,
including successful no-reasoning and tool-call responses, with effective
provider/profile/model and finish/error diagnostics. Failed streams retain
partial reasoning and any visible partial text as an explicit incomplete
assistant message; incomplete assistant prefixes are preserved for UI/history
but excluded from all future provider replay, and incomplete tool calls are
discarded and not executed. The visible prefix and its reasoning diagnostic
are appended together as one contiguous session-store batch. Persistence
failures fail closed and are joined with provider failures rather than being
ignored.

Store-less applications retain process-local conversation semantics but
serialize the entire run lifecycle, preventing concurrent turns from sharing
messages, hooks, run IDs, or provider-step counters.

The gateway exposes reasoning only through an explicit structural timeline /
history projection and distinct `reasoning_delta` / `reasoning_final` run
events. Raw deltas are coalesced before retained delivery, and the web UI
updates one collapsed DOM `<details>` panel per run/provider step instead of
appending one panel per chunk. Incomplete/timeout diagnostics remain visible;
empty successful panels are omitted. Live retained/SSE delivery and explicit
reasoning history preserve the raw provider reasoning content, subject only to
the existing bounded byte limits. Reasoning remains separate from ordinary
message/tool display projection, provider context, and search. The legacy
unpaged history response and bounded timeline pagination expose reasoning only
as the explicit reasoning timeline field. Durable reasoning bodies are bounded
to 256 KiB during streaming with explicit `truncated` and `original_bytes`
metadata.

The local owner-facing TUI consumes those distinct run events as a visibly
separate, bounded `Thinking` section; reasoning never becomes assistant content
or future provider context. The same transcript renders redacted bounded tool
arguments/results and syntax-colored diff packets so operators can review the
model's intended action and observed result. Because reasoning and tool output
may still contain private workspace material, this diagnostic display is not a
safe publication/export boundary.

SSE accepts either `[DONE]` or a clean EOF after an explicit finish reason;
EOF without either is an incomplete provider response. Fork preserves reasoning
events up to the fork boundary because it copies the append-only source events.
Compaction and repair leave them in the source history while context assembly
ignores them. Export/search behavior remains message-only by default; callers
must explicitly request the reasoning timeline.

An EOF before any HTTP response is classified separately from rejected output.
The foreground agent retries that failure once against the same explicitly
selected provider, without fallback or tool execution, and emits a visible
`provider_output_rejected` event with `stage: "request"`. Direct provider
acceptance probes remain exactly-once operations. Other connection failures,
timeouts, cancellations, and interrupted response bodies are not replayed.
