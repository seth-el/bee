# Effective harness snapshots and completion manifests

Bee can inspect the composition it will actually run:

```sh
bee harness inspect
bee harness inspect --json
bee harness inspect --strict
```

The command resolves the selected provider profile, prompt composition, policy,
scope and write roots, plan mode, enabled skills/plugins, started extensions,
and the final tool registry. It does not call the model. Enabled extensions are
started so their advertised tools are included in the effective composition,
then stopped when inspection finishes.

The snapshot contains sizes, names, package checksums, and SHA-256 identities;
it does not contain system-prompt text, skill instructions, tool descriptions,
credentials, session history, retrieved memory, attachments, or per-turn remote
grants. Those dynamic per-turn inputs are listed as excluded. `captured_at` and
lint prose do not participate in the stable harness fingerprint.

`fingerprint` is the provenance identity and includes enforcement policy such
as whether a plan is required. `cache_prefix_fingerprint` identifies the model,
provider rendering family, effective chat-template settings, stable system
protocol, and provider-visible tool schemas. Local-only tool risk/mutability
metadata is excluded. Plan mode changes therefore remain distinct provenance
states without unnecessarily creating a different provider prompt-cache lane.

The composition linter reports structural faults such as duplicate tools,
missing skill/plugin tools, missing package checksums, initial context that
cannot fit after reserves, enabled-but-inactive extensions, an unusually large
always-on prompt, and an explicitly unlimited tool-step setting. Errors make
the command fail. `--strict` also makes warnings fail.

## Delegated completion manifests

Every local-child and ACP completion artifact directory receives
`completion.manifest.json` beside `completion.json`. The completion packet
carries a compact `completion_manifest` reference. The manifest binds:

- task and parent identity, task inputs, mutability, scope, budget, and
  acceptance criteria;
- the effective child/ACP harness, tool capabilities, provider/adapter, and
  plan requirement;
- terminal status and a normalized result fingerprint;
- SHA-256 digests and byte sizes for bounded regular-file artifacts.

The manifest stores hashes rather than task prompts, system prompts, tool
descriptions, credentials, or artifact contents. Symlinked, escaping, special,
oversized, and excessive artifact sets fail closed.

Mutable isolated runs additionally receive `integration.manifest.json` beside
`proposal.json`. It binds the integration proposal to the exact Git base commit
and attempt ID, hashes `workspace.diff`, and chains to the delegated completion
manifest through `upstream_fingerprint`. Bee still never applies or merges the
proposal automatically.

Library callers can load a manifest with `ReadCompletionManifest` and decide
whether output is reusable with `VerifyCompletionManifest`. Reuse succeeds only
when the requested task, harness, capabilities, provider, base commit, and/or
result expectations match and every recorded artifact remains byte-identical.
