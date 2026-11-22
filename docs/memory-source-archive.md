# Source archive

`bee memory source-archive` makes a bounded, private, read-only snapshot of
explicit source roots. It writes only under Bee's
`BEE_HOME/workspace/source-memory` area; OpenClaw source roots are never
modified. Apply refuses an existing destination unless `--overwrite` is
explicitly supplied. `--dry-run` performs the same validation, exclusions,
size checks, and hashing without creating the archive directory.

```sh
bee memory source-archive \
  --root long-term=/home/user/.openclaw/workspace/MEMORY.md \
  --root memory=/home/user/.openclaw/workspace/memory \
  --name openclaw-memory \
  --dry-run
```

Dreaming and transient material is excluded by default:

- `.dreams/` and everything below it;
- `dreaming/` and everything below it.

Additional relative paths or glob patterns can be repeated with `--exclude`:

```sh
bee memory source-archive \
  --root memory=/home/user/.openclaw/workspace/memory \
  --exclude 'topics/*.tmp' \
  --exclude 'session-corpus' \
  --name curated-memory
```

The JSON dry-run report and the persisted `manifest.json` contain the
normalized exclusion list, included file metadata and hashes, excluded paths,
`included`/`excluded` counts, and included/skipped byte totals. Excluded files
are not opened or hashed, so a large
`.dreams/short-term-recall.json`-style file does not consume the per-file byte
limit. Secret-shaped content, sensitive filenames, symlinks, non-regular files,
path escapes, and bound violations still fail closed.

The archive is separate from Bee's active durable memory store. Archiving source
files does not import, rewrite, delete, or otherwise activate any memory.
