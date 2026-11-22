# Bounded conversation history search

Bee keeps durable conversation transcripts in the existing private session JSONL
files. JSONL is the source of truth and is now persisted faithfully: payload
strings are not blanket-redacted before writing. Session files and rebuildable
sidecars may therefore contain credentials or other secrets and must remain
owner-only/private; this is an intentional local durability tradeoff. Gateway/API
history responses use a separate display projection that redacts secret-shaped
content and structurally re-marshals tool arguments; invalid displayed
arguments become a generic valid marker.

The tool is exposed by the ordinary assistant. It searches only user and assistant message text. Tool
calls, raw arguments, tool output, hidden reasoning, arbitrary files, and
credentials are not returned. Results include the source session ID/title,
message role, event timestamp, and a compact surrounding snippet.

Search is deterministic and bounded: query text, scanned sessions, messages per
session, result count, snippet bytes, and serialized output bytes are capped.
Malformed or unreadable transcript files fail closed rather than being skipped.
The actor must be the same local parent/user/system actor that constructed the
service; child actors cannot search conversation history.

Example model call:

```json
{"query":"what did we decide about the blue key","limit":4}
```

History recall is explicit. Ordinary turns do not scan all sessions by default,
but the normal assistant guidance tells the model to call `conversation_search`
when the user asks what was previously discussed, said, decided, or done.
History results are transient reference data and are never
automatically promoted to durable memory.

Before provider dispatch, every assistant tool-call argument in the assembled
request must be a complete JSON object. Malformed historical arguments fail
locally with an actionable repair diagnostic; they are never sent to a provider.
The explicit offline repair commands are `bee sessions repair --session ID
--dry-run` followed by `bee sessions repair --session ID --apply` after review.
Apply acquires the session lock, creates an atomic owner-only backup, preserves
non-corrupt events, and replaces only invalid arguments with an inert JSON
object. It never executes a historical command and cannot recover the original
secret/token bytes. During provider replay, malformed or structurally incomplete
historical tool-call/result groups are omitted without changing the durable
JSONL. Repair is bounded to one owner-only session file of at most 64 MiB and
100,000 events per invocation.
