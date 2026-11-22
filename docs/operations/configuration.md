# Configuration and permissions

## Symptoms

`bee health` or `bee readiness` reports an invalid version, unsafe permission, symlink, missing root, or OpenClaw path.

## Inspect

```sh
bee health --ops-config /private/bee/ops.json --json
stat -c '%a %U:%G %n' /private/bee/ops.json /private/bee/state /private/bee/diagnostics
```

## Recovery

- Fix the configuration in a reviewed private file with mode `0600`.
- Create existing state directories with mode `0700`.
- Remove path references to OpenClaw; do not point Bee at OpenClaw state.
- Re-run health before readiness.

Never relax permissions or bypass schema validation to make a check pass.

## Two-tier local helper configuration

The optional `child_broker` bounds shared local child/specialist endpoint
capacity. Waiting requests are FIFO within their effective priority lane;
background requests age deterministically through normal to high so a stream of
high-priority work cannot starve them.

`specialist.profile` must resolve to a configured local child profile (normally
`local-child-8082`); manual/cloud profiles are rejected. `verify_claims` uses
that specialist once at high priority and remains advisory/tool-free. Legacy
configs without named child profiles fail closed for evidence extraction,
verification, and reranking rather than silently using the main endpoint.
Memory reranking is disabled unless `memory.reranker.enabled` is explicitly
true; when enabled it is available only through the Application's explicit
evaluation method, uses the same resolved child specialist once at background
priority, and receives only parent-filtered opaque IDs plus bounded sanitized
excerpts.

For a single-slot local child server, a practical configuration is:

```json
{
  "child_broker": {
    "enabled": true,
    "max_concurrency": 1,
    "max_queue": 32
  },
  "specialist": {
    "enabled": true,
    "profile": "local-child-8082",
    "max_tokens": 4096,
    "evidence_enabled": true,
    "evidence_timeout_seconds": 60
  },
  "memory": {
    "reranker": {
      "enabled": true,
      "max_candidates": 4,
      "max_excerpt_bytes": 2048
    }
  }
}
```

The broker concurrency should match the actual provider's parallel slot count;
it is endpoint capacity, not a desired amount of agent parallelism.

## Unknown fields and version skew

Bee ignores config fields it does not know, with a warning, so an older
binary still starts on a config written for a newer one (a rollback needs
only the old binary, not the old config too). The warning is printed on
stderr at startup (`bee`, `bee --serve`, `bee jobs run-due`, `bee-telegram`;
for the services it lands in the journal), by `bee config validate`
(`ignored_fields` with `--json`), and as the `config_unknown_fields` check in
`bee doctor`, which fails under `--strict`.

Typos still fail. A field that closely matches a known field at the same
level fails to load with a "did you mean". Close means the same name
ignoring case, underscores and hyphens (`requirePlan`), or one or two edits
(`requre_plan`, `agnet`). Keys match the way Go's JSON decoder matches
them, so a key that differs from a known field only in case is not unknown.

To get the old behavior, where any unknown field is an error, set
`"strict_config": true` at the top level of the config, or
`BEE_STRICT_CONFIG=1` in the environment.

The Telegram adapter's own config (`telegram-adapter.json`) is still
strict.
