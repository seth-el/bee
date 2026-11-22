# Durable todo ledger

Bee's ordinary parent composition provides six model-facing tools:
`todo_create`, `todo_list`, `todo_show`, `todo_update`, `todo_complete`, and
`todo_archive`. The ledger is a human/assistant metadata store, separate from
session history, durable memory, coding plans, and scheduled jobs. A todo never
executes itself, schedules work, sends notifications, spawns agents, runs
commands, mutates files, or becomes job state. `due_at` is metadata only.

## Lifecycle and concurrency

The lifecycle is closed: `open` → `completed` → `archived`, or `open` →
`archived`. Completion and archive are explicit operations. Open todos may be
archived. `todo_update` edits fields on open or completed records only; it cannot
forge lifecycle state or timestamps. Completing an already completed todo and
archiving an already archived todo are idempotent only when the supplied
`expected_version` is current; stale versions always fail. Every mutation after
create requires `expected_version`. Versions start at 1 and increase by one for
each real mutation.

An update that supplies only values already present is a successful no-op: it
does not rewrite the snapshot, append history, or consume a version. The same
rule applies to repeating `complete`/`archive` with the current terminal
version. Stale versions still fail before idempotency is considered.

Fields are bounded and validated: title, optional description, low/normal/high
priority, optional RFC3339 `due_at`, unique bounded labels, lifecycle state,
opaque service-generated ID, version, and timestamps. NUL/control/path-like,
secret-shaped, duplicate, oversized, unknown, invalid-date, and unsafe-ID input
is rejected where applicable; descriptions retain ordinary newline/tab text,
while identifiers and labels reject path syntax. Partial updates preserve omitted fields. `due_at: null` or an empty
value clears the due date; `labels: null` or an empty array clears labels.

## Storage

The default store is private `state/todos.json` below Bee's caller-rooted state
root; tests inject an explicit path. The file is a checksummed version-1 JSON
snapshot, replaced atomically through a same-directory private temporary file,
`fsync`, rename, and directory sync. Writers use a cross-process exclusive lock
file, and both local and cross-process lock waits honor context cancellation.
Private directory/file modes and non-symlinked ancestors are required.
Corrupt, checksum-invalid, oversized, or truncated snapshots fail closed and
are never silently repaired or truncated. The snapshot retains a bounded
mutation history for create/update/complete/archive audit continuity; archive
never purges the record or its history. No shell or external dependency is used.
The complete snapshot is capped at 8 MiB. Read-only operations validate but do
not rewrite or auto-migrate a healthy snapshot.

`todo_list` excludes archived records by default and supports bounded status,
priority, label, due-date, limit, and offset filters with deterministic updated
UTC timestamp/ID ordering. Due-before/after bounds are exclusive. Tool JSON is
independently capped at 256 KiB and returns `truncated`, `offset`, `limit`, and
`next_offset` pagination metadata. `todo_show` returns one bounded record. There is no
purge, recurring task, dependency/subtask, scheduler, notification, UI, memory
promotion, or task execution integration in this slice.

## Composition and policy

The ordinary trusted/default parent receives all six tools. Reads are read-only.
Mutations retain the normal policy and prompted-mode approval behavior, but use a
narrow explicit plan-gate exception so managing the ledger does not require a
coding `bee_plan`. Locked, child, specialist, scout, extension,
and process-child paths do not inherit the ledger accidentally. Capability
metadata lists only tools actually composed for the parent.

## Deployment

Deployed September 26, 2026 in implementation commit `4139388`. The gateway
and CLI both embed exact revision
`41393886101664c24cd765359f378d7877a238e3` with `vcs.modified=false`.
`bee-gateway.service` restarted cleanly at 01:58:41 UTC and health/readiness
passed. The rollback bundle is
`/home/user/.bee/backups/bee-todo-ledger-20260926T015634Z/`. Parent tool
composition and locked/child boundaries passed focused acceptance. No live todo
record was created because archive is intentionally non-destructive and there
is no purge operation; the default todo snapshot and lock remained absent.
