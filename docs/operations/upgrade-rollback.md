# Upgrade and rollback

Preflight:

```sh
bee maintenance upgrade-check --ops-config /private/bee/ops.json --manifest /private/bee/state/release.json
```

Plan migrations before applying:

```sh
bee maintenance migrate --ops-config /private/bee/ops.json --manifest /private/bee/state/release.json
```

Apply only after a verified backup:

```sh
bee maintenance migrate --ops-config /private/bee/ops.json --manifest /private/bee/state/release.json --apply
```

Rollback is restore-based: preserve the pre-upgrade backup, restore it dry-run first, apply only after conflicts are resolved, then run health/readiness. Do not guess at schemas or manually rewrite state.
