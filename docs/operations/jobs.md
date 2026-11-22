# One-shot and recurring jobs

## Assistant job tools

The unified assistant composes a parent-owned `JobStore` at the private Bee
state path (`~/.bee/state/jobs.json`, or `BEE_HOME/state/jobs.json`). The
model-facing tools are bounded to the foreground lifecycle:

- `job_create` records one prompt-backed, one-shot job as `pending` with
  approval required. It requires an explicit name, owner, idempotency key,
  RFC3339 `run_at`, and bounded `max_attempts`.
- `job_list` and `job_show` expose only bounded, redacted metadata; prompt
  contents and attempt details are not returned.
- `job_approve`, `job_cancel`, and `job_recover` perform explicit lifecycle
  transitions. Approval is never automatic, and child executors do not inherit
  these tools.

The model-facing slice intentionally does **not** run due jobs and cannot
create or edit recurring definitions. The operator-facing `bee jobs run-due`
command is the scheduler tick: it materializes configured recurring firings,
claims all due approved work through the durable job store, executes it, and
delivers notification intents. Run it from an owner-controlled systemd timer
or cron entry at the desired scheduler cadence. Bee does not hide a daemon in
an interactive process.

## Recurring-job inventory and design

Before recurring jobs, `JobStore` already provided atomic cross-process
claims, a persisted `running` lease, bounded retries, explicit stale recovery,
and durable notification intents. `Application.Run` already created a durable
session when `SessionID` was absent. The missing seams were a config-only
schedule, a per-firing authority contract, and recording the newly created
session on the job attempt.

Recurring definitions now live only in the primary config. Each due firing is
materialized as an approved job with a snapshot of its contract; later config
edits cannot widen queued work. The existing claim/finalize path remains
authoritative. `concurrency_policy` supports only `forbid`: a claim is rejected
while another occurrence of the same definition is running, including across
processes.

Every firing omits `SessionID`, so `Application.Run` creates a fresh durable
session. The resulting ID is stored on `attempts_log[].session_id`; model
output remains in that session journal and is not copied into the job store or
notification. Slash commands are disabled for scheduled prompts.

Example:

```json
{
  "recurring_jobs": {
    "jobs": [
      {
        "id": "workspace-health",
        "enabled": true,
        "schedule": {
          "start_at": "2026-11-03T00:00:00Z",
          "cron": "0 */6 * * *",
          "time_zone": "UTC"
        },
        "prompt": "Inspect the workspace and record a bounded health report.",
        "contract": {
          "allowed_tools": ["read_file", "list_files", "find_files", "grep"],
          "workspace_paths": ["/srv/work/project"],
          "max_wall_clock_seconds": 600,
          "max_tool_calls": 24,
          "output_target": "session",
          "notification_target": "inbox"
        },
        "max_catch_up": 1,
        "catch_up_window_seconds": 86400,
        "concurrency_policy": "forbid"
      }
    ]
  }
}
```

Schedules set exactly one of `cron` (five fields: minute, hour, day of month,
month, weekday) or `interval_seconds`. `start_at` is required and must use
UTC `Z`; cron matching may use the declared IANA time zone. Cron fields support
lists, ranges, and steps. Catch-up considers only the configured window,
emits oldest-first, and materializes at most `max_catch_up` occurrences per
tick (default one, maximum 32). An existing approved/running occurrence blocks
additional materialization so a stalled runner cannot grow an unbounded queue.

The contract is default-deny. Only named tools are exposed, and readable and
writable scope are both restricted to `workspace_paths`. Wall-clock and tool
call budgets are hard caps. The only output target in this phase is the
durable session and the only notification target is Bee's inbox. Configured
foreground policy still applies: a tool that requires owner approval cannot
be auto-approved by a recurring run. Such a firing becomes `paused`, retains
its session journal, and emits a paused notification. Missing tools, contract
violations, provider failures, and timeouts become failed notifications.
Everything outside the allowlist and workspace paths is forbidden. As an
additional first-pass invariant, a contract containing any mutating tool is
rejected when its writable workspace contains Bee's primary config, so a
scheduled model cannot create or edit recurring definitions indirectly.

`bee jobs run-due --now RFC3339` is the deterministic/stubbed-clock seam used
by tests and operational drills. It applies to both materialization and due
selection.

## Inspect

```sh
bee jobs show --store /private/bee/jobs/jobs.json --id JOB_ID
```

A `running` job is not automatically retried after a crash. The scheduler claims and persists the running lease, then releases the store lock before provider/job execution so inspection and cancellation remain available. Wait for its lease to expire (15 minutes for a one-shot job; for a recurring job, its contract's `max_wall_clock_seconds` plus 5 minutes, at most 24 hours plus 5 minutes), then choose explicitly:

```sh
bee jobs recover --store /private/bee/jobs/jobs.json --id JOB_ID --action retry
bee jobs recover --store /private/bee/jobs/jobs.json --id JOB_ID --action fail
```

Verify the resulting state with `bee jobs show`. Preserve the job store and attempts history when investigating ambiguous external side effects.

## Paused and failed recurring occurrences

A recurring occurrence that pauses (it needed an approval no headless run can
get) or fails stays in the job store and produces an inbox notification.
`bee inbox` lists unread notifications with their reason and the next
commands. The owner then chooses:

```sh
bee jobs retry --id JOB_ID                        # same prompt and contract
bee jobs retry --id JOB_ID --refresh-from-config  # after fixing the prompt or contract
bee jobs dismiss --id JOB_ID --reason "TEXT"      # never run it
```

`retry` returns the occurrence to `approved` with one more attempt; the next
`run-due` runs it as the same occurrence, so nothing is materialized twice. It
refuses when a later occurrence of the same job already exists, since running
the old one now would do its work out of order (a nightly job reading the
clock would process the wrong day); dismiss it instead. `--refresh-from-config`
replaces the occurrence's persisted prompt and contract with the job's current
definition in config. Without it, the persisted contract is used unchanged.
`dismiss` makes the occurrence terminal (`canceled`, with the reason recorded),
and `materialize` never recreates it. Both record an `owner_actions` entry on
the job (at most 8) and mark the job's inbox notifications read. The model has
no tool for either.

`bee jobs` commands default `--store` to the assistant's job store, so it can
be omitted on a normal install.

## Manual verification

1. Configure a disabled recurring definition and run `bee config validate`;
   then enable it after reviewing the exact prompt, paths, and tool allowlist.
2. Run `bee jobs run-due --store … --config … --now …` with a timestamp at
   the first occurrence. Confirm one succeeded/failed/paused job is emitted.
3. Inspect the job and confirm its latest attempt has a non-empty, new
   `session_id`; inspect that session to find the durable output.
4. Run the identical tick again and confirm no duplicate occurrence appears.
5. Hold one fixture execution open and run a second tick; confirm the second
   runner does not start an overlapping occurrence for that definition.
6. Exercise a manual-approval-only tool and confirm the job becomes `paused`
   and Bee's notification inbox receives a paused item without auto-approval.
