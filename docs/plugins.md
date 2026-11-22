# Bee plugins

> **Not model-facing since 2026-11-12.** The main assistant no longer offers the tools below (`plugin_create, plugin_test, plugin_enable, plugin_list`) (`prunedForegroundTools` in `assistant.go`; usage review in AGENTS.md). They remain implemented and tested, and restoring one is a one-line change.

Bee plugins are private, semantic-versioned external-process packages. They
add tools through `bee.extension.v1`; Bee does not load Go shared objects or
other package code into the host process.

## Package contract

A plugin is a directory containing `plugin.json` and its declared executable:

```text
weather/
├── plugin.json
└── bin/
    └── weather-extension
```

```json
{
  "schema_version": 1,
  "name": "weather",
  "version": "1.0.0",
  "description": "Provide bounded local weather tools.",
  "extension": {
    "name": "plugin.weather",
    "command": "bin/weather-extension",
    "tools": ["weather_lookup"],
    "env_allowlist": ["PATH"],
    "startup_timeout_seconds": 5,
    "request_timeout_seconds": 30
  }
}
```

Names and versions use the same validation as skills. Paths must remain inside
the package. Unknown fields, symlinks, special files, unsafe permissions,
oversized packages, and duplicate capabilities are rejected. The initialize
handshake must advertise exactly the manifest tool allowlist and may not
advertise provider, hook, or context capabilities.

Installed revisions live under `BEE_HOME/plugins/NAME/VERSION`. Only one
revision is active; the previous revision is retained for rollback.

## Operator lifecycle

```text
bee plugin test --source ./weather --start-extension
bee plugin install ./weather
bee plugin enable --version 1.0.0 weather
bee plugin list
bee plugin inspect weather
bee plugin rollback weather
bee plugin disable weather
bee plugin remove --version 0.9.0 weather          # dry-run
bee plugin remove --version 0.9.0 --apply weather  # delete
```

Enabling, disabling, or rolling back a plugin changes the next Bee
composition. Restart the current CLI or service to load the selected process.

## Model builder workflow

Ordinary unlocked parent assistants expose three plugin builder tools:

`plugin_list` is a separate read-only catalog of enabled revisions and their
declared tools.

1. `plugin_create` creates an inert source tree under
   `.bee-builds/plugins/NAME/VERSION`. Its executable is deliberately a failing
   placeholder.
2. The model implements the package using its normal scoped coding tools.
3. `plugin_test` requires human approval before executing model-authored code,
   then performs full static validation and a live initialize handshake,
   returning the deterministic package checksum.
4. `plugin_enable` accepts the source path and that exact checksum. Bee always
   routes this tool through a second human approval, even under trusted policy.
   After approval, Bee repeats the live test, refuses changed bytes, installs
   the revision privately, and enables it for the next composition.

Web gateway approvals use the existing `/api/approvals` flow. The fullscreen
TUI displays an inline approval prompt; press `y` to approve or `n`/Enter to
deny. Interfaces without an approval hook fail closed. Direct lifecycle CLI
commands remain an operator escape hatch.

## Security boundary

Plugin processes run as the Bee operating-system user. The extension protocol,
environment allowlist, exact tool list, timeouts, output bounds, normal tool
policy, approvals, and audit trail constrain their Bee-facing behavior, but
they are not an OS sandbox.

The approval-only test and activation path prevents a model from executing or
activating generated code through the dedicated tools without review. It
cannot make an unrestricted shell safe: a model granted
trusted host `exec` has the same OS authority as its operator and could invoke
the lifecycle CLI directly. For a hard self-extension boundary, do not grant
trusted host shell authority, or run Bee and the approval/install service under
separate OS identities.
