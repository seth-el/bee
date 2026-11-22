# Durable notification inbox

Bee job success and failure notifications use a private, caller-rooted
`notifications.json` snapshot. Terminal job finalization durably records a
deterministic notification intent in the authoritative job store first; the
snapshot then contains an outbox record and its inbox projection in one
checksummed atomic commit. Delivery keys are
`job/<job-id>/attempt/<attempt-id>/state/<state>`, so retrying a notifier is
idempotent and cannot create duplicate inbox rows.

`RunDueWithNotifier` delivers pending durable intents after execution. Delivery
claims use a bounded five-minute lease so concurrent foreground runners cannot
claim the same intent; an expired `sending` attempt is recorded as interrupted
before a later run-due pass retries it. Inbox persistence failures never roll
back terminal job state. At most three delivery attempts are retained in job
history, after which the durable intent remains failed for operator action.
Corrupt, truncated, mismatched, oversized, or unsafe private state fails closed
and is never silently repaired.

The web gateway exposes bounded `GET /api/notifications`, idempotent
`POST /api/notifications/:id/read`, and a bounded global
`GET /api/notifications/events` SSE stream. The embedded UI provides an unread
badge capped at `99+`, a notification panel, mark-read actions, and stable
`before` pagination for older items. Read mutations advance the store sequence
even when the unread badge remains capped. HTTP responses omit delivery keys,
attempt IDs, and private failure details; the private job store retains bounded
failure details for operator inspection.

Local and HTTP readers: `bee inbox` lists unread
notifications (`--all` includes read ones, `--json` for scripts) and shows
the private failure reason for paused and failed jobs; `bee inbox ack <id>` or
`ack --all` marks them read. The full-screen TUI shows an unread badge in the
composer title (refreshed every 30 seconds and after each run) and a `/inbox`
command (`/inbox ack` marks all read), also available through
`POST /api/commands`. `bee --serve` exposes the same bounded
`GET /api/notifications` (`limit`, `before`; unknown query parameters are
rejected) and `POST /api/notifications/:id/read` as the legacy gateway, with
`Cache-Control: no-store`. Over HTTP, failure reasons stay private.
`bee jobs retry` and `bee jobs dismiss` mark the job's notifications read.

Telegram, web push, email, extension notifications, and other adapters remain
out of scope for this slice.
