# Bee

Bee is a self-hosted personal-agent runtime written in Go. It runs an
assistant against OpenAI-compatible model servers (typically local ones such as
llama.cpp), gives it policy-controlled tools inside a sandbox, keeps durable
sessions and long-term memory on disk, and exposes it through a terminal UI, an
HTTP API, and Telegram.

It is a personal project, built to be read: boring standard-library Go,
explicit boundaries, and bounded everything. It targets Linux only, and the Go
API is not stable.

## Highlights

- **Sandboxed, policy-controlled tools.** File, search, shell, web, and
  memory tools run through one registry and executor. Commands run under
  Bubblewrap with explicit write roots, and secrets are hidden from sandboxed
  processes. Policies (`trusted`, `prompted`, `locked`) and approval modes
  decide what needs a human, and `/network off` revokes network access for a
  single session. See [permissions](docs/permissions.md).
- **Durable sessions.** Every conversation is an append-only JSONL log with
  one active turn at a time, resumable from any client. Sessions can be
  searched, titled automatically, and exported as a redacted Git history. See
  [conversation history](docs/conversation-history.md) and the
  [session store](docs/operations/session-store.md).
- **Context management.** Request admission against a known context window,
  pruning of oversized old tool output, and compaction checkpoints whose
  summary can be written by the model, with recent turns kept verbatim. The
  source transcript is never rewritten. See [context kernel](docs/context-kernel.md).
- **Long-term memory.** A local, append-only memory store with lexical,
  temporal, and optional semantic retrieval, an explicit review and import
  pipeline, and lifecycle states so stale facts can be retired rather than
  deleted. See [memory](docs/memory.md).
- **Scheduled autonomy with a leash.** Recurring jobs declare a contract:
  allowed tools, writable and read-only paths, and time and tool budgets.
  Anything outside the contract pauses the job and lands in a notification
  inbox instead of failing silently. A nightly memory-consolidation
  ("dreaming") job is included as an example. See [jobs](docs/operations/jobs.md),
  the [notification inbox](docs/notification-inbox.md), and
  [the dreaming job](docs/memory-dreaming/recurring-job.json).
- **Extensible.** An MCP client (stdio), versioned skills and plugins,
  subprocess extensions, lifecycle hooks, and language-server diagnostics, all
  routed through the same policy and approval path as built-in tools. See
  [MCP](docs/mcp-client.md), [skills](docs/skills.md),
  [plugins](docs/plugins.md), [extensions](docs/extensions.md),
  [hooks](docs/lifecycle-hooks.md), and [LSP](docs/lsp-integration.md).
- **Child agents.** Bounded child tasks with explicit scopes and budgets,
  read-only parallel children, and isolated Git worktrees for mutable work. See
  [orchestration](docs/orchestration.md).
- **Several front ends, one core.** A fullscreen TUI and line-oriented CLI, an
  authenticated OpenAI-compatible HTTP server with session, command, approval,
  and run-lifecycle endpoints, a Telegram adapter (with optional voice), and a
  small web gateway. See [OpenAI-compatible serving](docs/openai-serving.md),
  [Telegram](docs/telegram.md), and [gateway](docs/gateway.md).
- **Operable.** `bee doctor`, health and readiness checks, provider
  reachability with an optional fallback profile, per-call model timing, and
  backup, restore, and upgrade runbooks. See [operations](docs/operations/README.md).

## How it fits together

```
 TUI / CLI    HTTP server    Telegram    web gateway        front ends
     \             |             |            /
      +-------- Application (runs, events) ---+
                         |
       agent loop -- provider profiles (main, child, fallback)
                         |
       tool registry -> executor -> policy / approvals / sandbox
                         |
   sessions (JSONL)   memory store   jobs + inbox   extensions / MCP
```

Front ends are adapters over the same `Application`, which streams typed run
events. Providers sit behind a narrow interface, so the core is not tied to
one API. See [architecture](docs/architecture.md).

## Quick start

You need Go 1.22+, Bubblewrap (`bwrap`), Linux, and at least one
OpenAI-compatible model endpoint.

```sh
go build -o bee ./cmd/bee
./bee init                 # create ~/.bee with a minimal config
./bee doctor --strict      # check the install
./bee "Hello"              # one turn; prints the session ID
./bee --tui                # fullscreen terminal UI
./bee --session last "And then?"   # continue the latest session
```

Bee's home is `~/.bee` (override with `BEE_HOME`): the config is
`~/.bee/config.json`, the workspace and memory live under `~/.bee/workspace`,
runtime state under `~/.bee/state`, and secrets under `~/.bee/secrets`. Bee
requires an explicit config and never starts with surprising defaults.

A minimal config with a main and a child model:

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
        "context_window_tokens": 131072
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
    "child_profile": "local-child-8082"
  },
  "assistant": {
    "policy": "prompted",
    "approval_mode": "manual"
  }
}
```

To serve the HTTP API, put a bearer token in `~/.bee/secrets/serve.token`
(mode `0600`) and run:

```sh
./bee --serve --serve-token-file ~/.bee/secrets/serve.token
```

More: [installation](docs/INSTALL.md), [workspace layout](docs/workspace-layout.md),
[operator runbook](docs/OPERATOR-RUNBOOK.md), and the full
[reference](docs/reference.md) for configuration and CLI details.

## Useful commands

| Command | What it does |
|---|---|
| `bee config validate` / `effective` | Check the config, or show it with defaults filled in |
| `bee --list-sessions` | List durable sessions (resume one with `--session ID`) |
| `bee sessions …` | Migrate, repair, title, or export sessions |
| `bee memory …` | Search, show, review, import, and maintain durable memory |
| `bee jobs …` | Create, inspect, run, retry, or dismiss jobs |
| `bee inbox` | Read and acknowledge job notifications |
| `bee skill …` / `bee plugin …` | Install, test, enable, and roll back packages |
| `bee doctor`, `health`, `readiness` | Diagnostics |

Inside a session, `/help` lists slash commands such as `/status`, `/plan`,
`/compact`, `/resume`, `/inbox`, and `/network`.

## Documentation

- **Overview:** [architecture](docs/architecture.md), [reference](docs/reference.md),
  [platform support](docs/platform-support.md)
- **Safety:** [permissions](docs/permissions.md),
  [interaction policy](docs/owner-interaction-policy.md),
  [terminal and exec](docs/terminal.md), [Git mutations](docs/bounded-git-mutations.md)
- **Context and sessions:** [context kernel](docs/context-kernel.md),
  [context status](docs/context-status.md),
  [conversation history](docs/conversation-history.md),
  [runtime context](docs/runtime-context.md)
- **Memory:** [contracts](docs/memory.md), [local backend](docs/local-memory.md),
  [retrieval](docs/memory-retrieval.md), [lifecycle](docs/memory-lifecycle.md),
  [semantic search](docs/memory-semantic.md), [mood](docs/mood.md)
- **Interfaces:** [HTTP serving](docs/openai-serving.md),
  [HTTP sessions](docs/http-sessions.md), [commands](docs/http-commands.md),
  [run lifecycle](docs/http-run-lifecycle.md),
  [attachments](docs/http-attachments.md), [Telegram](docs/telegram.md),
  [channels](docs/channels.md), [gateway](docs/gateway.md)
- **Extending:** [MCP](docs/mcp-client.md), [skills](docs/skills.md),
  [plugins](docs/plugins.md), [extensions](docs/extensions.md),
  [hooks](docs/lifecycle-hooks.md), [LSP](docs/lsp-integration.md),
  [coding backends](docs/coding-backends.md)
- **Operations:** [runbooks](docs/operations/README.md),
  [jobs](docs/operations/jobs.md), [provider failure](docs/operations/provider-failure.md),
  [backup and restore](docs/operations/backup-restore.md)

## Development

```sh
go test ./...
go vet ./...
make build        # builds ./bee
make eval         # golden-task evaluation scenarios
```

The [golden-task evaluation](docs/golden-evals.md) replays scripted scenarios
against stub models and compares the resulting tool-call traces, so harness
changes can be checked without a live model.

## License

Apache License 2.0. See [LICENSE](LICENSE).
