# Provider and endpoint acceptance

Bee's provider routing is explicit and provider-neutral. The acceptance command
proves configured OpenAI-compatible endpoints without creating a session,
loading memory, using tools, or invoking Telegram/OpenClaw.

## Deterministic tests

`provider_acceptance_test.go` uses three local HTTP provider spies. The spies
record only bounded request metadata and verify:

- main, child, and manually selected profiles reach only their own endpoint;
- manual/cloud profiles are skipped without explicit opt-in;
- a provider error has zero fallback attempts and does not contact another
  profile;
- acceptance output does not contain authorization headers or secrets;
- nonce and probe duration limits are enforced.

The normal Go test suite never contacts 8081, 8082, or any cloud provider.

## Local endpoint probes

After building Bee, probe the configured main and child profiles:

```sh
bee providers accept --config "$HOME/.config/bee/config.json"
```

The default probe set is the configured main and child roles. Manual profiles
are listed as skipped unless explicitly enabled. The output is a bounded JSON
report with profile, role, model, endpoint host, response status class, timing,
and fallback count. It does not include prompts, response text, API keys, or
raw provider errors.

To select specific local profiles:

```sh
bee providers accept \
  --config "$HOME/.config/bee/config.json" \
  --profiles local-main-8081,local-child-8082 \
  --nonce bee-acceptance-20260819
```

Each probe sends one short, nonce-bearing request with at most 128 output
tokens and a maximum timeout of 60 seconds. There is no retry or fallback.

## Cloud profile probe

A real cloud request is never made by default. Profile selection and validation
can be tested without network access, but an actual cloud request requires an
explicit operator flag:

```sh
bee providers accept \
  --config "$HOME/.config/bee/config.json" \
  --profiles cloud \
  --allow-cloud-probe \
  --nonce bee-cloud-acceptance-01
```

This performs exactly one bounded request to the configured manual profile. It
may incur provider cost. The flag is intentionally required and is not used by
ordinary tests, startup, fallback handling, or uncertainty paths.

## Acceptance evidence

A successful report must show:

- `passed: true`;
- one response for each requested local profile;
- `fallback_attempts: 0`;
- no skipped profile that was expected to be probed;
- matching configured profile/role/model metadata;
- no credentials or raw prompt/response material in the report.

Endpoint acceptance checks that profiles answer correctly. It changes no
routing, channel, or memory configuration.
