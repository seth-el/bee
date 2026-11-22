# Provider or channel failure

Run local checks first:

```sh
bee health --ops-config /private/bee/ops.json --json
bee readiness --ops-config /private/bee/ops.json --json
```

Use `--check-provider` or `--check-channel` only when an external check is intended. Inspect bounded diagnostics and job/attempt state. Retry through the existing bounded foreground policy; do not start a second poller or remove a live state lock. If a job is stale, use the explicit jobs recovery runbook.

## Model server unreachable

`bee doctor` probes the main, child and fallback model servers with a 10 s
`GET /health` (`provider_reachability`; manual/cloud profiles are skipped and
no completion is sent). `bee readiness --check-provider [--config PATH]` runs
the same probe. `BEE_DOCTOR_SKIP_PROVIDER_PROBE=1` keeps doctor offline.

During a call:

- Connection setup is bounded at 15 s. Refused, timed-out, unreachable-host
  and DNS failures report `Model server unreachable: HOST:PORT (reason)`.
- A call that has produced nothing for 60 s triggers a health check. Any HTTP
  answer means the server is alive, perhaps reading a long prompt, and the
  call keeps waiting. Two failed checks 15 s apart end the call as
  unreachable, about 90 s after the last output.

Every surface says so: the TUI (error line plus status notice), the CLI,
Telegram ("The model server isn't reachable right now"), the HTTP run
lifecycle (`state: failed`, `reason: model_unreachable`), and job failure
reasons in `bee inbox` and pushes.

### Fallback

`routing.fallback_main_profile` names a profile that answers interactive
turns when the main server is unreachable before any reply. After a
fallback, Bee keeps using it for 2 minutes before trying the main server
again. Each run that falls back emits one `provider_fallback` event (from,
to, reason), journals it in the session, and says so in the TUI, in Telegram
(a note under the reply) and in the HTTP run lifecycle (`fallback_profile`).
It never applies to:

- a failure after output started;
- an explicit `--profile`;
- a turn with images when the fallback cannot read images;
- recurring or one-shot jobs, which fail and notify instead.

The fallback has its own context window. A conversation longer than that
window fails on the fallback with the server's error.
