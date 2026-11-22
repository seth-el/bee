# Bee reference

Detailed reference for configuration, the CLI, and the Go API. Start with the
[README](../README.md) for an overview; topic documents in this directory go
deeper on each subsystem.

## Configuration and layout

Bee's personal root defaults to `~/.bee` and can be overridden with `BEE_HOME`. The default config is `~/.bee/config.json`; durable memory is `~/.bee/workspace/memory`; sessions/runtime state is under `~/.bee/state`; and protected secrets belong under `~/.bee/secrets`. Use `bee init` to create this layout safely without overwriting an existing config, then `bee doctor --strict` for bounded local diagnostics. Use explicit flags such as `--config` and `--memory-root` for alternate profiles. If no config exists, Bee prints an actionable error with the `--print-example` and `--init-config` options. The authenticated OpenAI-compatible serving mode is documented in [docs/openai-serving.md](openai-serving.md). See [docs/INSTALL.md](INSTALL.md), [docs/OPERATOR-RUNBOOK.md](OPERATOR-RUNBOOK.md), and [docs/workspace-layout.md](workspace-layout.md).

Example config:

```json
{
  "schema_version": 1,
  "profiles": [
    {
      "name": "local-main-8081",
      "role": "main",
      "provider": {
        "type": "openai-compatible",
        "base_url": "http://127.0.0.1:8081",
        "model": "local",
        "image_input": true
      }
    },
    {
      "name": "local-child-8082",
      "role": "child",
      "provider": {
        "type": "openai-compatible",
        "base_url": "http://127.0.0.1:8082",
        "model": "local"
      }
    }
  ],
  "routing": {
    "main_profile": "local-main-8081",
    "child_profile": "local-child-8082",
    "voice_profile": "local-main-8081",
    "manual_profiles": [],
    "max_children": 3
  },
  "agent": {
    "system_prompt_file": "workspace/SOUL.md"
  },
  "assistant": {
    "policy": "trusted",
    "approval_mode": "manual",
    "require_plan": false,
    "tool_display": "compact",
    "show_thinking": false
  }
}
```

`bee init` and `bee --print-example` generate a deliberately minimal document:
provider topology and operator policy are explicit, while generation, context,
specialist, broker, and disabled-feature limits inherit the current bounded
runtime defaults. Duplicate object keys, trailing JSON, and a non-object top level are
rejected. Unknown fields are ignored with a warning (a near miss of a known
key such as `requre_plan` still fails with a suggestion); set
`"strict_config": true` or `BEE_STRICT_CONFIG=1` to reject every unknown
field.
Legacy documents without `schema_version` continue to load as version 1. Use
`bee config migrate --config PATH` to preview the exact normalized document and
add `--apply` to replace it atomically with owner-only permissions. `bee config
effective` shows the validated, default-expanded configuration while replacing
secret-file locations with `<configured>`.

Set `provider.context_window_tokens` to the selected model's known capacity to
enable request admission. Zero means explicitly unknown; Bee never guesses a
generic window. Old bulky tool results may be reduced only in the outgoing
projection, while `/compact` appends a provenance-bearing checkpoint and keeps
the source transcript. Automatic compaction is opt-in and runs only for
overflow recovery. See [docs/context-kernel.md](context-kernel.md).

For context controls, an explicit `reserve_tokens: 0` removes the separate
recovery reservation and `tool_result_tokens: 0` disables the per-result size
cap. Zero `output_reserve_tokens` and `summary_tokens` retain their safe
runtime fallbacks rather than treating unbounded output or summaries as free.

`agent.max_tool_steps` defaults to `32` provider tool batches per segment. A positive value is a hard tool-admission boundary; explicit `0` (or `/tool-limit off`) leaves tool steps unlimited. Bee warns before a finite boundary using `tool_landing_steps` (default `8`) and, if the model still requests tools at the boundary, reserves a completion with provider-neutral `tool_choice: none` to give a concise answer and save a durable continuation checkpoint instead of ending with a loop error. The finalizer retains the ordinary tool schemas and reasoning/template settings for KV-prefix reuse, while Bee rejects rather than executes any violating tool call. `turn_timeout_seconds` defaults to `600`; explicit `0` disables it. Turn and finalization deadlines are admission boundaries: Bee does not start new provider or tool work after they expire, but an already-streaming provider response may finish. The work timer does not own finalization: `finalization_timeout_seconds` (default `60`) creates a fresh admission window from the parent run context, while explicit user cancellation and provider-level timeouts still stop immediately. `deadline_warning_seconds`, `finalization_max_tokens`, and `no_progress_limit` tune the landing warning, closing response, and repeated equivalent-observation/failure detector.

`max_auto_continuations` optionally admits a bounded number of additional tool-work segments before checkpointing (default `0`). `auto_compact_between_segments` can append a compaction checkpoint for completed segment chatter between those segments. There is deliberately no non-disableable emergency tool ceiling. The web UI exposes a **Continue** action for a saved checkpoint, while CLI/TUI users run `/continue [checkpoint-id]`; both send the checkpoint ID back to Bee so the structured summary, active-plan fingerprint, evidence/observation fingerprints, unresolved work, blockers, and next action are injected explicitly. `bee --session ID --continue "PROMPT"` likewise selects the latest pending checkpoint. A selected checkpoint is marked resumed, and exact repeated calls to read-only tools return its prior evidence metadata instead of performing the read again. To remove the generation cap only for the main provider role, set `generation.main_max_tokens` to `0`; omit it to retain the normal `max_tokens` cap. Child, memory, voice-child, acceptance-probe, and manual/cloud requests continue using their existing role-specific behavior.

`generation.reasoning_effort` optionally records and sends the main request's reasoning level. Supported values are `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, and `max`; omission leaves the level at the provider default. Bee sends an explicit value both as the OpenAI-compatible `reasoning_effort` field and, for local template compatibility, as `chat_template_kwargs.reasoning_effort`. `/status` reports the effective configured value rather than inferring one from model output.

`agent.system_prompt_file` is optional and defaults to `BEE_HOME/workspace/SOUL.md` (normally `~/.bee/workspace/SOUL.md`); a missing file keeps Bee's built-in capability/truthfulness prompt. Relative paths are BEE_HOME-relative and `~` paths are expanded. When a file is present it must be a readable owner-only regular file no larger than 64 KiB. File contents are appended to the built-in prompt. Omit `agent.system_prompt` to use this composition; an explicitly configured non-empty value remains authoritative and ignores SOUL.md. Parent sessions then append the conventional `BEE_HOME/workspace/BEE.md` as delimited operator-authored workspace context. BEE.md is also owner-only, non-symlink, and bounded at 64 KiB; it is not sent to child-role agents. An explicitly empty `agent.system_prompt` disables the complete system message for legacy compatibility. Keep both files free of secrets.

`base_url` may be an OpenAI-compatible server root, `/v1`, or the full `/v1/chat/completions` endpoint. The generated config contains only the named local `local-main-8081` and `local-child-8082` profiles. Named profiles are authoritative when `profiles` are present. The top-level `provider` spelling remains supported for legacy single-provider documents only; a profile-based file containing a conflicting top-level provider is rejected. Manual or cloud profiles are opt-in: add one with role `manual`, list it under `routing.manual_profiles`, and select it explicitly with `--profile NAME`. Bee never escalates automatically. API keys are optional for local endpoints. If needed, set a profile's `provider.api_key_file` to a private file containing the key; Bee validates owner-only permissions, reads it only for the request, and never prints it. See [docs/provider-acceptance.md](provider-acceptance.md) for endpoint/provider-spy acceptance.

## CLI chat and the TUI

Ordinary CLI chat is durable by default: Bee creates a private session, prints its stable `session: ID` line, and persists the conversation across process exits. Use `--model MODEL` for an explicit per-run model override; the selected model is validated before provider execution and exposed in run metadata. The human REPL keeps that session across turns; type `/new` to start a fresh durable session, `/resume [session-id]` to switch to an existing one, `/steer <correction>` while a response is running to interrupt and redirect it in the same session, and `/exit` or Ctrl-D to leave. Use `--session ID` to continue a session explicitly, or `--session last` for the most recently active one (`--resume ID` is a deprecated alias); add `--continue "prompt"` to resume that session's interrupted turn from its checkpoint, `--list-sessions` to enumerate sessions, and `--inspect-session ID` to inspect metadata. Untitled sessions get a short title from the child model after their first reply; `bee sessions title-backfill` lists older untitled sessions, and `--apply` titles them (one child call each, without changing their order in the list). Applications can use `NewSessionStore` with an injected root for tests, or `NewDefaultSessionStore` for the user data directory. Session history is append-only JSONL; messages are a derived view. Durable session files faithfully retain local conversation/tool payloads and may contain secrets, so the session directory and files remain owner-only/private. Provider tool-call batches are staged and validated before persistence or execution. Truncated or malformed batches execute nothing, emit bounded diagnostic metadata without raw arguments, and receive at most one transient recovery retry; a second failure ends the run without poisoning the session. `finish_reason: length` is incomplete rather than successful. Bee also retries a length-limited response once when no tool call was emitted, with guidance to call a tool promptly or return a concise final answer; the discarded partial response is not replayed. Replay omits malformed or structurally incomplete historical tool groups without rewriting the source events. If older history needs an explicit on-disk rewrite, use `bee sessions repair --session ID --dry-run` and `bee sessions repair --session ID --apply` only after reviewing the report.

Each durable session permits one complete turn at a time across processes. A
second overlapping turn fails with `session already has an active turn`, and a
session cannot be deleted until its active turn finishes. Other sessions remain
independent. General-assistant compositions also append content-free run
lifecycle diagnostics to `BEE_HOME/state/diagnostics/runtime.jsonl`; prompts,
tool arguments, provider output, and filesystem paths are not recorded there.

`--tui` uses a compact fullscreen interface with a scrollable transcript,
streamed assistant output, explicit bounded reasoning sections, redacted and
bounded tool arguments/results, colorized Git/edit diffs, and a persistent `›`
composer pinned to the bottom of the terminal. Page Up/Page Down scrolls the
transcript while normal terminal mouse selection remains available for copying
output; the draft remains editable while a response streams. Submit
`/steer <correction>` to interrupt that response, or press Enter to send an
ordinary draft once the response is complete. The composer grows with wrapped or explicit lines up to five rows,
then keeps the cursor in view while scrolling the draft. Up/Down moves across
explicit and wrapped draft rows, falling back to command history at the draft
boundary. Modified Left/Right and Backspace/Delete use common terminal word
editing sequences; Ctrl-W remains a portable delete-previous-word fallback,
and Ctrl-J inserts an explicit line break. Assistant Markdown, including fenced code blocks, headings,
lists, quotes, links, tables, and inline emphasis/code, is formatted for the terminal.
Tables use aligned, wrapped terminal grids when space permits and labeled rows on narrow screens.
Redirected input or output automatically uses the compatible line-oriented
transcript. Provider text is normalized before display and cannot inject
terminal control sequences. `/status` renders a bounded card containing the
selected model, configured reasoning level, current directory, known context
window, plan mode, session/profile, capability scope, and recent context/usage
telemetry.
The local TUI is an owner-facing diagnostic surface and can display tool output
and private provider reasoning; do not copy its transcript into an untrusted
channel without reviewing it.

The fullscreen TUI opens with a text masthead: the BEE wordmark, the model
and profile, the session, the workspace, and the current run state (ready,
working, thinking, needs approval, error, done). Request-context usage appears
in the composer border. Narrow or short terminals drop the masthead.

## Memory commands, tools, and policy

`bee memory list|show|search|doctor|proposals|export|import` is the explicit, parent-owned durable-memory surface. Durable records do not expire from age by default; read-only `list`, `show`, and `search` accept `--retention-days N` to opt into an age window (`0` remains unlimited) and `--include-sensitive`. `doctor` reports event, record, proposal, source-corpus, and semantic-index health without importing source files. `proposals list|show|approve|reject` exposes the review lifecycle with explicit audit IDs and reasons. Import is dry-run by default, refuses conflicts, and never silently overwrites. `bee memory inventory` is a separate read-only source scan: it writes only a bounded metadata manifest, never creates memory records, and excludes Dreaming/session/generated state by default. `bee memory source-archive` is a separate bounded, read-only snapshot into Bee's private source-memory area; Dreaming paths are excluded by default, repeatable `--exclude` flags add path/glob exclusions, and dry-run manifests report excluded paths/counts without opening or hashing excluded files. It never writes OpenClaw roots or Bee active memory. `bee memory candidates` extracts provenance-bearing review candidates from an inventory, but still creates no durable records. `bee memory review` records explicit human decisions and deterministic normalization, still without durable writes. `bee memory import-normalized` is dry-run by default, and apply only with explicit audit ID/reason and `--apply`; it reuses Bee's atomic parent-owned import path. Search supports current/as-of/since/before temporal retrieval over active, historical, superseded, and expired records. Optional local semantic retrieval adds with an incremental ID/vector index, automatic refresh after mutations once an assistant is composed, live reload, filtered vector search, and lexical fallback; initial index creation remains `bee memory semantic rebuild`. The default remains lexical-only, and memory is never sent to cloud providers. See [docs/memory-inventory.md](memory-inventory.md), [docs/memory-source-archive.md](memory-source-archive.md), [docs/memory-candidates.md](memory-candidates.md), [docs/memory-review.md](memory-review.md), [docs/memory-import-normalized.md](memory-import-normalized.md), [docs/memory-temporal-retrieval.md](memory-temporal-retrieval.md), and [docs/memory-semantic.md](memory-semantic.md).

The ordinary `bee` assistant includes the configured root-bounded filesystem/argv, durable-memory lookup and lifecycle tools, a private TTL scratchpad scoped to the current durable session, and read-only Linux inspection tools when supported. Scratchpad expiry is lazy-on-access and values are separate from durable memory; process inspection is current-user-only, service inspection is `systemd --user`-only, command output is capped, and no service mutation is exposed. See [docs/scratchpad-system-inspector.md](scratchpad-system-inspector.md). Built-in `write_file` mutations are journaled below the injected session-store root by the default composition, with per-readable-root scope isolation and approval-required hash-guarded undo; arbitrary commands, terminals, extensions, child processes, and external side effects are not mutation-journaled or automatically reversible. `write_file` unifies create, append, exact edit, and whole-file replacement. Omitting `operation` is create-only and refuses an existing path; append, edit, and replace must be explicit, and may include `expected_sha256` to reject stale mutations. `assistant.approval_mode` defaults to `manual`; `auto` resolves eligible prompts without changing policy, plans, write roots, or Bubblewrap, and appends a bounded metadata-only outcome to the durable session audit. Package test/activation remains human-reviewed. When plan mode is on, read-only inspection works before a plan while mutating or consequential tools require a valid `bee_plan`; when it is off, policy and approval controls still apply. Trusted policy avoids repeated write prompts inside configured write roots while retaining bounded execution, audit, explicit provider selection, direct exact-target SSH for the foreground assistant, optional parent-owned SSH narrowing grants, and no automatic cloud escalation or Git merge. With an initialized Bee home, relative filesystem paths and command working directories resolve from `BEE_HOME/workspace`; the user's home remains a secondary readable root, while outside-workspace mutations require approval. Use `assistant.write_root` or `assistant.write_roots` to configure automatically writable locations, `--scope-root PATH` or `assistant.scope_roots` to override readable roots, or `--full-machine` for full-machine reads. `--policy prompted` gates every mutation, `--policy locked` exposes no tools, and approval mode determines whether eligible gates wait for a person. There is no separate `bee code` command. See [docs/permissions.md](permissions.md) for the complete tier model.

The experimental native `terminal` PTY tool is disabled by default. Enable `terminal.enabled` only for private/development testing; it provides bounded in-memory sessions and explicit open/run/submit/write/read/resize/signal/interrupt/close/list/inspect actions, but no HTTP/WebSocket PTY endpoint. Terminal shells share the normal Bubblewrap write-root sandbox and may read or `cd` elsewhere without gaining persistent write access. Sessions disappear after process restart. See [docs/terminal.md](terminal.md).

Optional bounded post-turn memory extraction is configured under `memory.automation` and is disabled by default. Equivalent records and proposals are idempotent across turns, and `/status` reports only content-free saved/unchanged/proposed/rejected counts. The owner can enable it explicitly with a local child profile; keep `auto_save_safe` false until proposal behavior is reviewed. See [docs/memory.md](memory.md) for the strict JSON, bounds, rejection, and parent-owned write/proposal lifecycle.

## Go API: composing the assistant

Compose the ordinary assistant once and use it for chat, inspection, memory lookup, web research, and coding tasks:

```go
app, err := bee.ComposeGeneralAssistant(config, profile, bee.AssistantCompositionOptions{
    Store: store,
    ScopeRoot: scopeRoot, // defaults to Bee workspace first, then user home
    Policy: bee.AssistantPolicyTrusted,
})
if err != nil { /* handle error */ }
defer app.Close(context.Background())
```

The root-bounded local tools are `read_file`, `list_files`, `find_files`, `grep`, and the operation-driven `write_file`, with canonical `repo_clone`, bounded `memory_search`/`memory_show`, and optional bounded raw `web_search`/`web_fetch` plus opt-in `web_browser`/`web_browser_act` (static fetch first; browser targets are session-bound and never exposed, and interactive browser actions require a valid plan). Foreground composition additionally exposes `exec_readonly` for synchronous local shell inspection inside a read-only, networkless, host-process-isolated Bubblewrap view, and `exec` for mutation-capable commands in the ordinary write-root sandbox. `exec`, `terminal`, and approval-gated `host_exec` require an active plan when plan mode is enabled. Normal SSH/OpenSSH behavior, inherited environment plus validated overlays, bounded output, process-group cleanup, and optional PTY/background follow-up remain on `exec`; `host_exec` is the synchronous approval-gated escape hatch for explicitly intended unrestricted host mutations. For long web questions, optional `web.scout.enabled` replaces raw foreground web tools with bounded `web_research` plus `web_fetch`, backed by the local child profile (normally 8082). See [docs/web-research-scout.md](web-research-scout.md). The legacy `run_command` argv/target contract remains only for internal/child compatibility. Passwords entered through terminal write actions may be recorded in tool-call history; key authentication is preferred. Read-only tools work before a plan; mutating or unknown tools require a valid `bee_plan`. Because the web gateway currently has no application authentication, keep it strictly on a trusted network even with write-root confinement.

`ChildRunnerPool` and `ProcessChildRunnerPool` provide bounded independent runner slots for real read-only parallel children; mutable work remains serialized or isolated in Git worktrees.

`TaskPlan`, `ScopedTask`, and `ChecklistItem` are serializable validation seams for a lead model. Plan mode is off by default and can be toggled with `/plan on`, `/plan off`, or the `--plan`/`--no-plan` CLI overrides; `/plan` or `/plan status` reports the current durable plan and criterion counts, while `/plan reset [reason]` and `/plan complete [note]` close the current planning epoch. `assistant.require_plan` supplies the default for sessions that have not recorded a mode yet. The effective mode is persisted per durable session. Enabling it clears the previous active plan and requires a fresh `bee_plan`; disabling it preserves planning history while stopping plan enforcement. When enabled, Bee exposes `bee_plan`, `plan_status`, and `plan_update`, allows read-only inspection without creating a plan, and blocks mutating or unknown tools until a valid, unambiguous plan is recorded. Equivalent plan submissions reuse the active plan; a materially different replacement requires an explicit reset. Bee injects the current updated plan as required runtime context even after conversation compaction. Gateway deployments expose pending decisions through `/api/approvals`; prompted policy uses it for ordinary mutations, while trusted policy uses it for outside-write and intrinsically approval-required capabilities. Multiple readable roots can be configured with `assistant.scope_roots`; automatically writable roots use `assistant.write_root` or `assistant.write_roots`. See [docs/permissions.md](permissions.md).

Terminal tool activity defaults to `compact`: Bee pairs each call with its result, uses a short semantic summary, groups adjacent exploration, and only previews bounded output when it is useful (for example, command output or failures). Set `assistant.tool_display` to `compact`, `full`, or `off`; `full` retains the diagnostic argument/result cards. The legacy `assistant.show_tool_calls: false` setting still hides tools. Set `assistant.show_thinking` to `false` to hide reasoning. `/tools compact|full|off` and `/thinking on|off` override those defaults for the current process without changing the config file (`/tools on` remains an alias for `full`).

Host-local speech helpers are disabled by default and never assume a particular user's home directory. Configure `speech.transcription_script` for uploaded-audio transcription and `speech.synthesis_wrapper` for `/speak` and `/say`. Optional `speech.synthesis_working_directory` and `speech.synthesis_output_directory` values default to `BEE_HOME/workspace` and its `generated/voice-replies` directory. Relative speech paths are resolved beneath `BEE_HOME`, which keeps a copied config portable; executable code still runs as the Bee OS user and should be explicitly trusted.

## Sessions

```go
store, _ := bee.NewSessionStore("/tmp/bee-test")
session, _ := store.Create(bee.SessionMetadata{ProjectRoot: projectRoot})
_ = session.AppendMessage(bee.Message{Role: bee.RoleUser, Content: "hello"})
context, _ := bee.AssembleContext(session.Events, bee.ContextOptions{Budget: 4096, RecentMessages: 8})
_ = context.Messages
```

`SessionStore` supports create, append/replay, list summaries, fork at an event boundary, and deterministic compaction events. `DiscoverProjectInstructions` reads only bounded `AGENTS.md` files, ordered from filesystem ancestors to project-root descendants; later files have higher precedence. Session context does not load durable user memory or arbitrary files.

## Run events and interfaces

`Application.Run` returns a typed `RunEvent` stream covering run lifecycle, session identity, assistant deltas/messages, provider-output rejection/recovery, plans, tool calls/results, approvals, usage, and failures. `RunPrint`, `RunJSON`, `RunTUI`, and `RPCServer` are thin adapters over that same stream. NDJSON writes one JSON event per line to stdout; local RPC uses JSON-RPC 2.0 request/response lines plus streamed `event` notifications. The TUI uses a standard-library terminal renderer with a line-oriented fallback for redirected I/O.

## Extensions

Extensions are opt-in subprocesses configured under `extensions`. Both shipped
product roots (`cmd/bee` and `cmd/bee-telegram`) use
`ComposeGeneralAssistant`; actual assistant runs start enabled extensions,
register their tools in the ordinary `Tool`/`Registry`/`Executor` path, and
close them on shutdown. Inspection-only commands do not start them. `ExtensionHost`
filters environments, enforces bounded frames/messages and startup/request
timeouts, validates initialize manifests/capabilities, and isolates
crashes/protocol failures, so extension tools cannot bypass root scope, policy,
approval, audit, output limits, sessions, or run events. Extension
notifications are untrusted observations; provider, hook, and context
capability names are discovery seams only and are not executed.

```go
hosts, err := bee.NewExtensionHosts(config.Extensions)
for _, host := range hosts {
    if err := host.Start(ctx); err != nil { /* handle isolated extension failure */ }
    _ = host.RegisterTools(executor.Registry)
}
```

See [docs/extensions.md](extensions.md) for the wire contract and [cmd/bee-extension-example](../cmd/bee-extension-example) for a tiny standard-library process.

## MCP client

Bee can also launch explicitly configured local MCP stdio servers and project
their advertised tools into the same `Tool`/`Registry`/`Executor` path. The
initial compatibility scope is the handshake-based MCP `2025-11-25` revision;
HTTP transports, resources, prompts, and server-side MCP are not implemented.
Per-server allow/deny lists bound exposure, and only operator-configured
`read_only_tools` avoid mutating-tool approval behavior. Server annotations do
not grant trust. Startup, pagination, schemas, calls, output, and shutdown are
bounded; one failed server is omitted without failing Bee or another server.

See [docs/mcp-client.md](mcp-client.md) for configuration, exact limits,
protocol scope, process trust, and failure behavior.

## Versioned skill packages

Bee supports private instruction skills under `BEE_HOME/skills`. A package
contains a strict `skill.json` manifest and bounded `SKILL.md`; optional
package-contained executables reuse `bee.extension.v1` with an exact declared
tool allowlist. `bee skill test|install|list|inspect|enable|disable|rollback|remove`
manages immutable semantic-versioned revisions and atomic activation state.
Installed skills are inert until explicitly enabled.

Ordinary unlocked parent assistants expose read-only `skill_list` and
`skill_load` tools instead of injecting every skill into every prompt. Skill
instructions are untrusted workflow guidance and cannot grant tools, widen
scope, or bypass plans, policy, approval, audit, timeout, or output controls.
See [docs/skills.md](skills.md) for the package format, CLI, executable
bridge, and current boundaries.

Bee also supports first-class external-process plugins under
`BEE_HOME/plugins`. `plugin.json` revisions use the same private install,
enable/disable, rollback, checksum, and exact extension-allowlist controls.
Unlocked parent assistants can scaffold and live-test inert skill/plugin source
packages. Live test and enable calls require human approval even in automatic
approval mode, and enable
revalidates the reviewed checksum before activation. See
[docs/plugins.md](plugins.md).

## Child tasks

`ChildTask` is the parent-owned authorization contract for one bounded child. It carries parent/task IDs, objective and prompt, an explicit `ExecutionScope`, allowed tool names, timeout/step/output budgets, an artifact directory, and acceptance criteria. `ChildRunner.Run` creates one separate `Agent` with a copied, allowlisted executor; the child cannot add tools, widen paths, or create approvals. Policy and audit hooks remain inherited from the parent executor, and missing approval still fails closed.

The runner returns a `ChildCompletionPacket` with status, summary, changed files, commands/tests/evidence, uncertainty, artifacts, and questions/blockers. It writes `completion.json` and a fingerprinted `completion.manifest.json` under the task's root-bounded artifact directory. Mutable isolated runs also write an `integration.manifest.json` bound to the Git base commit and review diff. Cancellation is context-based, output is capped, and filesystem snapshots are bounded. `bee harness inspect` exposes the content-free effective composition snapshot, stable fingerprint, and composition linter used by this provenance boundary. See [docs/effective-harness.md](effective-harness.md). The in-process runner uses an isolated in-process Bee execution boundary. `ProcessChildRunner` launches one explicit command in a bounded working directory and Unix process group, applies startup/request/frame/output limits, brokers provider calls and approvals through the parent, and isolates crash/malformed/duplicate/unknown responses. Process mode supports built-in coding tools; custom/extension tool brokering and custom policy serialization remain deferred. See [docs/child-process.md](child-process.md).

The ACP coding-backend work adds an explicit, disabled-by-default `coding` config with local-child implementer/reviewer/scout defaults, optional provider-neutral ACP overrides, user backend overrides, and a side-effect-free registry. Bee never discovers adapter binaries or reads OAuth/OpenClaw state; the existing child path remains a composition-root responsibility. See [docs/coding-backends.md](coding-backends.md).

## Orchestration

`ChildObservableExecutor` is an optional compatibility-preserving extension of `ChildExecutor`. In-process and process-backed runners can forward bounded lifecycle, provider, tool, approval, and completion observations to a parent `TraceSink`. `JSONLTraceSink` persists append-only trace records without adding unbounded logs to session context.

`OrchestrationPlan` validates parent-owned task identity, dependency edges, cycles, scopes, budgets, and root-contained non-overlapping artifact directories. `SequentialOrchestrator` runs one child at a time.

`ChildTask.Mutability` makes read-only versus mutable intent explicit. `ParallelOrchestrator` infers read-only only for an empty or known built-in read-only tool set; mutation-capable or unknown tools must not be mislabeled read-only. `MaxConcurrent` bounds independent ready tasks, which are selected lexically, run through a semaphore-like slot bound, and report aggregate timeout/step/output budgets, dependency/failure/block propagation, cancellation fan-out, and child observations to the same trace sink. Mutable tasks are supported only when the plan serializes execution (`MaxConcurrent <= 1`); parallel writers and conflicting scope/artifact relationships fail closed. See [docs/orchestration.md](orchestration.md).

`WorktreeOrchestrator` preserves the existing read-only path and routes explicit mutable tasks through a `WorkspaceManager`. `GitWorkspaceManager` validates a clean repository, resolved base commit, repository-contained task scope, safe task/attempt IDs, and a dedicated Bee-managed root before creating a detached worktree with bounded argv-only Git commands. Failed, cancelled, and successful mutable children preserve their worktrees by default. `IntegrationProposal` records changed/untracked files, a bounded diff artifact, conflicts/errors, tests/evidence/artifacts, and review readiness. Bee never applies or merges a proposal; callers must explicitly call cleanup after inspection. Ambiguous or stale lease metadata fails closed.

## Identity and memory contracts

`IdentityProfile`, `UserProfile`, `MemoryRecord`, `MemoryProposal`, `MemoryEvent`, `MemoryQuery`, `MemoryRetrievalResult`, and `MemoryContextPacket` remain separate provider-neutral contracts. Identity and user profile data are not durable-memory records; durable memory is not session history; and `MemoryContextPacket` is a bounded transient projection rather than a catch-all context type.

`NewLocalMemoryStore` uses a caller-provided private root and replays an append-only JSONL event log with deterministic versions, tombstones, fail-closed corruption checks, conservative cross-process writer locks, and metadata-validated in-process replay caching. The ordinary assistant exposes strict omission-preserving `memory_edit`, tombstone `memory_forget`, and filesystem-transaction-backed `memory_supersede` operations with bounded summaries. See [docs/memory-mutation-tools.md](memory-mutation-tools.md). `NewLocalIdentityStore` and `NewLocalUserProfileStore` use separate atomic 0600 snapshots. `NewLocalMemoryProposalSink` writes only validated child proposals to a separate log; parent acceptance still requires a new parent-owned `MemoryEvent`. `RetrieveRanked` scores all authorized lexical candidates before applying the candidate cap and supports filtered semantic lookup. The core does not require embeddings, QMD, or remote calls; the optional local semantic configuration adds its own coalesced background refresher. See [docs/memory.md](memory.md), [docs/memory-retrieval.md](memory-retrieval.md), and [docs/local-memory.md](local-memory.md).

## Runtime context

Bee integrates identity, user-profile, and bounded memory context without mixing it into durable session history. Ordinary `Agent.Run` turns do not retrieve memory automatically; the model-facing `memory_search` tool is the explicit recall path and preserves configured lexical/semantic retrieval. Library callers may opt into prompt-based recall with `AgentOptions.AutoRecallMemory`; opted-in memory is delimited reference data and drops first under context pressure. Memory lookup fails closed by default when that opt-in is used, with an explicit optional policy available to callers. Child tasks can receive only parent-created, bounded `ChildMemoryProjection` data; they never receive a memory service or durable write handle. See `docs/runtime-context.md`.

The general assistant also exposes read-only `runtime_info` for truthful,
non-secret host/runtime introspection. The current time arrives with every
turn as a clock record, so `current_time` is no longer model-facing.
Active top-level runs can be cancelled
by parent-owned `/stop` or `/kill` commands (`/cancel` remains an alias), and
zero/unset provider and ACP request timeouts remain unlimited while explicit
budgets and safety/resource bounds continue to apply.

## Channels

Bee has provider-neutral `InboundMessage`/`OutboundMessage` envelopes, safe channel/account/conversation/thread identifiers, bounded metadata, attachment references, voice references, and reply metadata. `ChannelRouter` validates and authorizes normalized input, atomically deduplicates inbound identities, derives a deterministic `SessionRouteKey`, and routes each external conversation to its own existing Bee session through `Application.Run`. A `Channel` exposes only an outbound send seam; credentials, polling/webhooks, and provider handles remain adapter-owned and are never passed to `Agent` or child processes.

See [docs/channels.md](channels.md) for the contract and explicit deferrals.
