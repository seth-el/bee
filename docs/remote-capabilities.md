# Foreground shell and legacy remote capabilities

The ordinary foreground assistant exposes a first-class `exec` tool for
arbitrary shell command strings on the Bee gateway host. It runs as the Bee OS
user and preserves normal Bash and SSH behavior. Bubblewrap exposes the host
filesystem read-only and rebinds configured Bee write roots writable. The
initial `workdir` defaults to the preferred write root, but commands may read
or change directory elsewhere without gaining persistent write access there.

`exec` accepts a required `command` plus optional `workdir`, environment
overlay, timeout, `pty`, `background`, and bounded `yield_ms`. Pipes,
redirects, heredocs, multiline scripts, and shell metacharacters are allowed.
Use `pty` or `background` for interactive/long-running work, then use the
existing `terminal` actions (`read`, `write`, `submit`, `interrupt`, `close`,
etc.) with the returned session id and cursor. PTY sessions are in-memory and
bounded. Passwords typed through terminal write actions can still be recorded
in tool-call history until a separate secret-input broker exists; key
authentication remains preferred.

Foreground `exec` and its `terminal` process follow-up actions are classified
as mutating and require an active plan when plan mode is enabled. The separate
`exec_readonly` surface handles synchronous local inspection without a plan in
a read-only, networkless, host-process-isolated sandbox. `host_exec` bypasses
write-root containment only for one synchronous command after explicit
approval and also remains plan-gated; PTY/background use is rejected. These
tools are never projected to child executors, locked assistants, specialists,
web scouts, or extensions.

## Legacy/internal path

`run_command` remains only for legacy/internal and child compatibility. It is
not model-facing in the trusted ordinary assistant, and its old argv-based
`target:ssh` contract must not be used for foreground shell/SSH work. Stricter
internal callers may continue to use parent-owned remote grants and their
existing validation. Those grants are not widened by `exec`.

Bee does not use Python password drivers, `sshpass`, or `expect`, and it does
not rewrite SSH targets or force `BatchMode`/`StrictHostKeyChecking` for
foreground `exec`.
