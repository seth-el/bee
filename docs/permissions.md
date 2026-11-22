# Permission and scope model

Bee separates filesystem scope, planning, policy, and approval. These controls
can be combined without implicitly enabling one another.

```json
{
  "assistant": {
    "policy": "prompted",
    "approval_mode": "manual",
    "scope_roots": ["/home/user/.bee/workspace", "/home/user"],
    "write_root": "/home/user/.bee/workspace",
    "require_plan": false,
    "allow_unclassified_before_plan": false,
    "unsafe_disable_process_sandbox": false
  }
}
```

## Approval and containment tiers

Approval selection is config-only and independent from policy, plan mode, and
containment. `assistant.approval_mode` applies to ordinary assistant sessions;
OpenAI-compatible serving has its own `serve.approval_mode`. Both accept only
`manual` or `auto`, default to `manual`, and reject invalid values before a
session can run. Tool/model output cannot change the mode.

| Tier | Configuration | Prompt behavior | Containment |
| --- | --- | --- | --- |
| 0 (default) | `approval_mode: "manual"` | Every policy-required or intrinsic gate waits for its existing human approval hook. | Bubblewrap and direct path checks remain on. |
| 1 | `approval_mode: "auto"` | Eligible gates resolve automatically. `skill_test`, `plugin_test`, `skill_enable`, and `plugin_enable` still require human review; enable keeps the tested-checksum gate. | Byte-for-byte the same scope and sandbox configuration as tier 0. |
| 2 (unsafe downgrade) | `assistant.unsafe_disable_process_sandbox: true` | Whichever approval mode was selected. | Ordinary process write-root containment is disabled. This is never implied by `auto`. |

Tier 1 changes only the resolution of `PolicyRequireApproval`. Policy denials
and errors, plan gates, tool registration, external capability configuration,
parent-owned operations mutation grants, scheduled-job approval, and ACP
worktree permissions remain authoritative. It adds no network or external-send
tool; if such an extension tool is already registered and policy-gated, its
eligible prompt follows the selected approval mode. `host_exec` is likewise an
existing approval-gated escape hatch, not a capability added by tier 1; in
`auto` its eligible gate is resolved automatically, so operators should remove
that tool from reduced registries that must never permit unrestricted host
mutation.

Every tier-1 auto-approved execution appends one metadata-only session audit
event with the tool name, an allowlisted target (bounded to 512 bytes), and the
success/failure outcome. Arguments and tool error text are omitted from that
durable entry. Built-in `write_file` operations additionally retain the
existing hash/preimage mutation journal and undo path. Commands, extensions,
and external effects are audit-recorded but are not thereby reversible.

`SOUL.md`, `memory/mood.md`, and workspace `AGENTS.md` are intentionally
writable in tier 1 when they fall inside configured write roots. This is the
operator's editorial-authority ("the pen") decision: audit and, for built-in
file tools, mutation journaling provide accountability instead of a special
read-only identity-file exception. Outside-write-root and symlink checks still
apply exactly as they do in tier 0.

- `policy: trusted` runs the write-root-confined foreground shell and ordinary
  mutations inside configured write roots without approval. Outside-write-root
  file mutations, `host_exec`, hash-guarded mutation undo, and other tools
  carrying Bee's intrinsic explicit-approval marker still create an approval
  gate. Manual mode asks and fails closed when no approval hook exists; auto
  mode resolves eligible gates as described above.
- `policy: prompted` exposes the same foreground shell but requires approval for mutating calls.
- `policy: locked` exposes no executor-backed tools.
- Plan mode independently requires a valid `bee_plan` before mutating or unclassified tools. Read-only tools, including `exec_readonly`, remain immediately available for lookups and inspection. It is off by default. Use `/plan on`, `/plan off`, or the `--plan`/`--no-plan` CLI overrides; `/plan status` reports the durable active plan and criterion counts, while `/plan reset [reason]` and `/plan complete [note]` close that planning epoch. `require_plan` is the configuration default for sessions without persisted mode state. Enabling starts a fresh planning epoch, while disabling preserves prior plan history without enforcing it. An equivalent `bee_plan` submission reuses the active plan; replacing it with a materially different plan requires an explicit reset.
- `allow_unclassified_before_plan` applies only when plan gating is enabled. It lets extension tools that omit mutability metadata run before a plan; policy and approval checks still apply.
- `scope_root` selects one readable filesystem root. `scope_roots` selects
  multiple readable roots. Do not configure both.
- `write_root` selects the preferred persistent-write root. `write_roots`
  selects multiple automatically writable roots, with the first as the
  relative-path and command-working-directory default. Do not configure both.
  Every write root must be inside a readable root.
- `full_machine` cannot be combined with `scope_root` or `scope_roots`; it may
  be paired with `write_root`/`write_roots` to keep full-machine reads and
  narrow persistent writes.

When neither root setting nor a CLI override is present, an initialized
`BEE_HOME/workspace` is the primary readable root, preferred write root, and
the user's home directory remains a secondary readable root. This makes
relative paths such as `memory/...` resolve inside Bee's workspace without
removing read access that was available under the previous home-only default.
If the Bee workspace does not exist yet, both defaults remain the user home.
Explicit roots and `full_machine` always take precedence; full-machine reads
still prefer an initialized Bee workspace for writes unless write roots are
explicitly configured.

The built-in file tools remain symlink-aware and confined to readable roots.
They mutate configured write roots directly and require explicit approval for
a path in another readable root. Approved mutations are journaled against the
readable root containing their resolved target, so secondary-root writes retain
the same hash-guarded undo behavior as primary-root writes. Foreground `exec` uses Bubblewrap on Linux to
expose the host read-only while rebinding only the configured write roots as
writable. It preserves network, SSH, process, and read access, with three
exceptions. The user runtime directory (`XDG_RUNTIME_DIR`, normally
`/run/user/<uid>`) is a private tmpfs, because its sockets (systemd user bus,
tmux, editor servers) would let a sandboxed command ask an unsandboxed process
to write outside the write roots; an `ssh-agent` socket inside it stays
reachable. That means `systemctl --user` and rootless container sockets are
unavailable inside `exec`; use `host_exec`. `~/.bee/secrets` is hidden from
every sandboxed command, and the file tools refuse it too. `/tmp` is a private
per-session directory under `state/runtime/sandbox-tmp` that persists across
commands in the session (read-only for `exec_readonly`), never the host `/tmp`
and not visible to the file tools; idle directories are pruned after 7 days.
When a command fails with `Read-only file system`, its result carries a
`sandbox_hint` naming the write roots and the ways forward.

**Launch directory.** Interactive Bee (REPL, one-shot, TUI, JSON) also makes
the directory it was started from readable and writable, as coding agents do,
and tells the model about it (`BEE_LAUNCH_DIR` in shell commands). The
workspace remains the preferred root for relative paths. It is skipped for
`--serve` and `--rpc-stdio`, with `--write-root`, `--scope-root`, or
`--full-machine`, and with `--no-launch-dir`. It is refused for `/`, the home
directory, anything containing or inside Bee's home, the session store, the
secrets directory, or the config file.

**Approve for this session.** When `write_file`, `edit_file`, `git_stage`, or `git_commit` (the git tools are not model-facing by default)
needs approval because its target is outside the write roots, the request may
carry `session_grant`: the enclosing git work tree, else the target's
directory, under the same guards as the launch directory. Answering
`approved_session` (TUI `s`, REPL `s`, or `{"decision":"approved_session"}`)
makes that directory a write root for the rest of the session, for the file
tools and the `exec` sandbox. Grants are held in memory per session (a restart
or another session starts without them) and are bounded. `host_exec` and
package activation are never session-grantable. `host_exec` is a synchronous, approval-gated
escape hatch for an explicitly intended unrestricted host mutation. It cannot
open a PTY or background session, so later commands cannot reuse an approved
unrestricted shell.

Process sandboxing is on by default and fails closed. At first use Bee checks
that `bwrap` is present and executable. On NixOS, a missing executable is fixed
by adding `pkgs.bubblewrap` to `environment.systemPackages`; an executable that
exists but cannot be loaded is a different failure and points to
`programs.nix-ld` (or using the Nix-built Bubblewrap package). Other spawn
failures report the resolved executable and underlying error. Bee never falls
back to an unrestricted process automatically.

Every Bubblewrap invocation overwrites the child environment with
`BEE_SANDBOX=1`. When a Bee process carrying that marker constructs another
sandboxed command, it validates the same scope, executable, and working
directory but skips the impossible nested Bubblewrap layer. The command is
still confined by the outer sandbox, and Bee emits the greppable warning
`process sandbox: already inside a Bee sandbox; running
unsandboxed-within-sandbox` once per process. The marker is propagated across
that direct child invocation even when the tool uses a filtered environment.
It is advisory nesting state, not an authentication or authorization boundary:
the outer Bubblewrap policy remains the security boundary, and a caller able to
forge Bee's launch environment must not be treated as trusted because of this
variable. The explicit unsafe opt-out below retains its existing behavior and
does not gain sandboxing from the marker.

`assistant.unsafe_disable_process_sandbox: true` is an explicit security
downgrade for an operator who accepts ordinary `exec` and terminal processes
running with the complete authority of the Bee OS user, including process tools
used by delegated children. The configured write roots no longer confine those
processes, even though direct file tools retain their own path checks. Results
carry `execution_mode: "unsandboxed"`, and `exec_readonly` is not exposed
because it cannot honestly claim a read-only boundary without Bubblewrap. Keep
the option omitted or `false` unless this risk is deliberately accepted.

## Per-session network switch (`/network`)

By default, sessions keep LAN and internet access: `exec`, `terminal`, remote
commands, MCP and LSP. The owner can revoke it for one session with
`/network off`, restore it with `/network on`, and check it with `/network`.
It works in the TUI, over HTTP (`POST /api/commands`) and in Telegram. It is an owner
command: no model tool changes it, and jobs never run commands. Its state is a
`network_policy` event in the session, so resuming keeps it and the transcript
shows each change. Before a session exists (a fresh TUI), it applies to the
next session. It can't change while a response is running. It works the same
in tier 0 and tier 1.

While it is off:

- New sandboxed processes (`exec`, local `run_command`, new `terminal`
  sessions) start with `--unshare-net`, the same empty network namespace
  `exec_readonly` always uses. If the sandbox can't apply it (it is disabled,
  or Bee is already inside a sandbox), the command is refused, never run with
  network.
- `host_exec`, remote (`ssh`) `run_command`, `repo_clone`, `web_fetch`,
  `web_search`, `web_browser`, `web_research`, `media_search`,
  `media_retrieve`, and MCP and extension tools refuse at call time. They stay
  in the tool list, so the prompt-cache prefix doesn't change.
- Terminals are application-wide. Turning network off closes the terminals
  this session opened while it had network, and the reply says how many. Any
  other terminal that still has network can be read or closed but not driven
  (write, run, resize, signal) from this session.
- Shared servers (MCP, extension hosts, LSP) keep running for other sessions.
  This session's calls to them are refused, and LSP diagnostics are skipped.
- Delegated in-process children inherit the setting. Process-isolated
  children refuse to start.
- Lifecycle hooks are owner config and keep running unchanged. Calls to Bee's
  own model servers are not affected.

Turning network on applies to new processes. It is shown in `/status`
(Network), in the TUI status line ("network off"), on `run_start`
(`network_off`), and on every HTTP run-lifecycle record of the run.

## Gateway approvals

When `bee-gateway` runs in prompted policy with manual approval mode, an
ordinary mutating tool call remains pending until a client decides it. Trusted
policy uses the same broker only for intrinsically approval-required tools.
Auto mode resolves eligible gates before the broker; package test/activation
still reaches it. A request that does reach the broker remains pending until a
client approves or denies it, the run is cancelled, or its context expires.

```http
GET /api/approvals
```

```http
POST /api/approvals/{id}
Content-Type: application/json

{"decision":"approved"}
```

The other valid decision is `denied`. Keep the gateway private: approval
requests contain bounded tool arguments needed for an informed decision.
Authorization failures identify the rejecting `plan`, `policy`, or `approval`
layer and include a recovery hint.
