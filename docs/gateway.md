# Bee gateway (web-only)

`bee-gateway` is a long-lived web-only process that composes one ordinary Bee assistant and exposes a small web UI/API. Telegram polling is deferred to the separate `bee-telegram` executable. By default the gateway listens on `127.0.0.1:18790` and serves the embedded UI at `/`.

For deliberate LAN exposure, pass `--listen 0.0.0.0:18790` and restrict access with the host/VM firewall. The web UI currently has no application authentication, so do not expose the wildcard listener beyond a trusted network.

When the ordinary assistant uses trusted policy, it can expose the
mutation-capable `exec` and `terminal` process tools described in
[terminal.md](terminal.md). Plan mode gates those tools when enabled, but it is
not an authentication boundary. They provide arbitrary command execution as
the Bee OS user, including ordinary SSH and interactive PTY sessions. On an
unauthenticated wildcard listener, every client that can reach the gateway
receives that authority; trusted-network isolation is therefore mandatory
rather than advisory.

```sh
bee-gateway --config /path/to/bee.json
```

The gateway admits at most `web.max_concurrent_runs` active turns across all
sessions (default 4, maximum 64). Admission is non-blocking: an overloaded
gateway returns `429`, while an overlapping turn for the same durable session
returns `409`. Deleting a session with an active turn also returns `409`.
Capacity is released when the run event stream closes, including cancellation.

```json
{
  "web": {
    "max_concurrent_runs": 4
  }
}
```

### Message avatars

The web UI shows the Bee SVG beside assistant messages and a `Y` initials
fallback beside user messages. Optional `web.user_avatar_file` and
`web.bee_avatar_file` values can override those sources. Bee reads each
configured file once at gateway startup, limits it to 2 MiB, and accepts only
PNG, JPEG, GIF, or WebP image content. Missing, oversized, non-regular, or
unsupported files prevent startup with a validation error rather than exposing
an arbitrary filesystem path.

When configured, the files are served only through the fixed same-origin
endpoints `/assets/user-avatar` and `/assets/bee-avatar`; those endpoints do
not accept a path or filename from the request. If no file is configured, the
user initials and built-in Bee SVG remain the defaults.

```json
{
  "web": {
    "user_avatar_file": "/home/bee/.bee/assets/user-avatar.jpg",
    "bee_avatar_file": "/home/bee/.bee/assets/bee-avatar.jpg"
  }
}
```

The brand header uses the Bee SVG responsively. The home/empty state remains
text-only so the mascot is not duplicated in the center of the page.

### Message timestamps

Message timestamps use `web.time_zone`, an IANA time-zone identifier. The
blank/omitted default is `UTC`. Use a named zone such as `Europe/Berlin`
rather than a fixed offset so daylight-saving transitions render correctly. Invalid identifiers, control characters, or overlong values
prevent configuration validation/startup.

```json
{
  "web": {
    "time_zone": "Europe/Berlin"
  }
}
```

The API is intentionally bounded and synchronous at the HTTP boundary while runs are tracked asynchronously for cancellation and SSE delivery:

- `GET /healthz`
- `GET /readyz`
- `GET /api/metrics` for process-lifetime, content-free run/admission counters
  and the current active-run gauge
- `GET /api/approvals` for bounded pending owner decisions, including their
  opaque approval ID and run/session association
- `POST /api/approvals/{id}` with `{ "decision": "approved"|"approved_session"|"denied" }`
  (`approved_session` additionally grants the request's `session_grant`
  directory for the rest of the session; see `docs/permissions.md`);
  the response acknowledges the decision while the run event remains the
  authoritative final status
- `GET /api/interaction-policy` (optionally with `?session_id=...`) for the
  current display visibility and effective plan-mode state
- `GET /api/sessions` (optionally `?project_id=...` or `?unassigned=true`)
- `GET /api/sessions/search?q=...` for bounded, case-insensitive durable message search; it accepts the same optional project filters and returns pinned-first, newest-updated sessions with short role/snippet matches
- `POST /api/sessions` with `{ "title": "optional", "project_id": "optional", "project_root": "optional" }`; creating in a completed project returns `409` until it is reopened
- `PATCH/PUT /api/sessions/{id}` with any of `{ "title": "new title", "pinned": true|false, "project_id": "project-id"|null, "project_root": "optional" }`; metadata changes are durable and old session files remain readable
- `GET /api/sessions/{id}` or `GET /api/sessions/{id}/history` for bounded durable chat replay
- `GET /api/sessions/{id}/artifacts` for safe artifact metadata linked to one session
- `POST /api/sessions/{id}/attachments` as bounded multipart upload (`file` field); uploads are MIME-sniffed, basename-normalized, stored as immutable artifacts, and returned with `uploaded`, `processing`, `ready`, or `failed` status
- `GET /api/sessions/{id}/attachments` and `GET /api/attachments/{id}` for bounded attachment metadata/status; attachment payloads remain downloadable through their linked artifact
- `GET /api/artifacts/{id}` for metadata, `GET /api/artifacts/{id}/download` for the immutable payload, and `GET /api/artifacts/{id}/preview` for bounded text-only previews
- `DELETE /api/sessions/{id}` to permanently remove one durable session by path-safe ID
- `GET /api/projects`
- `POST /api/projects` with `{ "name": "...", "description": "optional", "root_path": "optional", "create_directory": false }`; creation is metadata-only by default. Set `create_directory: true` to create a safe slug-named directory under Bee's canonical `.bee/workspace/projects` root and store its absolute path in `root_path`. Do not combine `create_directory: true` with an explicit `root_path`.
- `PATCH/PUT /api/projects/{id}` with optional fields including `{ "completed": true }` to mark complete or `{ "completed": false }` to reopen; completed projects remain viewable and report `completed_at`
- `DELETE /api/projects/{id}` to remove metadata only; it returns `409` while sessions remain assigned and never deletes `root_path`
- `POST /api/sessions/{id}/messages` with `{ "prompt": "...", "model": "optional", "attachment_ids": ["att-..."] }`; the prompt remains durable text while only metadata references are written to session JSONL
- `GET /api/runs/{id}/events` (bounded Server-Sent Events)
- `GET /api/runs/{id}` for bounded run status/replay metadata (`running`/`done`, terminal observation, cursors, and overflow/gap state)
- `POST /api/runs/{id}/cancel`

Request bodies are limited to 64 KiB and JSON fields/events are bounded. The gateway reuses Bee's `Application`, `SessionStore`, route/session contracts, policy, approvals, memory, coding, and fallback semantics; it does not add another orchestration layer.

Run SSE is an owner-display projection rather than a copy of private durable
events. `tool_display: off` omits tool calls/results; compact mode retains safe
call metadata but omits result bodies; full mode returns bounded structurally
redacted details. `thinking: false` omits reasoning events. Approval events
carry explicit `pending`, `approved`, `denied`, or `error` status, and a run
emits at most one copy of each meaningful approval transition. Raw provider
replay and durable session records are not rewritten by this projection.

Runtime metrics contain counts only: started, succeeded, failed, canceled,
active, session-busy, and admission-rejected. They contain no prompts, tool
arguments, provider output, or filesystem paths and reset when the process
restarts. The same application lifecycle emits bounded JSONL diagnostics at
`BEE_HOME/state/diagnostics/runtime.jsonl`.

Deliberate downloadable outputs use the provider-neutral `create_artifact` tool. Payloads are stored below `.bee/state/artifacts`, separate from session JSONL and project roots, with path-safe IDs, safe filenames, MIME metadata, size, SHA-256, timestamps, and session/run linkage. The tool is not a file watcher: ordinary files are not automatically registered. Artifact payloads are immutable and have no arbitrary-delete API.

Uploaded text/code receives a bounded text preview as transient provider context; the original artifact remains exact and is never executed automatically. Audio transcription is disabled unless `speech.transcription_script` names an explicitly configured local executable. A successful transcript is a derived text artifact linked from the attachment and injected with bounded context. Images are stored and previewed, then passed as bounded base64 data-URI content parts only when the selected OpenAI-compatible provider explicitly declares image input; providers without that capability retain the clear stored-but-not-interpreted behavior.

For deployments that deliberately set `specialist.image_sidecar_enabled`,
images are instead interpreted by the configured local child specialist with
explicit `image_input: true`. The bounded `{summary,claims}` result is
transient turn context; image refs are not sent to the main provider, and a
sidecar failure never falls back to sending image bytes to the main model.
Original attachment metadata and artifacts remain authoritative.

The embedded UI names a default session from its first ordinary prompt, keeps
the durable session ID unchanged, and replays user/assistant history when an
older conversation is selected. Local `/status`, `/help`, `/plan`, `/compact`,
`/subagents`, and `/tasks` prompts stay on the normal run/SSE stream and do not call the provider;
`/tasks` reports the latest recorded session plan when one exists, while
`/subagents` reports bounded child capacity and configured coding routes.

### Markdown tables

Final assistant messages support bounded GitHub-Flavored Markdown pipe tables.
The first row is the header and the second row must use at least three hyphens
per column, with optional alignment markers such as `:---`, `---:`, or
`:---:`. Leading and trailing pipes are optional. Cell contents retain the
gateway's safe inline Markdown behavior (including bold, links, and inline
code); escaped `\|` renders as a literal pipe, and pipes inside inline-code
spans do not split a cell.

Tables are rendered with a keyboard-focusable, horizontally scrolling local
wrapper so narrow screens do not acquire page-wide overflow. A small scroll
hint appears only when additional columns are hidden off-screen. The first slice
deliberately requires consistent column counts, does not support rowspan or
colspan, does not parse fenced code blocks, and falls back to ordinary text
when table bounds or syntax checks fail. Bounds are enforced for columns, rows,
cell size, and total table size.

### Resumable run streaming

Run event SSE uses stable monotonically increasing cursors in `id:` fields. New
clients should reconnect with `?after=<last-cursor>` (or `Last-Event-ID`);
legacy `?from=<next-index>` remains accepted. Cursors are strict unsigned
decimal values. A `409` response with `gap: true` means the requested replay
is no longer retained and the client must reconcile from durable session
history. The run status endpoint exposes `next_cursor`, `oldest_cursor`,
`terminal_observed`, and `replay_gap` so a client can distinguish a temporary
transport interruption from completed work.

The gateway retains a bounded 1024 ordinary events plus two terminal slots.
Overflow is explicit in status and replay requests that cross a missing cursor
are rejected rather than silently pretending continuity. Oversized ordinary
events are represented by a bounded `run_gap` event; oversized terminal events
are projected to a compact terminal event because streamed deltas and durable
history carry the full response. SSE comments are emitted every 15 seconds,
and each connection still rolls over after 10 minutes; clients should resume
from their last cursor across that rollover.

## Telegram deferred

Telegram is not configured, imported, or started by `bee-gateway`. The web gateway owns only its one Bee `Application`, HTTP server, active web runs, and configured extensions. Telegram remains a separate future adapter in `cmd/bee-telegram`, using the same general assistant composition and shared compatibility helper. Do not treat the web gateway as a Telegram cutover.

## systemd user unit

`deploy/bee-gateway.service.example` is a reviewed web-only template. Copy and
adapt it explicitly; Bee does not install it automatically. The current
deployment can run it as a private `bee-gateway.service` with the LAN
listener on port 18790. Use `systemctl --user status|restart
bee-gateway.service`, preserve the prior binary before upgrades, verify
`/healthz` and `/readyz`, and keep the wildcard listener firewall-restricted.
Telegram polling is intentionally deferred and is not a gateway service
option.
