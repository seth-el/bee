# Terminal and trusted foreground `exec`

Bee has an optional native `terminal` tool for private/development use. It is
disabled unless the primary config explicitly sets `terminal.enabled` to
`true`.

The foreground assistant exposes two shell surfaces. `exec_readonly` runs one
synchronous local inspection command with the host filesystem read-only, a
private temporary directory, and isolated network and process namespaces. It
is classified read-only, so file/configuration inspection does not need a Bee
plan. It cannot start PTYs or background sessions, use SSH, download data, or
persist writes.

`exec` runs arbitrary shell strings on the gateway host. Non-PTY calls run Bash
with inherited Bee process environment plus a validated explicit overlay.
Bubblewrap exposes the host filesystem read-only and rebinds only configured
Bee write roots writable; normal network, SSH, process, and read access remain
available. The command runs as the Bee OS user. Shell syntax, pipes, redirects,
heredocs, multiline scripts, and ordinary `ssh` are intentionally preserved.
`BEE_WRITE_ROOT`, `BEE_WORKSPACE`, `BEE_PROJECTS`, and `BEE_MEMORY` identify
canonical destinations.

Use `exec` with `pty:true` or `background:true` for interactive and long-lived
commands. It composes with the same `TerminalManager` used by `terminal`,
returning a session id and cursor/status. Follow up with `terminal` actions such
as `read`, `write`, `submit`, `interrupt`, `resize`, and `close`. Sessions and
output buffers are process-local and bounded; PTY sessions disappear on Bee
restart. Passwords entered through terminal write actions may be recorded in
tool-call history until a separate secret-input broker exists, so key auth is
preferred.

For a reusable shell, `open` returns a session id and may optionally receive a
`command` (or `input`) to start immediately. `run` sends a command and waits up
to `wait_ms` for its completion marker and output. `submit` sends a command plus
Enter and returns immediately; both actions accept either `command` or `input`,
but not both. `write` sends `input` exactly as provided for interactive programs.
`read` returns buffered bytes after `cursor` and, when `wait_ms` is positive,
waits for new bytes or session exit. Reuse the newest returned cursor to avoid
re-reading output.

`exec`, its `terminal` process follow-up actions, and approval-gated `host_exec` are
mutation-capable and require an active plan when plan mode is enabled. The
ordinary `exec` sandbox permits persistent mutation only in configured write
roots. `host_exec` bypasses that sandbox only for one synchronous command after
its gate is resolved (manually by default, or by eligible automatic approval);
it cannot create a PTY or background session. These
foreground capabilities are not projected to children, locked assistants,
specialists, web scouts, or extensions.

The old `run_command` argv/target contract remains only for legacy/internal
compatibility and is not the trusted assistant's foreground shell/SSH tool.
Do not use `run_command target:ssh`, Python password drivers, `sshpass`, or
`expect`.
