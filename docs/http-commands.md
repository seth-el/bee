# HTTP slash commands

HTTP clients use `POST /api/commands` for owner commands. The endpoint is separate
from `/v1/chat/completions`: command names and arguments are validated by the
HTTP adapter, dispatched through `DispatchCommand`, and returned as the same
structured `LocalCommandEvent` used by the runtime. Command text is never
appended to a session or sent to a provider.

The request requires normal bearer-token authentication and the
client-owned `X-Session-Id`. Bee resolves that external ID through the existing
route binding and acquires the durable session turn lease. A command therefore
returns `409` while the selected session has an active turn and cannot race a
chat completion.

## Inventory

| Commands | Classification | HTTP endpoint |
|---|---|---|
| `/status`, `/help`, `/subagents`, `/tasks` | Read-only/informational | Enabled |
| `/plan` or `/plan status` | Read-only query of durable plan state | Enabled |
| `/tools`, `/thinking`, `/tool-limit` without arguments | Read-only queries of process-local owner settings | Enabled |
| `/plan on\|off\|reset\|complete` | Mutates durable plan mode/lifecycle | Enabled. The command itself is not approval-gated. Enabling plan mode makes `PlanGate` require a fresh plan before later tool mutations. |
| `/tools …`, `/thinking …`, `/tool-limit …` | Mutates process-local owner display/loop settings | Enabled. These settings do not execute tools or alter durable transcript content. |
| `/compact` | Appends a durable context checkpoint, potentially using the configured model | Enabled under the session turn lease; no `PlanGate` or approval gate exists in the TUI path. |
| `/new`, `/resume` | Creates/selects durable sessions | Excluded. Clients use `/api/sessions` so conversation identity remains client-owned and route-bound. |
| `/continue [checkpoint-id]` | Selects the latest pending or named durable checkpoint | Enabled as a two-step handoff: the command endpoint returns the shared core action, then the client starts one normal streamed chat turn with its validated `checkpoint_id`. |
| `/steer` | Returns an action for an interactive run-loop adapter | Excluded; a direct local event would falsely imply that the run action occurred. |
| `/stop`, `/kill`, `/cancel` | Cancels every active top-level run in the Bee process | Excluded because the current core action is not scoped to the selected web session. |
| `/video`, `/image` | Starts adapter-managed media jobs | Excluded; dispatch alone does not perform the media run. |
| `/speak`, `/say` | Starts provider/host speech behavior in a supporting adapter | Excluded; dispatch alone does not perform the voice run. |

Unknown and explicitly excluded commands return a bounded structured command
error with HTTP `400`, never an internal-server error. HTTP parsing rejects
unknown JSON fields, trailing JSON, query parameters, invalid names, and
oversized bodies or arguments.

`/continue` does not run provider work inside the command request. Under the
selected session's turn lease it resolves the checkpoint through the same
`DispatchCommand` path as the TUI and returns `continue.{checkpoint_id,prompt}`.
The client then posts that canonical prompt to `/v1/chat/completions` with the
optional bounded `checkpoint_id` extension. The ordinary OpenAI serving path
maps it directly to `RunRequest.CheckpointID`, so SSE framing, lifecycle
recovery, approval polling, and busy-session behavior stay identical to a
normal turn. Command text never enters provider-facing history.
