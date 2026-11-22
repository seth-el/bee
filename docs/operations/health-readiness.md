# Health and readiness

```sh
bee health --ops-config /private/bee/ops.json --json
bee readiness --ops-config /private/bee/ops.json --json
```

Health is structural and local. Readiness checks whether configured foreground work can run. Provider/channel checks are explicit opt-ins and may contact services only when requested.

Exit nonzero on failed checks. Do not repair state from health/readiness; inspect first and use the relevant maintenance runbook.
