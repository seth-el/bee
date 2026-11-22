# Bee operator runbook

Bee's portable operations boundary remains foreground-first and does not
require a daemon or OpenClaw. A web deployment may also use a reviewed
`systemd --user` unit for `bee-gateway`; service management is
an operator-owned deployment layer rather than an automatic installer action.

## Normal startup and checks

```sh
bee init                         # safe to repeat
bee doctor --strict              # local structural/config checks
bee --config ~/.bee/config.json  # interactive foreground REPL
```

`doctor` reports `PASS`, `WARN`, or `FAIL`. It checks Linux/architecture,
private Bee directories, config permissions and validation, and configured
provider API-key file permissions. It does not call a provider or display
secret contents. Exit status is nonzero on `FAIL`; `--strict` also fails on
`WARN`.

If doctor reports a missing layout path, run `bee init`. Do not repair a
permission failure by making state world-readable. Directories should be 0700,
config, context, and session files 0600, and API-key files owner-only (normally
0600). Durable session history is faithful local data and may contain secrets;
never copy session JSONL or repair backups to shared locations or external
logs.

### Personal system prompt

Bee optionally loads `BEE_HOME/workspace/SOUL.md` (normally
`~/.bee/workspace/SOUL.md`) as the personal prompt file. Create it manually;
`bee init` does not require it. Use `agent.system_prompt_file` for another
absolute, `~`-expanded, or BEE_HOME-relative path. The file must be a regular,
non-symlink owner-only readable file and is bounded at 64 KiB. Bee preserves its
built-in capability/truthfulness preamble and appends the file. An explicit
non-empty `agent.system_prompt` wins over SOUL.md, which is useful for a fully
replaced operator prompt.

Parent agents also load the conventional `BEE_HOME/workspace/BEE.md` after the
effective prompt as delimited operator-authored workspace context. The same
regular-file, non-symlink, owner-only, readable, and 64 KiB rules apply. BEE.md
is not projected to child-role agents. An explicitly empty `system_prompt`
disables the complete system message for legacy compatibility. Never place
credentials or other secrets in either file.

## Provider setup and failure handling

1. Keep `local-main-8081` as the normal main role when available.
2. Keep `local-child-8082` bounded; child capacity is explicitly limited.
3. Select an operator-configured manual profile only with `--profile`.
4. Put credentials in a separate 0600 file under `~/.bee/secrets`.
5. Run `bee doctor` after editing config, then run provider acceptance only when
   a live probe is intended:

```sh
bee providers accept --config ~/.bee/config.json
bee providers accept --config ~/.bee/config.json --profiles manual-name --allow-cloud-probe
```

Provider failure never triggers automatic cloud escalation. Inspect the
selected profile, base URL, model, timeout, and key-file permissions. A local
provider outage is an operational failure, not a reason to silently change
providers.

## Memory migration and backup

Durable memory is under `~/.bee/workspace/memory`; sessions and transient state
are under `~/.bee/state`; secrets are separate. Use the inventory/candidates/
review/import workflow in [INSTALL.md](INSTALL.md) for migration. Do not copy
OpenClaw runtime logs or secret files into Bee's workspace.

For operations backups configured with an ops config:

```sh
bee maintenance backup --ops-config /private/bee/ops.json \
  --destination /secure/backups/bee.tar.gz
bee maintenance restore --ops-config /private/bee/ops.json \
  --source /secure/backups/bee.tar.gz       # dry-run
bee maintenance restore --ops-config /private/bee/ops.json \
  --source /secure/backups/bee.tar.gz --apply
```

Review conflicts before `--apply`. Provision secrets separately; normal
backups intentionally do not contain them. Preserve the old backup until a
restore and `bee doctor`/health check succeed.

## Upgrade and rollback

Verify the archive checksum before installation. Install to an explicit prefix,
then check the new binary:

```sh
sha256sum -c bee-VERSION-linux-ARCH.tar.gz.sha256
scripts/install.sh --prefix "$HOME/.local" --archive bee-VERSION-linux-ARCH.tar.gz
bee version
bee doctor --strict
```

Rollback is install-based: keep the previous archive, reinstall it to the same
explicit prefix, and rerun doctor. Do not rewrite state files by hand.

For the current user-service deployment, preserve the previous live binary
before replacement and restart only after the new build is installed:

```sh
systemctl --user status bee-gateway.service
cp -a ~/.local/bin/bee-gateway ~/.local/bin/bee-gateway.backup-$(date -u +%Y%m%dT%H%M%SZ)
systemctl --user restart bee-gateway.service
curl -fsS http://127.0.0.1:18790/healthz
curl -fsS http://127.0.0.1:18790/readyz
```

If health/readiness fails, restore the preserved binary, restart the service,
and inspect `journalctl --user -u bee-gateway.service`. Compare
`go version -m ~/.local/bin/bee-gateway`, the binary checksum, the unit's
`ExecStart`, and the intended Git revision; a docs-only HEAD may legitimately
be newer than the commit embedded in the running binary. The current LAN
listener is wildcard-bound and has no application authentication, so verify
host/VM firewall restriction before treating the service as private.

## Troubleshooting

### `config file ... does not exist`

Run `bee init`, or pass an explicit `--config` path. Initialization preserves
an existing config and will not replace it.

### `config is invalid`

Run `bee doctor --json` and inspect the config with a reviewed editor. Keep the
file 0600. Use `bee --print-example` to compare shape, then fix only the
invalid fields. Never paste API keys into tickets or logs.

### API-key permission failure

Use `stat` to inspect the file and set owner-only permissions:

```sh
chmod 600 ~/.bee/secrets/openrouter-api-key
```

The file must be regular, not a symlink, and must not be group/world readable.

### Provider request fails

Confirm the endpoint is reachable and compatible, then run an explicit
`bee providers accept` probe. Check the selected `--profile`, model, and
base URL. Bee does not silently fall back to another provider. If the error
mentions malformed tool-call arguments or a JSON parse column, do not retry the
session blindly: provider-bound assistant tool-call arguments are preflighted
locally. Inspect first with `bee sessions repair --session SESSION_ID --dry-run`; apply
only after review with `bee sessions repair --session SESSION_ID --apply`.
Repair is offline/local, lock-protected, creates a private atomic backup, and
replaces invalid historical arguments with inert JSON. Provider replay also
omits malformed or structurally incomplete call/result groups without rewriting
JSONL. It does not execute historical commands and cannot recover the original
token.

### Install fails with permission denied

Choose a prefix you own, such as `$HOME/.local`, or obtain authorization for a
system prefix through your normal host process. The installer itself does not
use `sudo` and does not modify user data.

### Need machine-readable evidence

Use `bee doctor --json`, `bee health --json`, and the release `.sha256` file.
These outputs contain metadata and statuses only; review before sharing.
# Gateway streaming checks

Long generations use resumable SSE. A browser reconnects with the last event
cursor and keeps the active run/cancel state while `GET /api/runs/{id}` reports
`running`. If a replay request returns `409` with `gap: true`, durable session
history is the recovery source. `replay_gap`/`overflow` in the status response
means the bounded in-memory event retention overflowed; it is not evidence that
the Bee process restarted. The gateway emits keepalive comments every 15
seconds and rolls individual SSE connections after 10 minutes, so clients and
proxies must permit reconnects rather than treating rollover as cancellation.
