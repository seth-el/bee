# Running the scheduler on a timer

Recurring jobs have no trigger of their own. Each occurrence is created and run
by `bee jobs run-due`, the scheduler tick, and nothing in `bee --serve` or the
TUI calls it. Without a timer an enabled recurring job never runs.
`bee doctor` reports this as `recurring_scheduler: WARN` once an enabled job's
latest occurrence is more than an hour overdue and absent from the job store.

## The units

`docs/operations/systemd/` has a user service and timer:

- `bee-jobs.service`: one tick, `Type=oneshot`, running
  `bee jobs run-due --config ~/.bee/config.json` from `~/.bee/workspace`. With
  no `--store`, `run-due` uses the same job store as the assistant
  (`assistant.job_store_path`, else `~/.bee/state/jobs.json`).
- `bee-jobs.timer`: every five minutes, `Persistent=true`, so one tick runs
  after the machine wakes. systemd never starts a second tick while one is
  still running; the job store's claim and `concurrency_policy: forbid` also
  prevent overlap across processes.

Install:

```sh
cp docs/operations/systemd/bee-jobs.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now bee-jobs.timer
```

Check it:

```sh
systemctl --user list-timers bee-jobs.timer
journalctl --user -u bee-jobs.service -n 20   # one line per tick
bee doctor                                     # recurring_scheduler: PASS
bee inbox                                      # paused or failed runs
```

Each tick prints `run-due: nothing due`, or one line per job that ran: id,
final state, occurrence, duration, session, and for a paused or failed
occurrence its error and the recovery commands. `--json` restores the full
job records for scripts.

Disable with `systemctl --user disable --now bee-jobs.timer`. Queued and
paused occurrences stay in the job store.

## Before enabling it

Enable the timer only after a few observed runs of each recurring job look
right (see the memory-dreaming notes in `AGENTS.md`). Until then, run ticks by
hand: `bee jobs run-due --now <occurrence as RFC3339 UTC>`.
