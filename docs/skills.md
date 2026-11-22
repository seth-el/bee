# Bee skills

Bee skills are private, versioned workflow packages. An instruction-only skill
contains bounded Markdown guidance for the model. An executable skill may also
contain one subprocess that implements Bee's existing `bee.extension.v1`
protocol. Skills do not introduce a second agent loop or a second permission
system.

## Package contract

A package is a directory containing at least:

```text
review/
├── skill.json
└── SKILL.md
```

`skill.json` uses schema version 1:

```json
{
  "schema_version": 1,
  "name": "review",
  "version": "1.0.0",
  "description": "Review a change for correctness and missing tests.",
  "instructions": "SKILL.md",
  "required_tools": ["read_file", "git_diff"],
  "optional_tools": ["exec"]
}
```

Names use lowercase ASCII letters, digits, hyphens, and underscores. Versions
use semantic `MAJOR.MINOR.PATCH` syntax. Instruction and executable paths must
be package-relative and cannot traverse outside the package. Unknown manifest
fields, symlinks, special files, duplicate dependencies, oversized packages,
and unsupported control characters are rejected.

Installed packages live under `BEE_HOME/skills/NAME/VERSION`. Bee copies them
into private owner-only directories, computes a deterministic SHA-256 package
checksum, and never runs code during installation.

## Lifecycle commands

```text
bee skill test --source ./review
bee skill install --enable ./review
bee skill list
bee skill inspect review
bee skill test review
bee skill enable --version 1.1.0 review
bee skill disable review
bee skill rollback review
bee skill remove --version 1.0.0 review          # dry-run
bee skill remove --version 1.0.0 --apply review  # delete
```

Every command also accepts `--root PATH` and `--json`. Registry state is stored
atomically in a private `state.json` file
under a cross-process lock. Enabling a different installed revision retains the
previous revision for explicit rollback. The active revision cannot be removed.

Installation is local-only. Bee does not scan `PATH`, download packages, or
silently activate newly installed revisions.

## Runtime loading

An initialized ordinary assistant exposes two read-only tools:

- `skill_list` returns a bounded catalog of enabled skills and reports missing
  required tools.
- `skill_load` returns one enabled revision's instructions, version, and
  checksum. It fails if a required tool is unavailable.

The full contents of every enabled `SKILL.md` are not added to every prompt.
The model selects a relevant skill through these tools, and the resulting
tool call/result remains part of normal session provenance. Skill text is
explicitly labeled untrusted workflow guidance.

## Model builder workflow

Ordinary unlocked parent assistants also expose `skill_create`, `skill_test`,
and `skill_enable`. Creation writes an inert source package beneath
`.bee-builds/skills/NAME/VERSION`. Testing validates the package, performs a
live handshake for executable skills, and returns its deterministic checksum.
Testing is approval-required because a skill may contain model-authored code.
Enabling requires that exact checksum and is sent through a separate human
approval, including under trusted policy. Bee then re-tests the source and
refuses any post-review change before installing and enabling the revision.

The web gateway uses its approval API, while the fullscreen TUI presents an
inline `y`/`n` decision. Interfaces without an approval hook fail closed.
Instruction-only skills become visible immediately when the composition
already has a skill registry; executable skills and newly initialized
registries require a restart.

Required and optional tool declarations are dependency metadata, not grants.
All subsequent tool calls still pass through Bee's normal scope, plan, policy,
approval, audit, timeout, and output controls. Locked and child compositions do
not receive the skill tools.

## Executable skills

An optional package-contained executable is declared with an exact tool
allowlist:

```json
{
  "schema_version": 1,
  "name": "example",
  "version": "1.0.0",
  "description": "Example executable skill.",
  "instructions": "SKILL.md",
  "required_tools": ["example_lookup"],
  "extension": {
    "name": "skill.example",
    "command": "bin/example-extension",
    "tools": ["example_lookup"],
    "env_allowlist": ["PATH"],
    "startup_timeout_seconds": 5,
    "request_timeout_seconds": 30
  }
}
```

The command must be an executable regular file inside the package. Use
`bee skill test --source PATH --start-extension` to perform the initialize
handshake before installation, or test an installed revision the same way.

When an enabled executable skill is composed, Bee translates it into an
ordinary explicit extension configuration. Before registration, the handshake
must advertise exactly the manifest's tool allowlist and no provider, hook, or
context capabilities. Calls then use the normal extension executor boundary.
Executable activation changes take effect on the next application composition.

Extension subprocesses are failure-isolated but are not an operating-system
sandbox: initialization code runs as the Bee user. Install and enable only code
the operator trusts, keep environment allowlists narrow, and use Bee's normal
tool policy for exposed operations.

## Current boundaries

The implementation deliberately omits network marketplaces, automatic updates,
signatures, and hidden activation. A new revision is installed alongside the
old one and enabled explicitly. Builder activation is checksum-bound and
human-approved. Trusted host `exec` remains host shell authority and can invoke
the operator CLI directly; deployments requiring a hard separation must omit
that authority or separate the installer by OS identity.
