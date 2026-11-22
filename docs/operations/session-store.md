# Session store location

Bee keeps session transcripts in `~/.bee/state/sessions` (`$BEE_HOME/state/sessions`
when `BEE_HOME` is set). Set `BEE_SESSION_ROOT` to an absolute path to use a
different store (tests, unusual installs).

`XDG_DATA_HOME` no longer chooses the store. Older builds used
`$XDG_DATA_HOME/bee/sessions` whenever `XDG_DATA_HOME` was set and `BEE_HOME`
was not. To keep such installs working until they are moved, Bee still opens
that legacy store while it holds sessions and the new store holds none.
`bee doctor` reports this as a `session_store` warning.

Each file in the store is a session (`<id>.jsonl`) or one of its sidecars
(`.lock`, `.turn.lock`, `.idx.json`, `.search.idx`), plus the `mutations/`
journal and `scratchpad/` directories. Bee refuses to open a store through a
symlink, so there is no compatibility link.

## Moving a legacy store

1. Stop everything that has sessions open: `systemctl --user stop bee-serve
   bee-telegram`, and quit any TUI.
2. `bee sessions migrate` shows what would move. `bee sessions migrate --apply`
   moves the whole directory with one rename. It refuses, changing nothing, if
   a session lock is held, if the destination is not empty, or if the two
   locations are on different filesystems.
3. Point any adapter that names the old path at the new one. For
   `bee-telegram`, update `session_root` in its adapter config and the unit's
   `ReadWritePaths`.
4. Start the services again and check `bee doctor` (`session_store` passes).

Running the migration again is a no-op. Job contracts may not include either
location (`recurringContractRoots`), and neither can become a launch-directory
write root.

## Versioned history (redacted)

Raw transcripts are never committed: they keep tool arguments and output
verbatim, including any secret a command printed. What is versioned is a
redacted copy:

```sh
bee sessions export-history            # write ~/.bee/history/sessions
bee sessions export-history --commit   # and commit it (repo created on first use)
```

`~/.bee/history` is its own git repo, outside every agent write root, so the
agent cannot rewrite even the copy of its ground truth, and a secret that slips
through can be purged from this repo without touching the workspace (memory)
history. Redaction has two layers:

1. Exact known values: every file under `~/.bee/secrets`, plus token, key,
   secret and password files named by `~/.bee/*.json` (for example the
   Telegram adapter's `token_file`).
2. Secret-shaped strings: `key=value`/`key: value` assignments, `Bearer`
   tokens, private-key blocks, common token prefixes (`sk-`, `ghp_`, `glpat-`,
   Slack, AWS, Google, Telegram bot tokens), and values under secret-named
   JSON keys, also inside JSON tool arguments.

A secret in neither layer still gets through, for example a password typed
into chat that is not stored under `secrets/`. If that happens, add the value
to a file under `~/.bee/secrets/`, re-export, and purge the old commits from
`~/.bee/history` (for example with `git filter-repo`). The copy is derived:
deleting `sessions/` and exporting again rebuilds it.

Only changed copies are rewritten, so each commit contains just the sessions
that grew. A line still being written is left for the next export.

## Nightly commit and push

`docs/operations/bee-nightly-commit.sh` commits the workspace repo (memory,
`SOUL.md`, `BEE.md`; images and other media are ignored) when it changed,
exports and commits the redacted history, and pushes each repo that has an
`origin` remote. Every step runs even if an earlier one failed, and the script
exits nonzero if any did. `bee doctor`'s `history_remotes` check warns when
either repo has commits its origin does not have.

```sh
install -m 0755 docs/operations/bee-nightly-commit.sh ~/.local/bin/bee-nightly-commit
cp docs/operations/systemd/bee-nightly-commit.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now bee-nightly-commit.timer   # 03:00 nightly
```

Remotes must be private: the history is redacted but still conversation
text, and the workspace is the assistant's memory. Pushes use the Bee user's
SSH key, so the key must work non-interactively (no passphrase prompt).

## Redacting other files for a repo

`bee sessions redact-file INPUT OUTPUT` writes a copy of any file with the same
two redaction layers (for example a chat export kept under
`workspace/archive/`). A single JSON document is redacted structurally and
re-encoded; JSON Lines and text files line by line. Commit the copy and keep
the original out of the repo.
