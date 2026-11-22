# Read-only memory inventory

M1 inventories approved source files before any memory migration. It does not
create Bee `MemoryRecord` values, write to OpenClaw memory, extract sections,
or alter runtime ownership.

## Command

```sh
bee memory inventory \
  --root long-term=/home/user/.openclaw/workspace/MEMORY.md \
  --root memory=/home/user/.openclaw/workspace/memory \
  --output generated/memory-inventory.json
```

`--root` is repeatable and must be an explicit `LABEL=ABSOLUTE_PATH` source.
The command can also emit JSON to stdout by omitting `--output`.

Default exclusions are:

- `.dreams`
- `.openclaw-runtime`
- `.openclaw`
- `session-corpus`
- `generated`
- `archive/dreaming`

Additional exclusions can be supplied with repeatable `--exclude` flags.
Symlink roots are rejected; symlink entries inside a root are skipped. Source
files are never modified.

## Manifest contents

The owner-only manifest contains bounded metadata only:

- source root label and relative path;
- coarse source classification (`curated_long_term`, `curated_topic`,
  `daily_note`, `archive`, or `source_file`);
- file size, line count, modification timestamp, and SHA-256;
- duplicate relationship when content hashes match;
- excluded/error status and a bounded reason;
- aggregate counts, byte totals, truncation state, and diagnostics.

It never contains source text, secret values, API keys, prompts, or credentials.
Files with sensitive-looking names (`.env`, `secret`, `credential`, `password`,
`.pem`, `.key`) or secret-shaped content are excluded and have no hash in the
manifest.

The scan is bounded by default to 10,000 files, 4 MiB per file, and 128 MiB of
included source bytes. Output refuses to overwrite an existing manifest and is
written with mode 0600.

## Boundary

A manifest is inventory evidence, not an import file. M2 will separately split
approved sections and create candidate records. Nothing in M1 changes OpenClaw
memory or Bee durable memory.
