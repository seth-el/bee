# Bee operations runbooks — Phase 10G

These runbooks describe explicit recovery and never require OpenClaw or
recommend deleting state files casually. The portable product boundary is
foreground-first; a private deployment may add an operator-managed supervisor
such as the current `systemd --user` `bee-gateway.service`, which must preserve
the same backup, validation, health, readiness, and rollback boundaries.

## Common safety rules

- Use a clean private operations root.
- Run `bee health` before `bee readiness`.
- Use dry-run before every maintenance/restore/migration mutation.
- Preserve the latest backup before applying a migration or restore.
- Do not place tokens/API keys in configs, archives, diagnostics, or command arguments.
- A held lock means another owner exists; do not remove the lock file while its owner may be alive.

## Runbook index

- [Configuration and permissions](configuration.md)
- [Health and readiness](health-readiness.md)
- [Jobs and stale recovery](jobs.md)
- [Running the scheduler on a timer](recurring-jobs-timer.md)
- [Cleanup](cleanup.md)
- [Backup and restore](backup-restore.md)
- [Upgrade and rollback](upgrade-rollback.md)
- [Provider/channel failure](provider-failure.md)

## Phase 10 closeout gate

A release is operationally acceptable when a clean private root can validate config, report health/readiness, run a foreground workflow, create a backup, restore into a clean root, plan/apply bounded **diagnostics-only** cleanup, run migration dry-run, recover a stale job explicitly, and follow the documented restore-based rollback procedure—with no OpenClaw dependency and no silent destructive mutation. Session, artifact, and job retention workers are outside the supported automatic maintenance boundary.
