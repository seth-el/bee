# Bee installation

This first packaging slice is a foreground Linux install. It does not install a
service, register a package-manager entry, modify OpenClaw, or create `~/.bee`
automatically.

## Requirements

- Linux on `amd64` or `arm64`.
- Go 1.22+ only when building from source.
- Bubblewrap (`bwrap`) for the foreground shell's read-host/write-workspace
  filesystem boundary. Bee fails closed for ordinary process execution when
  write-root enforcement is active and `bwrap` is unavailable; approval-only
  `host_exec` remains the explicit unrestricted escape hatch. On NixOS, add
  `pkgs.bubblewrap` to `environment.systemPackages` when the command is absent.
  If the executable exists but the OS cannot load it, enable `programs.nix-ld`
  instead of treating that as a missing package. An explicit unsandboxed
  security downgrade is available as `assistant.unsafe_disable_process_sandbox`;
  see [permissions.md](permissions.md).
- A local OpenAI-compatible endpoint, or an explicitly configured manual
  provider profile.

## Build a release archive

From the Bee checkout:

```sh
VERSION=v0.1.0 GOARCH=amd64 BUILD_TELEGRAM=1 scripts/release.sh
sha256sum -c dist/bee-v0.1.0-linux-amd64.tar.gz.sha256
```

The release script runs `gofmt`, `go test ./...`, `go vet ./...`, builds with
`-trimpath` and `CGO_ENABLED=0`, and writes a versioned archive plus SHA-256
checksum under `dist/`. Set `SOURCE_DATE_EPOCH` for byte-for-byte repeatable
archives.

Release builds inject version, commit, and build time. Ordinary source builds
fall back to Go's embedded module/VCS build information, including a dirty-tree
suffix, so `bee version` remains useful without release linker flags.

## Inspect and install

Review the script before use, then provide an explicit absolute prefix:

```sh
scripts/install.sh --dry-run \
  --prefix "$HOME/.local" \
  --archive dist/bee-v0.1.0-linux-amd64.tar.gz
scripts/install.sh --prefix "$HOME/.local" \
  --archive dist/bee-v0.1.0-linux-amd64.tar.gz
```

The installer writes only `PREFIX/bin/bee` and, when present,
`PREFIX/bin/bee-telegram`. It never creates or mutates `~/.bee`, reads
OpenClaw state, or invokes OpenClaw. Add `PREFIX/bin` to `PATH` yourself.

## Initialize the user workspace

Initialization is explicit and idempotent:

```sh
bee init
# or: BEE_HOME=/private/bee bee init
bee doctor
```

`bee init` creates the private `~/.bee` layout, `workspace/BEE.md`, and a
0600 minimal `config.json`. The generated file records provider topology and
operator policy while inheriting current bounded runtime defaults; disabled
feature scaffolding is not emitted. Existing config and context files are
preserved, and `bee init` never overwrites them. Use `bee init --home PATH` for
a portable profile. The source checkout and user workspace remain separate.

## Configure a provider

Edit `~/.bee/config.json` (0600). The generated document contains only local
main and child profiles. Manual and cloud profiles are optional: add one with
role `manual`, include its name in `routing.manual_profiles`, and select it
explicitly:

```sh
bee --profile manual-name "one explicit request"
```

When `profiles` are configured, edit the provider nested in the selected named
profile. The top-level `provider` object is retained only for legacy documents
without profile routing; conflicting legacy and named definitions are rejected.
Configuration parsing is strict, so misspelled or duplicate fields fail fast.
The generated config includes `"schema_version": 1`; unversioned legacy files
still load as schema 1. Validate and inspect upgrades before applying them:

```sh
bee config validate --config ~/.bee/config.json
bee config effective --config ~/.bee/config.json
bee config migrate --config ~/.bee/config.json
bee config migrate --config ~/.bee/config.json --apply
```

Migration is a dry-run unless `--apply` is present. Apply validates first and
then performs an owner-only atomic replacement. Effective output expands
defaults and redacts configured secret-file locations.

For a credential, write only the key to a separate owner-only file:

```sh
umask 077
printf '%s\n' 'YOUR_PROVIDER_KEY' > ~/.bee/secrets/openrouter-api-key
chmod 600 ~/.bee/secrets/openrouter-api-key
```

Set the selected profile's `provider.api_key_file` to that path. Bee validates
that the file is regular and owner-only, and never prints its contents.
`bee doctor` checks config validity and key-file permissions without contacting
the provider.

To add personal operating instructions, create `~/.bee/workspace/SOUL.md` with
0600 permissions and keep it below 64 KiB. The optional config field
`agent.system_prompt_file` accepts an absolute path, a `~` path, or a path
relative to `BEE_HOME`; omission uses `workspace/SOUL.md`. A missing file keeps
Bee's built-in capability/truthfulness prompt. File contents are appended to
that prompt. Omit `agent.system_prompt` for SOUL.md composition; an explicitly
configured non-empty value takes precedence over SOUL.md.

`bee init` also creates `~/.bee/workspace/BEE.md`. Parent agents append this
canonical file as delimited workspace context after the effective system
prompt. It must remain a regular, non-symlink 0600 file no larger than 64 KiB,
is re-read when an agent is constructed, and is never projected to child-role
agents. An explicitly empty `agent.system_prompt` disables the complete system
message for legacy compatibility. Do not store secrets in either prompt file.

## Verify

```sh
bee version
bee doctor --strict
bee --config ~/.bee/config.json "smoke test"
```

Provider endpoint probing is an explicit separate action:

```sh
bee providers accept --config ~/.bee/config.json
```

## Memory migration

Keep source checkout, secrets, sessions, and durable memory distinct. Inventory
and normalize a source before import:

```sh
bee memory inventory --root long-term="$HOME/.openclaw/workspace/MEMORY.md" \
  --root memory="$HOME/.openclaw/workspace/memory" \
  --output /tmp/bee-memory-inventory.json
bee memory candidates --inventory /tmp/bee-memory-inventory.json \
  --output /tmp/bee-candidates.jsonl
bee memory review list --candidates /tmp/bee-candidates.jsonl
bee memory import-normalized --normalized /tmp/bee-normalized.jsonl \
  --report /tmp/bee-review.json --memory-root "$HOME/.bee/workspace/memory" \
  --audit-id migration-20260820 --reason 'reviewed migration' --apply
```

Review generated artifacts and keep secrets out of the workspace. Import is
otherwise dry-run by default and refuses conflicts.
