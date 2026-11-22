# Channel contracts and routing

Phase 8A is the provider-neutral channel slice. It adds contracts and tests without implementing a live Telegram or voice adapter.

## Normalized envelopes

`InboundMessage` contains safe channel/account/conversation/thread identifiers, sender and message/update identities, bounded text, optional `AttachmentRef` values, optional `VoiceInput` references, reply metadata, and bounded non-secret metadata. `OutboundMessage` carries the same route identity plus bounded assistant text, optional attachment/voice references, reply-to identity, and output metadata.

Validation is fail-closed:

- IDs accept only bounded alphanumeric/`._:-` components and never represent filesystem paths;
- text, metadata entries/bytes, attachment count/size, filenames, content types, and references are bounded;
- attachment filenames must be safe basenames, content types are simple media types, and references cannot be local paths or credential-bearing URLs;
- metadata keys and secret-shaped values such as bearer credentials, API keys, and token assignments are rejected;
- `VoiceInput`/`VoiceOutput` are references only; no audio bytes or provider calls enter Bee.

## Session routing

`NewSessionRouteKey` deterministically encodes channel, account, conversation, and optional thread identity. `ChannelRouter` requires an application session store so routes cannot silently fall back to the process-global agent. It serializes work per route, maps each route to one durable `SessionID`, and allows independent conversations to retain separate sessions.

By default, route bindings and deduplication remain process-local for compatibility. An adapter or caller that needs restart recovery can opt into `ChannelRouterOptions.RouteBindings` with a `RouteBindingStore`; `LocalRouteBindingStore` persists the validated `SessionRouteKey` to `SessionID` mapping in a caller-selected 0600 JSON file. The router loads a prior binding on first use and persists the first `RunSession` binding, releasing the message claim if persistence fails. This is a durable mapping seam, not a delivery checkpoint or queue.

The local JSON implementation is synchronized within one store instance and commits each update with a temp-file write and rename. It does not coordinate multiple processes or merge concurrent writers, and the adapter still owns update checkpoint/ack ordering after a successful Bee run and every outbound send. Use an external store/lock when several workers share a route-binding file.

The router applies a parent-owned `ChannelAuthorizer` before model execution and an atomic `MessageDeduplicator` before session lookup. The default `MemoryMessageDeduplicator` is bounded and uses channel/account/conversation/update identity when an update identity is present, otherwise channel/account/conversation/message identity. Route state is bounded as well; once the configured route capacity is reached, new routes fail closed rather than growing without limit. A `ChannelPolicy` provides bounded sender and conversation allowlists plus message limits.

The router consumes the existing `Application.Run`/`RunEvent` stream and sends one bounded `OutboundMessage` through the registered channel's narrow `Send` seam. It preserves account, conversation, thread, and reply-to message identity. No channel credential, polling handle, webhook server, or provider SDK is passed to `Application`, `Agent`, or child processes.

`InMemoryChannel` is a standard-library fake for tests. Tests cover route isolation, duplicate inbound IDs, unauthorized senders, reply/thread preservation, bounded output metadata, and no cross-conversation session reuse.

## Explicit deferrals

This phase does **not** implement:

- Telegram API polling, webhooks, bot tokens, or live Telegram transport;
- attachment download, local file materialization, or arbitrary filesystem paths;
- live media/voice handling, including provider calls, STT/TTS, conversion, and upload;
- retry/recovery workflows, durable delivery claims, background queues, workers, or delivery scheduling;
- CLI/channel wiring beyond testable contracts and seams;
- Phase 9 scheduling, triggers, recurring jobs, or automation.

Those concerns require separate parent-owned boundaries, operational policy, and tests before they are added.
