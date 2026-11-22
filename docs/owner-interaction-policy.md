# Owner-facing interaction policy

This note records the interface-neutral seam for approval brokering, display
visibility, approval status, and plan-mode state. The fullscreen TUI, the
legacy web gateway, and the authenticated OpenAI-compatible serving surface are
adapters over this seam; none of them owns a separate policy implementation.

## Inventory before implementation

### Approval brokering

- `tui_terminal.go` owns `tuiApprovalBroker`, a private prompt channel, the
  inline request rendering, and explicit `y`/`n` handling.
- The redirected line TUI in `interfaces.go` installs a separate scanner-backed
  approval hook. The CLI and REPL have two more terminal-specific hooks.
- `tools.go` owns the actual policy decision and emits an executor event only
  after approval succeeds, fails, or is denied. `runtime.go` projects that
  completed decision as `RunApproval` and persists it for durable sessions.
- `gateway/approvals.go` owns another broker. `GET /api/approvals` lists its
  process-global pending requests and `POST /api/approvals/{id}` decides one.
  Pending requests have no run/session association and are absent from run
  SSE. This API is available only on the legacy gateway today.
- Authenticated OpenAI-compatible serving exposes the shared bounded approval
  broker. Manual prompted serving fails closed without a decision, while
  config-selected auto mode resolves only eligible gates. Package test and
  activation remain human-reviewed even under trusted policy or auto mode.

### Display visibility and redaction

- `display_visibility.go` stores visibility on `Application` but describes it
  as terminal-only state.
- `/tools`, `/thinking`, and `/status` in `commands.go` read or mutate that
  state. The result is structured in `LocalCommandEvent`, so the legacy
  gateway can reach it indirectly by posting a slash command.
- `tui_render.go` and `tui_terminal.go` independently suppress reasoning and
  tool events. `tui_details.go` applies the structural redactors in
  `display_projection.go` while formatting tool details.
- Legacy gateway history applies `ProjectMessageForDisplay`, but live run SSE
  retains raw tool and reasoning events without consulting visibility.
- OpenAI-compatible serving emits assistant text only. It exposes neither raw tool
  details nor the visibility state that explains the projection.

### Approval status

- The completed core `RunApproval` is authoritative and durable.
- The fullscreen TUI renders the pending prompt locally and the final status
  from `RunApproval`. It deliberately does not render another status when the
  approval key is pressed.
- The gateway decision POST returns an acknowledgement and the run SSE later
  returns `RunApproval`, but the contract does not state which one a client
  should render. There is no explicit pending/completed status field.
- OpenAI-compatible serving exposes no approval status.

### Plan-mode gating

- Enforcement is already core-owned by `PlanGate`, session `plan_mode`
  records, `Application.PlanMode`/`SetPlanMode`, and per-run agent options.
- TUI coupling is limited to slash-command and status-card presentation.
- The legacy gateway reaches the same Application methods indirectly through
  `/plan` and `/status` message runs and receives structured local-command
  events. It has no direct interaction-policy state endpoint.
- OpenAI-compatible requests disable slash commands. They run under the effective
  gate but cannot inspect or change its state.

## Interface-neutral seam

1. `Application` owns an approval coordinator. It implements the existing
   `ApprovalHook`, retains bounded pending prompts, associates each prompt with
   the core run/session context, and accepts an explicit approved/denied
   decision by opaque approval ID. Adapters list and decide through Application
   methods; they do not implement their own wait channels.
2. The coordinator emits a pending approval through the current run context
   only after the prompt is registered. `ApprovalEvent` carries its opaque ID
   and an explicit lifecycle status. The executor continues to emit the single
   final decision. Only final decisions are durable.
3. A decision HTTP response is an acknowledgement, not a second display
   event. The pending and final `RunApproval` events are authoritative for
   rendering. Repeated final statuses for the same approval ID/status are
   suppressed before gateway delivery.
4. The existing gateway contract remains process-global and ID-addressed.
   Adding run/session metadata is backward-compatible; this slice does not add
   a same-session restriction that would change existing clients. Authenticated
   HTTP serving reuses the same `/api/approvals` paths rather than defining a
   second approval protocol.
5. Raw `RunEvent` and durable session data remain faithful. A core display
   projection applies `Application.DisplayVisibility`: hidden reasoning and
   tool events are omitted, visible tool arguments/results use the existing
   structural redactors, and compact mode does not expose raw result bodies.
   The gateway retains and streams only this projected form.
6. The initial run event exposes a bounded interaction-policy snapshot:
   display visibility plus effective plan-mode state. `/tools`, `/thinking`,
   and `/plan` continue to return their existing structured update events.
   Plan enforcement itself remains unchanged in `Application`/`PlanGate`.
7. The TUI keeps its observable text and explicit-key behavior. It consumes
   pending/final core approval events and submits decisions to Application;
   terminal layout and compact formatting remain adapter concerns.

## Compatibility and safety

- Bearer authentication and SSE framing/keep-alives are unchanged.
- Existing OpenAI chat-completion payloads remain unchanged; policy routes are
  additive same-origin routes outside the OpenAI-compatible contract.
- Approval arguments use the same bounded structural display projection before
  they cross an HTTP display boundary. Secrets, tokens, and private credential
  material must not be logged.
- Durable approval records contain final outcomes only, preserving current
  replay semantics and avoiding a stale pending record after interruption.
