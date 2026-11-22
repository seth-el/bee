# Cleanup

Always plan first:

```sh
bee maintenance cleanup --ops-config /private/bee/ops.json
```

Apply only after reviewing the bounded report:

```sh
bee maintenance cleanup --ops-config /private/bee/ops.json --apply
```

Cleanup currently targets eligible derived diagnostics only. Session, artifact, and job retention settings are unsupported and fail validation; Bee does not purge authoritative job/session state automatically. If cleanup reports an error, preserve the report and investigate the specific candidate; do not delete the root manually.
