# Supervised ACP and local-child coding backends

Bee's coding backend seam is provider-neutral and disabled by default. The
parent owns the task, plan, worktree lease, scope, approvals, cancellation,
artifacts, audit, and final review. A configured Bee child provider is the
default implementation, review, and scouting backend.

## Configuration

The main Bee config accepts a `coding` object:

```json
{
  "coding": {
    "enabled": true,
    "default_implementer": "local-child",
    "default_reviewer": "local-child",
    "default_scout": "local-child",
    "fallback_implementer": "local-child",
    "fallback_reviewer": "local-child",
    "fallback_scout": "local-child",
    "limits": {
      "max_worktree_bytes": 536870912,
      "max_artifact_bytes": 33554432,
      "max_artifact_files": 256
    },
    "backends": [
      {
        "name": "local-child",
        "kind": "local_child",
        "enabled": true
      }
    ]
  }
}
```

The `local-child` entry names the already-configured Bee child provider. The
composition root supplies its `ChildExecutor`; Bee does not launch a second
local process for this backend.

Additional ACP adapters may be configured explicitly with `kind: "acp"` and a
provider-neutral command/process configuration. Registry construction is side
effect free: commands are not resolved or launched during validation or
construction. ACP authentication supports only the explicit `api-key` method
with the allowlisted `OPENAI_API_KEY` environment variable; credentials never
enter task packets, progress events, completion packets, or artifacts.

Fallbacks are explicit per role and currently accept only `local-child`. A
fallback is attempted only when an explicitly requested ACP backend is
unavailable; task failure, timeout, cancellation, permission denial, and
malformed results are terminal and are not retried. If a mutable ACP attempt
has to fall back, Bee preserves that attempt and acquires a fresh parent-owned
worktree for the local child.

## Routing

Use `CodingBackendRegistry.Route(role, userOverride)` for explicit routing.
The user override wins; otherwise the configured role default is used. Roles
are `implementer`, `reviewer`, and `scout`. There is no prompt matching,
implicit cloud escalation, OAuth escalation, or automatic merge.

With coding disabled, role routing returns a disabled route without launching
anything. A composition root that already owns a local child executor can
continue serving the `local-child` route without registering a second backend.

## Model-facing delegation

When coding is explicitly enabled (or a parent supplies a local child
executor), the general assistant may expose `delegate_coding`. The tool accepts
one role, an optional backend override, goal/prompt, project root, acceptance
criteria, and bounded timeout, step, and output budgets. It does not accept
commands, credentials, SSH grants, memory handles, or provider OAuth paths.

The parent plan gate and delegation service validate the request before any
backend runs. Implementers and reviewers are mutable and require a
parent-owned `WorkspaceManager` plus base commit; they execute through
`WorktreeOrchestrator` in an isolated lease and produce a review-only proposal.
Scouts use the read-only child path. Child scopes strip trusted foreground,
remote authority, environment allowlists, memory projections, and non-coding
tools. Progress and terminal completion evidence are bounded and forwarded as
coding run events; the tool returns a compact `ChildCompletionPacket` envelope.

The ordinary `bee` command can compose this parent-owned boundary with
`--coding-project-root`, `--coding-managed-root`, and `--coding-base-commit`.
These flags configure the existing assistant composition; they do not create a
separate coding mode or command.
