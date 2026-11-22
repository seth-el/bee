# Bee architecture sketch

This document records boundaries and the current Phase 8A implementation. The original Phase 0 sketch remains the source of truth for keeping later choices from blurring those boundaries. Phase 4 added a shared application facade and thin local interfaces; Phase 5 added explicitly configured external subprocess extensions; Phase 6A/6B add one bounded child execution boundary and its isolated process transport; Phase 6C adds observations, bounded traces, and deterministic sequential orchestration; Phase 6D adds bounded read-only parallel orchestration; Phase 6E adds leased Git workspaces and review-only integration proposals without automatic merging, retries, scheduling, or approval delegation; Phase 7A adds identity, user-profile, durable-memory, proposal, policy, retrieval-result, and bounded transient-memory-context contracts. Phase 7B adds a private local persistence adapter. Phase 7C adds deterministic bounded lexical retrieval/ranking and optional semantic fallback seams without embeddings or prompt assembly. Phase 7D adds an explicit parent-owned lifecycle service, proposal review, redacted displays, and deterministic export/import. Phase 7E integrates bounded runtime context. Phase 8A adds provider-neutral channel envelopes and route-isolated session routing without a live transport.

## Supported platform scope

Bee is supported on Linux. Windows is explicitly out of scope for the project and for replacement-readiness claims. Unix process groups, `flock`-based cross-process state locks, and Linux foreground coding-mode acceptance are part of the supported boundary; Windows equivalents are not silently assumed. See [platform-support.md](platform-support.md).

## Shape of the system

Bee is split into a platform-independent **core** and replaceable **platform layers**.

### Core layer

The core owns the agent request lifecycle and contracts:

- provider requests and responses;
- tool definitions, registration, validation, invocation, and results;
- sessions, messages, context assembly, and cancellation;
- policy checks, approval requests, and audit events;
- extension and subagent orchestration once those phases land.

The core should not import Telegram, a terminal UI, a vendor SDK, operating-system service code, or OpenClaw packages.

### Platform layers

Adapters translate external concerns into core contracts:

- provider adapters for model APIs;
- tool implementations for the local machine or remote services;
- persistence adapters for session and memory stores;
- TUI, print, JSON, and RPC interfaces;
- channel and voice adapters, scheduling, and process-management adapters;
- external extension hosts speaking JSON-RPC.

Adapters may depend on the core. The core must not depend on a particular adapter.

## Why Go

Go is the intended implementation language because it produces a single deployable binary, has a small standard library, straightforward concurrency and cancellation primitives, and makes interface boundaries easy to keep visible. Phase 0 uses no dependencies beyond the standard library. Dependencies must earn their place by solving a concrete operational or protocol problem.

## Provider interface

The provider contract should be narrow and provider-neutral. A future shape is conceptually similar to:

```go
type Provider interface {
    Complete(ctx context.Context, request Request) (Response, error)
    Stream(ctx context.Context, request Request, onDelta func(string) error) (Response, error)
}
```

`Request` contains normalized messages, explicit generation options, and provider-neutral `ToolDefinition` values; `Response` contains normalized output, usage, finish reason, and provider metadata where available. Phase 1/2 implements an OpenAI-compatible HTTP adapter using only the Go standard library. It accepts a server root, `/v1`, or full completions URL and parses SSE `data:` events into distinct visible-content, private-reasoning, and tool-call fragments. `reasoning_content` never enters `Message.Content` or future request context. The optional `RichStreamingProvider` contract preserves source compatibility for existing providers. Non-success HTTP responses and malformed/short SSE payloads are returned with any accumulated partial response.

The agent treats an assembled provider step as staged, untrusted data. Before recording an assistant tool-call message, it checks completion state, call IDs/names/types, bounded argument size, duplicate IDs, and complete JSON-object arguments for the entire batch. `length`/`max_tokens` finishes are incomplete. A malformed or truncated batch executes no tools; its call payload is excluded from durable tool events and future provider replay, while any visible partial text is marked incomplete and likewise excluded from replay. Bounded reasoning/provider metadata records the rejection without raw arguments. Bee makes at most one transient recovery retry per turn. Recoverable tool output receives smaller-call guidance and counts against the normal tool-step budget. A length-limited response with no tool output receives guidance to emit a needed tool call promptly or return a concise complete answer. A repeated failure returns a typed error while leaving the session resumable. Valid-JSON tool errors remain ordinary matched tool results.

The normalized `Message`, `ToolCall`, and `ToolDefinition` types deliberately do not mirror OpenAI's nested function-call wire shape. The adapter translates to and from that shape. The `Agent` preserves streaming and runs a bounded tool-call loop when the unified assistant composition supplies its executor.

## Configuration

Bee has one primary JSON config file. By default it lives at `~/.bee/config.json` (or `BEE_HOME/config.json`); `--config` is an explicit override. Missing config is an error rather than an implicit provider selection. `--print-example` displays a minimal current example and `--init-config path` creates one without overwriting an existing file. The decoder rejects unknown fields, duplicate object keys, trailing JSON, and non-object documents.

The current generated shape centers named provider roles and leaves runtime
bounds omitted so later bounded defaults can take effect:

```json
{
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
    }
  ],
  "routing": {
    "main_profile": "local-main-8081",
    "manual_profiles": []
  },
  "agent": {"system_prompt_file": "workspace/SOUL.md"},
  "assistant": {"policy": "trusted", "require_plan": false}
}
```

Profile-based files use `routing.main_profile` as the authoritative main
provider. The top-level `provider` object remains a compatibility spelling for
legacy single-provider files; conflicting legacy and named definitions are
rejected rather than resolved by hidden precedence.

`agent.max_tool_steps` defaults to 32 provider tool batches per segment; explicit `0` preserves unlimited behavior. Finite turns receive a configurable landing warning, then stop admitting new tools at the boundary. Turn and finalization deadlines are admission boundaries rather than in-flight generation deadlines: an admitted provider response may finish, while explicit parent cancellation and provider-level timeouts remain immediate. Tool-budget, internal-deadline, repeated plan-evidence, and no-progress boundaries all enter a separately timed finalizer with provider-neutral `tool_choice: none` and append a typed `turn_checkpoint` event rather than surfacing the old loop-limit/stall errors. The finalizer preserves the ordinary tool schemas and reasoning/template settings so the provider can reuse the same KV prefix; any provider tool-call violation is discarded and never reaches the executor. Exact repeated batches and substantially equivalent observations or failures are both covered by `no_progress_limit`. Explicit parent cancellation bypasses finalization. Bounded automatic segments are controlled by `max_auto_continuations`, with optional durable compaction between segments; there is no hidden emergency tool ceiling. `generation.main_max_tokens` is optional and role-scoped: an explicit `0` omits `max_tokens` only for main-provider requests, while omission preserves the normal generation cap and bounded child/memory jobs keep `generation.max_tokens`. Optional `generation.reasoning_effort` is normalized and forwarded in both the OpenAI-compatible request field and local-template kwargs; omission remains an explicit provider-default state in operator status.

Configuration plus the explicitly documented SOUL.md and BEE.md context files
control prompt behavior. The API key is read from the explicitly configured
file when present, which avoids putting secret contents in the config or logs.
Without an injected `Session`, Phase 1/2 behavior remains process-local: the
`Agent` owns an ordered message slice and no session is written to disk. With
`AgentOptions.Session`, user/assistant/tool events and provider usage are
appended to the supplied session while the same provider/tool loop remains in
place.

For parent roles, prompt construction also reads the conventional private
`BEE_HOME/workspace/BEE.md` and appends it as a separately delimited workspace
context block. This fixed convention is intentionally not another config field.
Child roles do not receive it, and it is not persisted into session events.

## Tool registry

Tools are capabilities, not arbitrary functions. The registry owns stable names, schemas, descriptions, risk classification, and lookup. A separate executor validates JSON-object arguments, applies policy, requests approval when required, invokes the implementation with a bounded context, bounds output, and records an audit event. Refused plan, policy, and approval decisions are audited before returning and, for durable sessions, persisted in the session JSONL without raw sensitive arguments. Tool implementations must not silently bypass those checks.

The unified general-assistant registry contains `read_file`, `list_files`, `find_files`, `grep`, the operation-driven `write_file`, canonical `repo_clone`, bounded `memory_search`/`memory_show`, and the mutating, parent-bound create-only `memory_remember`; configured web research adds read-only `web_search`/`web_fetch`. The foreground composition additionally exposes read-only `exec_readonly`, mutation-capable `exec`, approval-gated mutation-capable `host_exec`, and mutation-capable `terminal` follow-up controls, while deliberately hiding legacy `run_command`. `exec_readonly` runs a synchronous local shell in a read-only, networkless, host-process-isolated Bubblewrap view. `exec` runs Bash command strings as the Bee OS user with inherited environment plus validated overlays, normal shell/SSH behavior, bounded output, process-group cleanup, and optional PTY/background integration through `TerminalManager`. Bubblewrap makes the readable host filesystem read-only and rebinds only `ExecutionScope.WriteRoot`/`WriteRoots` writable. `host_exec` is synchronous-only and requires a fresh approval gate for each unrestricted host mutation; config selects manual or eligible automatic resolution. Synchronous foreground commands and compatibility `run_command` share private session-bound command-output retention when their durable session/artifact context exists: bounded results carry opaque continuation metadata for read-only `artifact_read`, while PTY/background transcripts remain non-durable. `run_command` remains an argv-based legacy/internal path for child and stricter grant callers. File tools are contained under readable execution roots, including symlink resolution; mutations outside configured write roots require approval. `write_file` defaults to create-only, requires explicit append/edit/replace intent for existing files, and rejects exact edits with zero or multiple matches. Read-only tools work before a plan; mutating or unknown tools, including every mutation-capable process surface, require the universal plan gate. Parent-owned todo metadata mutations retain their narrow exception. `memory_remember` reuses `MemoryService.Remember`, forces global `durable_memory` scope. When `ArtifactStore` is configured, the ordinary parent composition also exposes read-only `artifact_read`; it pages opaque, session-bound command-output artifacts with strict bounds and never exposes storage paths. See [command-output-artifacts.md](command-output-artifacts.md).

The ordinary parent also exposes the separate durable todo ledger tools
`todo_create`, `todo_list`, `todo_show`, `todo_update`, `todo_complete`, and
`todo_archive`. They are absent from child/restricted compositions, retain
prompted-mode approval for mutations, and use a narrow plan-gate exception so
human/assistant todo maintenance is not conflated with coding plans. The
ledger's private checksummed snapshot and lifecycle are documented in
[todo-ledger.md](todo-ledger.md).

## Sessions and context

A session has a stable ID, safe metadata, and an append-only JSONL event history. Typed events cover lifecycle, messages, tool calls/results, plans, approvals, denied-action audits, provider usage, and deterministic compaction. The JSONL store uses only the Go standard library, accepts an injected root for tests, tolerates a truncated final line, and reports corruption in completed lines. Session files are 0600 and faithfully retain local conversation/tool payloads, which may include credentials; credentials therefore remain private local session state. External/API display projections apply bounded structural redaction, and provider replay filters malformed or structurally incomplete historical tool groups without rewriting durable events. A non-blocking advisory turn lease serializes the complete run lifecycle for each durable session across store instances and processes; append locks remain narrower persistence guards. Session deletion acquires the same turn lease, so it cannot race an active provider/tool turn.

Messages are a derived view of events. `SessionStore` supports create, append/replay, load/resume, list summaries, fork at an event boundary, and compaction events without rewriting history. Context assembly is a separate service that assigns each item a source, priority, required flag, sequence, and deterministic size estimate. Required system/project/recent material is preserved; optional older material is evicted first under a caller-supplied budget. `Summarizer` is an interface seam only; Phase 3 does not call a model to summarize.

Project instructions are separate from durable user memory. Bounded discovery reads only `AGENTS.md` files from filesystem ancestors through the project root and then project-root descendants toward a target. The order is explicit and testable: later, more specific files have higher precedence. No arbitrary files are silently loaded into context.

## Identity, user profile, durable memory, and transient context

Phase 7A–7D keeps four boundaries explicit:

- `IdentityProfile` describes the assistant and is not a memory record or session prompt;
- `UserProfile` stores explicit user attributes/preferences and is not session history;
- `MemoryRecord` is bounded durable memory, addressed by a safe namespace/key and classified by sensitivity;
- `MemoryContextPacket` is a bounded transient projection for one request and must not be persisted as durable memory.

`MemoryPolicy` defaults to explicit writes only, no sensitive memory, no age expiry for durable records, bounded records/bytes/queries/context, and child read-only access with proposal submission. `MemoryEvent.ValidateForStore` is the parent-owned write gate: store mutations require a parent actor, an explicit flag, a valid audit event, and policy approval. `MemoryProposal.Validate` accepts only child-submitted proposals; a proposal is not a durable write until a parent explicitly accepts it and emits a store event. `NewLocalMemoryStore` implements `MemoryStore` with private append-only JSONL events, metadata-invalidated replay caching, deterministic versions/tombstones, fail-closed corruption handling, per-root process mutexes, and conservative cross-process lock files. `RetrieveRanked` adds bounded lexical scoring before candidate truncation, filter-before-score privacy, optional filtered semantic search, diversity, and transient context projection. `LocalMemoryService` adds explicit lifecycle operations, proposal review, bounded/redacted views, atomic import, and a best-effort mutation hook used by the optional incremental semantic refresher. Post-turn extraction and the local reranker are explicit config opt-ins; equivalent extraction output is idempotent. Proposal approval, cleanup, compaction, irreversible purge, and remote synchronization are never automatic. See [memory.md](memory.md), [memory-retrieval.md](memory-retrieval.md), [local-memory.md](local-memory.md), and [memory-lifecycle.md](memory-lifecycle.md).

## Policy and approvals

Policy is evaluated before side effects. It should be able to classify an action, deny it, allow it, or require approval. Approval is a first-class request with an expiry, an action summary, and an auditable decision; interfaces render and submit manual approvals but do not implement policy themselves. The config-only auto-approval mode resolves eligible requests inside the same executor while preserving plan, policy, scope, and sandbox checks and recording a bounded durable outcome. Package test/activation remains human-reviewed. No extension, subagent, or channel gets an implicit bypass.

The default Phase 2 coding policy allows read-only tools and requires approval for writes, exact edits, and command execution. Callers supply policy, approval, and audit implementations through small interfaces. Unknown policy decisions fail closed.

## Planning and delegation seam

`TaskPlan`, `ScopedTask`, and `ChecklistItem` are serializable data contracts with validation for inputs, outputs, ordered steps, assumptions/questions, variables/types/scope, acceptance criteria, task scopes, and checklist entries. Plan mode is off by default, persisted per session, and controlled by `/plan on|off|status|reset|complete`, `--plan`/`--no-plan`, or the `assistant.require_plan` default. Enabling starts a fresh planning epoch. While enabled, the internal `bee_plan`, `plan_status`, and `plan_update` tools record and validate the lead-model plan before mutating or unclassified tools may run. Read-only tasks do not require a plan. Equivalent submissions reuse the current plan; materially different replacements require a reset. The current plan and its fingerprint are injected as durable runtime context after reload or compaction.

Controlled turn boundaries append a structured checkpoint containing the active
plan projection, completed-call and observation fingerprints, blockers, and
the next action. Web clients continue with the checkpoint action; interactive
CLI/TUI clients use `/continue [checkpoint-id]`, and one-shot CLI callers use
`--continue`. Selecting a checkpoint appends a durable resume marker. Exact
repeated calls to read-only tools on that continuation reuse the checkpoint
observation metadata instead of performing the same inspection again.

The ordinary trusted/default composition also exposes `plan_status` and
`plan_update`. Acceptance criteria use the closed states `pending`,
`in_progress`, `passed`, `failed`, and `blocked`; `passed` requires bounded,
parent-verified `observed` evidence from the same session, while `model_claim`
evidence remains explicitly non-verifying. Status includes a bounded index of
recent eligible call/artifact IDs, and updates may atomically resolve a
`latest_successful_tool_result` selector before persisting the exact call ID.
Repeated semantic evidence failures have a separate three-attempt loop guard.
Updates are durable, session-scoped plan events applied through the existing
locked JSONL store. Locked, child, and specialist-style compositions do not
receive these live tracking tools. See
`docs/plan-tracking.md`.

## Application facade and interfaces

All interfaces route into one `Application` service. `Application.Run` accepts a provider/session-neutral `RunRequest` and returns a typed `RunEvent` stream. Events cover lifecycle, session identity/metadata, assistant deltas/messages, provider-output rejection/recovery, plans, tool calls/results, approvals, usage, and failures. The facade reuses `Agent`, `SessionStore`, `Executor`, `Context`, and planning behavior; adapters never implement a second agent loop. Start/end events also feed bounded in-memory counters and an optional diagnostic sink. The ordinary composition owns a private JSONL sink under `BEE_HOME/state/diagnostics`; records include lifecycle metadata and duration but exclude prompt, response, tool arguments/results, and filesystem paths.

- **print/REPL:** human-readable assistant text over the same durable, tool-capable general assistant;
- **JSON:** newline-delimited JSON events, with stdout reserved for JSON objects;
- **TUI:** fullscreen persistent-composer UI with a compatible line-mode fallback, streamed assistant/reasoning sections, bounded redacted tool details, colorized diffs, approvals, usage, and a structured runtime status card;
- **RPC:** local stdio JSON-RPC 2.0 with `run`, `continue`, `list_sessions`, `resume`, and `inspect_session`, streamed as `event` notifications followed by a request response.
- **OpenAI HTTP:** authenticated `/v1/chat/completions`, `/v1/models`, and
  `/healthz`; one HTTP request consumes one complete harness turn, and SSE
  projects assistant deltas plus keep-alive comments while tools run.

Adapters accept injected readers/writers where applicable so event rendering
and dispatch remain directly testable. `HarnessHandlers` is the shared
transport-neutral request layer used by stdio RPC and OpenAI HTTP, so neither
adapter owns or forks the agent loop. HTTP conversation IDs are durably bound
to Bee sessions and the durable transcript remains authoritative. See
[openai-serving.md](openai-serving.md) for authentication, session mapping,
streaming, and approval behavior.

## External extensions

Extensions are separate, explicitly allowlisted processes behind `bee.extension.v1`, a newline-delimited JSON-RPC 2.0 protocol distinct from the Phase 4 client RPC. `ExtensionHost` owns process lifecycle, filtered environment, bounded frames/messages, correlation IDs, initialize identity/capability discovery, request timeouts, cancellation, shutdown, stderr diagnostics, crash detection, and failure isolation. stdout is protocol-only.

Phase 5 implements external tools and notifications. Advertised tools are wrapped as ordinary Bee `Tool` values and registered in the existing `Registry`; calls therefore pass through the ordinary `Executor` for JSON validation, root scope, policy, approval, output limits, and audit. Application integration uses `StartExtensions`/`CloseExtensions` and does not create a second execution path. Providers, hooks, and context are represented as bounded discovery seams only. Extension data is untrusted and cannot inject system instructions or bypass context budgets. See [docs/extensions.md](extensions.md).

## MCP client boundary

Configured MCP stdio servers are another adapter into the ordinary tool
registry, not Bee extensions and not a parallel executor. `MCPHost` reuses the
application execution scope's process sandbox, filters the child environment,
owns the handshake-based MCP `2025-11-25` lifecycle, bounds JSON-RPC frames and
pagination, validates advertised object schemas, and projects accepted tools
under `mcp_<server>_<tool>` names. The shared executor remains authoritative
for plan gates, policy, owner approval, call timeout, output bounds, audit,
sessions, and run events. MCP annotations are never authorization input;
operator configuration is the only source of read-only classification.

MCP lifecycle failures are isolated per server. An unavailable server does not
fail application startup, the HTTP gateway, or an active session; only its
tools are absent or return a bounded unavailable error. See
[docs/mcp-client.md](mcp-client.md).

## One bounded child boundary

Phase 6A/6B keeps delegation composable by making the parent own the complete `ChildTask` contract:

- stable parent/task IDs plus objective, prompt, and acceptance criteria;
- an explicit root-bounded `ExecutionScope` and allowed tool-name list;
- timeout, maximum tool steps, and maximum output bytes;
- a root-contained artifact directory.

`ChildRunner.Run` creates exactly one separate `Agent` with a copied executor and an allowlisted registry. Built-in tools are rebound to a narrower child root when possible; custom tools cannot be used with a narrower root unless they already share the exact parent root. Policy, audit, and approval hooks are inherited rather than delegated, so the child has no path to self-approve or expand permissions. A child with no executor is read-only model work; a child with a risky tool still fails closed without a parent-supplied approval hook.

The runner bounds assistant output, uses a context timeout, records cancellation/timeout/budget states, snapshots bounded filesystem metadata/content hashes for changed-file evidence, and writes a root-contained `completion.json`. The returned `ChildCompletionPacket` is descriptive only: status, summary, changed files, commands/tests/evidence, uncertainty, artifacts, questions, and blockers.

`ProcessChildRunner` implements the same `ChildExecutor` interface over `bee.child.v1`. On supported Linux hosts it starts one explicit command in a scope-contained working directory with a filtered environment, bounded frames/messages, startup/request timeouts, and a dedicated Unix process group. Cancellation sends `$/cancelRequest` and then kills the complete group after a short grace period. Provider requests and approval requests travel back to the parent; credentials are never sent to the child, and missing approval fails closed. The process child receives no session history or durable memory. Process mode currently supports built-in coding tools; custom/extension tool brokering and custom policy/audit serialization are deferred. If a parent has a custom policy or auditor, process mode fails closed rather than silently bypassing that boundary. Windows process cleanup is explicitly out of scope. See [child-process.md](child-process.md) and [platform-support.md](platform-support.md).

## Sequential/parallel orchestration and traces

`ChildObservableExecutor` extends the unchanged `ChildExecutor` contract with an optional `RunObserved` method. `ChildRunner` maps its existing `AgentEvent` hook to `ChildObservation`; `ProcessChildRunner` forwards `bee.child.v1` event notifications from the child transport. The event path is observational only: an observer cannot change task scope, tools, approvals, or completion decisions.

`OrchestrationPlan` is parent-owned and validates stable IDs, parent identity, root-contained scopes, non-overlapping artifact directories, dependency references, duplicate edges, cycles, non-negative aggregate budgets, and a non-negative `MaxConcurrent` bound. `SequentialOrchestrator` chooses lexically smallest ready task IDs and runs exactly one child at a time. `ParallelOrchestrator` preserves that ordering for readiness, starts at most `MaxConcurrent` independent tasks, and waits for bounded completion results. A failed or budget-exceeded task blocks dependents; cancellation propagates through the shared run context; independent tasks may continue only under the explicit continue-independent failure policy.

`ChildTask.Mutability` is explicit. Parallel validation infers read-only only for no tools or the built-in `read_file`, `list_files`, `find_files`, and `grep` set. Mutation-capable built-ins and unknown/custom tools fail closed unless the task is explicitly mutable; mutable tasks are accepted only when execution is serialized (`MaxConcurrent <= 1`). Artifact directories may not conflict with another task's scope; shared roots are allowed for read-only tasks when artifact directories remain distinct. This slice does not create worktrees or permit concurrent writers.

`TraceSink` is a provider-neutral append-only seam. `JSONLTraceSink` stores bounded orchestration, task, child observation, completion, artifact, and failure records with 0600 permissions. `WorkspaceManager` is a separate provider-neutral lease seam. `GitWorkspaceManager` uses bounded `exec.CommandContext` argv calls only, refuses dirty or ambiguous repository state, records lease lifecycle events, and never mutates the main worktree. `WorktreeOrchestrator` preserves mutable worktrees for inspection and emits `IntegrationProposal`; explicit cleanup is available, while applying a proposal, automatic merging, retries, background scheduling, and automatic approval delegation remain deferred.

## Separation from OpenClaw

Bee is a separate Go repository and runtime. It may eventually interoperate with OpenClaw through explicit adapters or migration tooling, but it must not import OpenClaw internals, depend on OpenClaw's daemon lifecycle, or assume OpenClaw's configuration and state layout. Shared behavior should be specified at a protocol boundary and tested as an integration, not achieved through hidden coupling.

During coexistence, choose one owner for each piece of state and capability. Document temporary bridges, their failure modes, and their removal conditions. The goal is independent build, test, operation, and eventual migration—not a second implementation of OpenClaw's internals.

## Runtime context boundary (Phase 7E)

Runtime context is assembled additively from typed sources. Identity and project instructions are required/high priority; user profile is high priority; retrieved memory and prior-conversation snippets are optional and droppable. Profile, memory, and conversation recall render as delimited reference data rather than arbitrary prompt authority. Ordinary `Agent.Run` turns default to hybrid recall: Bee searches durable memory for substantive prompts, injects only confidently relevant results, and skips acknowledgements, control-like prompts, explicit memory writes, and recall opt-outs. Hybrid packets are capped at eight records and 16 KiB; explicit `memory_search` remains available for deeper lookup. Prompts that refer to earlier discussions can additionally receive up to four bounded snippets from prior sessions, excluding the active session. `off` disables automatic recall, while `always` preserves unconditional per-turn retrieval for callers that require it. Transient recall packets follow the current user message, remain outside the reusable prior-turn cache prefix, and are never persisted into session events or durable memory. Context diagnostics expose only bounded counts and estimated sizes. Child memory is parent-projected, scope/privacy filtered, and read-only.

The ordinary parent/default composition additionally exposes the read-only
`context_status` tool. It keeps the turn-context selection budget separate from
the messages-only estimate of the last provider request attempt, and reuses
`ContextDiagnostics`, retrieval diagnostics, tool-loop accounting, and current
plan state to report only bounded structural/quantitative state. Estimates use
the existing deterministic approximate-token unit and exclude tool-definition
overhead. Its request snapshot describes the provider request that produced the
current tool batch, before that batch's result is appended; the status call
itself counts in the provider-tool batch step count. It is not composed into
locked, child, specialist, scout, or other restricted agents.
See [context-status.md](context-status.md).

## Channel boundary (Phase 8A)

Phase 8A keeps channel concerns outside the agent loop. A transport adapter normalizes provider updates into `InboundMessage`; the parent-owned `ChannelRouter` validates the envelope, applies `ChannelAuthorizer`, atomically claims its message/update identity through `MessageDeduplicator`, derives a deterministic `SessionRouteKey` from channel/account/conversation/thread, and calls the existing `Application.Run`. Assistant output is converted into a bounded `OutboundMessage` preserving account, conversation, thread, and reply-to identity, then sent through the registered `Channel`'s narrow `Send` method.

Identifiers reject unsafe path/control characters; message text, metadata, attachment references, filenames, content types, and voice references are bounded and secret-shaped metadata is rejected. Attachments and voice values are references only: this phase does not download files, transcribe audio, synthesize audio, call Telegram, poll, handle webhooks, retry, enqueue background work, or wire the CLI. Channel credentials and direct channel handles remain in the parent/adapter boundary and are never included in child task contracts.

`InMemoryChannel` and `MemoryMessageDeduplicator` provide deterministic test seams. Router state is serialized per route, while separate conversations receive separate durable sessions; the core `Application`, `Agent`, and `Session` behavior remains unchanged.

## Bounded web-research scout (September 23, 2026)

Optional scout mode exposes parent-facing read-only `web_research` plus bounded known-URL `web_fetch`, while preserving `Application.WebResearch` as the raw executor for compatibility paths. Raw `web_search`, read-only `web_browser`, and plan-gated `web_browser_act` are not visible to the ordinary assistant when scout mode is enabled. Locked mode exposes neither.

Each invocation creates a fresh session-free Agent on the configured local child profile, normally `local-child-8082`. Its copied allowlist contains exactly `web_search`, `web_fetch`, and `web_browser`; it has no filesystem, command, memory, delegation, extension, browser-action, or recursive scout capability. A bounded semaphore and the existing `LocalChildCapacityBroker` cap concurrent work. The child receives an ephemeral run/session binding for browser isolation and no durable history or memory.

Each invocation is two-phase. Phase A is a bounded collector with the exact raw read-only web allowlist; a hard search wrapper prevents a third upstream request and cancellation stops collection on the first blocked/degraded search. The parent hook extracts successful normalized observations into a bounded 24 KiB evidence packet, promotes fetched/browser-inspected URLs over search-only candidates, preserves relevance/observation order, canonicalizes sensitive citation URLs, and sanitizes invented prose URLs. Browser tabs close before Phase B. Phase B is a fresh tool-free child Agent on the same local profile, with no executor, memory, specialist, or recursion; it synthesizes only from the untrusted evidence packet, grounded labels, and optional collector draft. Tool counts remain collector-only, while bounded provider-call metadata may report both phases. Queue waiting consumes the same overall timeout budget, with reserved synthesis time. See [web-research-scout.md](web-research-scout.md).

## Phase A two-tier local endpoint controls (September 19, 2026)

The parent composition root now owns a shared `LocalChildCapacityBroker` for
bounded work directed at the configured child/specialist endpoint. Child
runners, specialist helpers, recon review, and child-backed post-turn memory
extraction acquire one non-reentrant lease per endpoint operation. The broker
provides bounded concurrency, priority/FIFO lanes, queued cancellation, and
non-secret snapshots; it does not own final answers, tools, approvals,
mutations, durable writes, or recursive orchestration. Zero timeout continues
to mean parent-context-only cancellation.

Specialist observation compression first performs deterministic duplicate and
boilerplate removal while preserving high-signal errors, exit codes,
identifiers, and head/tail context. Lossless/structured observation tools are
not changed. The optional tool-free `EvidenceExtractor` returns strictly
validated, bounded, non-authoritative JSON with exact supplied anchors and
suggested verification targets; Phase A does not auto-run it on every result.
