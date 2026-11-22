# HTTP session selection

The HTTP session routes are adapters over Bee's existing durable sessions. The
web path does not introduce a second session type and never returns transcript,
message, reasoning, tool, or approval content through the session-list API.

## Existing state boundary

`Application.ListSessions` returns `SessionSummary` values ordered pinned-first
and then newest-first. The exact fields are `id`, optional `title`, `pinned`,
optional `project_id` and `project_root`, `created_at`, `updated_at`,
`event_count`, and optional `parent_id`. These are the same non-message
summaries available to the owner-facing CLI/TUI list.

Durable sessions are append-only private JSONL files with rebuildable private
index sidecars. `SessionStore.SearchSessions` is the existing core primitive
used by the legacy gateway: it performs case-insensitive matching over those
indexes, caps queries at 256 bytes/characters, returns at most 50 pinned-first
then newest-first results, and emits at most three redacted 240-byte snippets
per result. Reusing it adds no new index or persistence subsystem. The HTTP
adapter selects only user and assistant messages, matching the roles its
history endpoint may display; system and tool content remain outside this
boundary. Safe listed titles and session IDs also match without a content
snippet.

`X-Session-Id` remains a required, client-owned external conversation ID. Bee
hashes it into an `openai-http` route key and stores the route-to-durable-session
mapping in the normal private route-binding store. A chat request with a valid
but previously unseen external ID retains the existing auto-create behavior. A
request without the header remains invalid; Bee does not generate client
conversation identities.

## Authenticated HTTP seam

All routes require the existing bearer token, set
`Cache-Control: no-store`, and use strict request shapes:

- `GET /api/sessions` returns at most 200 `SessionSummary` objects. The normal
  pinned/newest ordering determines which entries are retained; the response
  sets `X-Bee-Sessions-Truncated: true` when older entries were omitted.
- `GET /api/sessions?q=...` accepts exactly one non-empty query, returns the
  existing bounded `SessionSearchResult` projection, and never accepts a body.
  The hard 50-result cap is the pagination boundary for this phase; there is no
  cursor or unbounded full-history response.
- `POST /api/sessions` requires a client-minted `X-Session-Id` and an empty JSON
  object. It creates one durable session, binds that external ID, and returns
  the external and durable IDs. An already-bound external ID is a conflict.
- `POST /api/sessions/{id}/select` requires an empty JSON object and
  `X-Session-Id: {id}`. The durable ID listed by Bee is therefore also the
  session's canonical external alias for client switching. The operation
  verifies an existing identical binding or creates it; it never overwrites a
  conflicting route.
- `GET /api/sessions/{id}/history` returns only the latest display-projected
  user and assistant messages for the selected durable session. It excludes
  system prompts, tool calls/results, reasoning, and artifacts. Image
  attachments are metadata-only references with an authenticated bounded
  preview path; payload bytes never enter the history JSON. Each message's
  `created_at` is the authoritative UTC timestamp from its append-only event,
  allowing clients to preserve the original send time across history replay.
  The response is capped at 200 messages, 64 KiB per message, and 2 MiB total;
  `has_more` and `content_truncated` make either bound visible to the client.
  History reads are snapshot-only and never acquire, cancel, or steer a turn.

The mapping remains many-external-to-one-durable: an older client-generated ID
may continue to address a session while the durable ID provides a stable alias
that a session picker can adopt. Existing clients do not need to change.

## Concurrency behavior

Every durable session has a nonblocking turn lease held for the full run across
goroutines and processes. Selection temporarily acquires that lease while
binding the canonical alias. If another run owns it, selection returns `409`
with an explicit busy state; it does not cancel, steer, or otherwise modify the
active run. A client should change its local identity only after a successful
response, then clear stale local state and load the selected session's bounded visible
history. A later race is still protected by the same lease when the next turn
begins.

## Manual verification

1. `GET /api/sessions` with a valid bearer token; verify bounded summaries and
   no message content. Without the token, verify `401`.
2. `POST /api/sessions` with a fresh `X-Session-Id`; verify the session appears
   in the list and the first chat completion with that ID lands in it.
3. Select an existing session; verify `GET /api/sessions/{id}/history` returns
   its user/assistant transcript without system, tool, or reasoning data, and
   the next request returns that durable ID in `X-Bee-Session-Id`.
4. Hold a turn active in another client and select that session; verify `409`
   and that the active turn is not interrupted.
5. Stop the run, retry selection, and verify it succeeds without a restart.
6. Search for text from an older user or assistant message; verify matching
   sessions appear in pinned/newest order with redacted snippets.
7. Send an empty query and one longer than 256 characters; verify both are
   rejected cleanly.
