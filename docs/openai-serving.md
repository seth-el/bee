# OpenAI-compatible HTTP serving

Bee can expose one configured general-assistant profile through an
OpenAI-compatible HTTP surface. This is an operator-controlled, authenticated
interface to the complete Bee harness: one `POST /v1/chat/completions` request
runs one full agent turn, including every internal provider call and tool
execution, and returns only the assistant-facing result.

This endpoint is RCE-as-a-service whenever its policy and tool registry permit
host commands. It binds to `127.0.0.1:18791` by default. Binding to `0.0.0.0`, a
LAN address, or a public interface is an explicit operator decision and should
be paired with an external TLS boundary and network access controls.

## Configuration and startup

Create a bearer token in an owner-only file:

```sh
umask 077
printf '%s\n' 'replace-with-a-long-random-token' > ~/.bee/secrets/serve.token
chmod 600 ~/.bee/secrets/serve.token
```

Add a `serve` block to the primary config:

```json
{
  "serve": {
    "enabled": true,
    "addr": "127.0.0.1:18791",
    "token_file": "~/.bee/secrets/serve.token",
    "policy": "prompted",
    "approval_mode": "manual",
    "max_concurrent": 8,
    "heartbeat_seconds": 15,
    "request_timeout_seconds": 1800
  }
}
```

The block is merged with the rest of the normal Bee config. Start it with:

```sh
bee --config ~/.bee/config.json
```

Alternatively leave `serve.enabled` false and start explicitly:

```sh
bee --config ~/.bee/config.json --serve \
  --serve-token-file ~/.bee/secrets/serve.token
```

Flags override the corresponding serving settings:

- `--serve-addr`
- `--serve-token` or `--serve-token-file` (the file is preferred because a
  command-line token may be visible in the process list)
- `--serve-policy`
- `--serve-max-concurrent`
- `--serve-heartbeat-seconds`
- `--serve-request-timeout-seconds`

The ordinary `--profile`, scope, coding-root, and `--full-machine` composition
flags still select the one assistant served by the process. Serving is a run
mode and cannot be combined with a foreground prompt, JSON/TUI/RPC mode, a
session selector, per-request CLI model override, plan-mode override, or SSH
grant.

## Endpoints

Every endpoint requires `Authorization: Bearer TOKEN`, including health:

- `POST /v1/chat/completions`
- `GET /v1/models`
- `GET /api/approvals`
- `POST /api/approvals/{id}`
- `GET /api/interaction-policy`
- `GET /api/sessions`
- `POST /api/sessions`
- `POST /api/sessions/{id}/select`
- `GET /api/sessions/{id}/history`
- `POST /api/sessions/{id}/attachments`
- `GET /api/attachments/{id}/preview`
- `GET /healthz`


`/v1/models` returns the single assistant profile selected for this serving
process. Clients send that profile ID as the request `model`; Bee keeps the
profile's provider model internal.

Example:

```sh
curl http://127.0.0.1:18791/v1/chat/completions \
  -H 'Authorization: Bearer replace-with-a-long-random-token' \
  -H 'Content-Type: application/json' \
  -H 'X-Session-Id: my-client-conversation-42' \
  -d '{
    "model": "local-main-8081",
    "messages": [{"role":"user","content":"Inspect the current project"}]
  }'
```

`stream: true` returns OpenAI-style `chat.completion.chunk` SSE data and a
terminal `data: [DONE]`. During provider or tool stalls the stream emits
`: ping` comments at the configured interval. Internal tool calls, tool
results, approvals, and reasoning events are not serialized into OpenAI chat
completion chunks. The additive authenticated `/api/approvals` routes expose
the same owner-decision contract as the legacy web gateway without changing
the OpenAI response schema.

`GET /api/sessions` without a query remains the bounded metadata-only listing.
With exactly one non-empty `q` parameter it performs the existing core session
search over listed titles/IDs and durable user and assistant messages. Queries
are capped at 256 bytes/characters; results are capped at 50 in pinned-first,
newest-first order,
with at most three 240-byte redacted snippets each. System and tool content is
not searched or returned on the HTTP boundary. Empty, duplicate, unknown, or
oversized query parameters are rejected. The endpoint never accepts a body and
all responses remain `no-store`.

## Conversation and session mapping

`X-Session-Id` is required and is an external client conversation ID, not a
Bee session ID. Bee hashes it into a route key and durably maps that key to one
random Bee session in `~/.bee/state/openai-http-routes.json`. The Bee session
itself remains in the ordinary session store. Different conversation IDs get
different sessions and can run concurrently. An overlapping request for the
same conversation returns `409`; the server never runs two requests against
one durable session concurrently. Once `max_concurrent` turns are active,
additional conversations return `429`.

Bee's durable transcript is authoritative. OpenAI clients normally resend the
full conversation, but Bee deliberately does not replay those prior user,
assistant, or tool messages. For each request it uses:

1. the durable history already associated with `X-Session-Id`;
2. system/developer text from the current request as ephemeral system context;
3. only the newest user message as the new durable turn.

This prevents double-counting client history. Request system/developer text is
not written into the session and therefore must be resent when the client
wants it applied to later turns.

Checkpoint continuation uses the optional top-level `checkpoint_id`
extension. The value is a bounded identifier previously returned by structured
`POST /api/commands` handling for `/continue`; it is valid only for the durable
session selected by `X-Session-Id`. Bee maps it directly to
`RunRequest.CheckpointID` and otherwise preserves the normal chat-completion
SSE and lifecycle contract. Missing, malformed, or cross-session checkpoint
references fail before provider work begins.

## Image attachments

The chat-completion parser continues to accept strings and text/input-text
content parts only. Image input uses a separate authenticated upload so
base64 payloads do not consume the 1 MiB chat JSON envelope and the server
never fetches client-supplied URLs.

`POST /api/sessions/{id}/attachments` accepts exactly one multipart `file`
field. The request's required client-owned `X-Session-Id` must already map to
the durable session in the path, and the target's turn lease must be idle.
The upload limit is 8 MiB. PNG, JPEG, GIF, and WebP are the complete allowlist;
the declared MIME type, detected magic bytes, decoded format, dimensions, and
full payload must agree. Source images are capped at 25 megapixels and 32,768
pixels on either axis. Authentication, busy-session behavior, and `no-store`
semantics are identical to the other owner web routes.

The response is bounded metadata: opaque attachment ID, safe filename, media
type, size, and an authenticated preview path. A subsequent completion may add
up to four unique IDs in the top-level `attachment_ids` array. Bee verifies
that every reference belongs to the selected durable session, then resolves it
through the existing `AttachmentStore → RunRequest → Message.Attachments`
pipeline. Image-capable providers receive the existing standard OpenAI
multimodal content-parts shape; there is no second model-facing content path.
An attachment never substitutes for the required non-empty newest user text.

Durable web history returns the same attachment metadata references alongside
the display-projected user message, never the original bytes. Authenticated
`GET /api/attachments/{id}/preview` verifies the current conversation mapping
and renders at most a 512×512, 2 MiB PNG. The client separately caps the number
of replay previews it loads.

## Policy and approvals

HTTP serving composes the same `Application`, executor, policy, session store,
and agent loop as terminal and stdio RPC interfaces. It does not implement a
second tool path.

The serving policy is independent of `assistant.policy` and defaults to
`prompted`:

- `trusted` permits ordinary policy-allowed tools without an interactive
  prompt. Intrinsically approval-only operations wait for an explicit decision
  through `/api/approvals`.
- `prompted` marks every mutating action as approval-required. In manual
  approval mode it uses the bounded broker and a disconnected or timed-out
  request fails closed; auto mode resolves only eligible gates.
- `locked` exposes no tools.

Serving approval mode is independently selected by
`serve.approval_mode` and defaults to `manual`. `manual` routes the gates above
through `/api/approvals`; `auto` resolves eligible gates without changing the
serving policy, tool registry, plan gate, filesystem roots, or process sandbox.
Package test/activation remains manual, including the tested-checksum gate.
Invalid values prevent startup rather than falling through to auto. There is no
request field or model-output path that can change this config-only setting.

Auto-approved serving actions append the same bounded, content-free durable
session audit event as other assistant sessions. Built-in file mutations also
retain their reversible mutation journal. Identity context files inside the
configured write roots remain writable in auto mode by design; see the "pen"
decision and full tier model in [permissions.md](permissions.md).

Pending approvals carry opaque approval, run, and Bee-session identifiers so a
same-origin owner UI can associate the prompt with its active response. The
existing decision scope remains process-global and approval-ID-addressed; the
run/session fields are associations, not alternate authorization credentials.
All approval routes require the same bearer token as the other protected
routes.

`GET /api/interaction-policy` returns the same bounded display visibility and
effective plan-mode state used by the TUI and legacy gateway. An optional
`X-Session-Id` selects the already-bound durable conversation; without it, the
response reports the serving process/default state. The route is outside the
OpenAI chat-completion schema and does not change completion SSE framing.

An HTTP-specific tool allowlist is not implemented in Phase 0. Use `locked`
for a model-only endpoint or compose a reduced registry through the library
when a narrower surface is required. A first-class config allowlist is a
follow-up because Bee's foreground registry also includes virtual plan/context
tools and optional web executors that must be filtered together rather than
partially hidden.

## Lifecycle and limits

The server uses the existing default session store and a separate durable
route-binding file; starting it neither deletes nor rewrites terminal-created
sessions. Each durable turn constructs its own session-backed agent while the
configured application/provider services are shared. The admission limit,
bounded request body (1 MiB), bounded message count, per-request timeout, and
agent/tool limits bound per-connection work. SIGINT or SIGTERM triggers a
graceful HTTP shutdown and then closes configured extensions.
