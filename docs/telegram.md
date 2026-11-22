# Telegram adapter

`adapters/telegram` is a standard-library-only transport boundary. Telegram wire structs, token handling, HTTP calls, polling, ID conversion, retries, checkpoints, media staging, and provider HTTP calls remain adapter-owned; Bee sees only normalized channel envelopes and opaque voice references.

## Text configuration

The adapter config is a separate JSON document containing:

- stable `channel_id` and `account_id`;
- expected bot ID and exact username, verified with `getMe`;
- a private `token_file` (existing files must be mode `0600`);
- absolute HTTP(S) `api_base_url`;
- bounded polling timeout/limit;
- caller-selected checkpoint and dedicated session roots;
- exact non-empty raw chat and sender allowlists.

The token is read only at composition time and is never copied into Bee config, normalized metadata, errors, fixtures, or logs.

## Slash commands

Telegram runs slash commands through the same core command handling as the
TUI. The bot publishes its "/" menu at startup (`setMyCommands`; if that
fails, commands still work). Supported: `/status`, `/new`, `/resume`
(lists conversations; `/resume <id or prefix>` switches this chat to one),
`/continue`, `/inbox` (`/inbox ack`), `/plan`, `/tasks`, `/compact`,
`/tool_limit` (Telegram's spelling of `/tool-limit`), `/subagents`, and a
Telegram-specific `/help`. `/cmd@botname` works too.

`/stop` (also `/kill`, `/cancel`) and `/steer <correction>` act immediately,
even while a reply is running: the poller keeps receiving updates during a
reply, queues ordinary messages to run in order after it, and handles these
interrupts at once. `/stop` cancels the running reply in that chat ("Stopped.",
or "Nothing is running."). `/steer` cancels it and queues the correction as
the next message. The durable checkpoint only advances past updates that have
finished, so a crash replays anything unfinished. While a reply runs, the chat
shows "typing…".

Not available, with a short reply saying so: `/tools` and `/thinking` (TUI
display settings) and `/video`/`/image` (web gateway media). Without voice
enabled, `/speak` and `/say` say so too.

Failed turns are answered in the chat ("Bee couldn't answer that: ... Send it
again to retry.") and the message is consumed, instead of the adapter exiting
and retrying the same update forever. A conversation that is busy in the TUI
or another client gets a "busy, send it again" reply.

## Images

Photos, and images sent as files, reach the model the same way HTTP uploads do:
the adapter downloads them into a private staging area
(`<bee state>/telegram-attachments`, cleaned after each message), the router
validates each one with the same checks as HTTP uploads (PNG, JPEG, GIF or WebP;
type, decoding, and dimension limits), stores it in the conversation's
attachment store, and passes it to the model with the message. A caption is
the message text; a photo alone is sent as "(sent an image)". The images then
show in the conversation's history in other clients too.

Files that are not usable images (PDFs, HEIC, oversized pictures) are not
passed on; the prompt names them, so the assistant can say what it didn't receive.
Images need a model profile with `image_input`.

## Voice

Add `"voice": {"enabled": true}` to the adapter config. Voice needs an
OpenAI-compatible transcription endpoint and a speech endpoint, set in the
`voice` block: `transcribe_url`, `speech_url` and `voice_id` are required;
`instruction` and `cfg_scale` set the speaking style and have defaults:

- **Voice notes in:** the note is transcribed by faster-whisper, the
  transcript is echoed back ("🎙 …") so a mishearing is obvious, and the text
  goes to the model like a typed message.
- **Voice out:** replies to voice notes, and to `/speak <prompt>`, arrive as text
  and then as a voice note. Code blocks are skipped, links and markdown
  flattened, and the whole reply is spoken. Long replies are synthesized in
  pieces of up to 1500 characters and joined into one voice note.
  `max_spoken_chars` (default and maximum 10000, about 11 minutes) caps it;
  past that the note ends with "The rest is in the text."
  `reply_with_voice` is `voice_notes` (default), `always`, or `never`.
- **`/say <text>`** reads text aloud without asking the
  model.
- A failed transcription is answered in the chat; a failed voice reply is
  logged and the text reply still arrives. TTS "busy" responses are retried
  a few times. ffmpeg must be on the adapter's PATH (it converts TTS PCM to
  Telegram's OGG/Opus).
- Audio is staged in `<bee state>/telegram-voice` (`voice.artifact_root`)
  and removed after each note is transcribed or sent.

## Shared sessions

Point `session_root` at Bee's main session store (in the main workspace,
`~/.bee/state/sessions`, the store the TUI and `bee --serve` use) and
set `route_bindings_path` to a file outside it (e.g. next to the
checkpoint). Telegram conversations then show up in the TUI's resume list
and in other clients' session lists. A new one starts as "Telegram · <date>"; after
the first reply the date is replaced by a short subject from the child model
("Telegram · Disk usage on beta"), and stays if that call fails. Any TUI or
HTTP conversation can be continued from Telegram with `/resume <id>`. Turn
leases keep two clients from running in one session at the same time.
`route_bindings_path` defaults to `telegram-route-bindings.json` inside
`session_root`.

## Inbox notification push

Set `notify_chat_id` (one of `allowed_chats`) to receive a message whenever a
job writes an inbox notification. By default only `paused` and `failed` are
pushed, since those need the owner; `notify_states` overrides the list (for
example `["paused", "failed", "succeeded"]`). Without `notify_chat_id`, push
is off.

- The adapter checks the inbox every 30 seconds, next to polling.
- Messages carry the state, job name and ID, and time, and point at
  `bee inbox`. The job's error text stays local; it is not sent to Telegram.
- Pushing never marks an item read. Ack it with `bee inbox ack`, `/inbox ack`,
  or `POST /api/notifications/{id}/read`.
- Delivery is at-least-once. A per-chat ledger next to the inbox
  (`notification-push-telegram-<chat>.json`, `0600`) records what was sent, so
  a restart does not repeat messages; a crash between a send and the ledger
  write can repeat one.
- Items that are already read, or more than 24 hours old when first seen, are
  skipped, so turning push on does not replay the backlog.
- A failed send is retried on later checks, up to 5 attempts. Failures are
  logged as counts without message content and never stop the adapter.

## Explicit voice path

The default `cmd/bee-telegram` constructor remains text-only. A caller that opts into voice composes `NewVoiceAwareTransport` with the existing `bee.ChannelRouter` and supplies:

- a private `VoiceArtifactStore` input root for Telegram downloads;
- `GemmaArtifactRecognizer` endpoint/model settings (and optional API key);
- a private `VoiceOutputArtifactStore` root;
- `TTSSynthesizer` endpoint, `ref_audio` file, `ref_text`, instruction,
  cfg scale, seed, and the shared TTS lock path;
- an explicit text-fallback policy.

This path does not create another Bee runtime. Gemma uses ffmpeg to normalize audio to 16 kHz mono WAV, then sends bounded OpenAI-compatible JSON with base64 `input_audio`, zero temperature, bounded tokens, and disabled thinking. The TTS server receives bounded multipart form data (`ref_audio`, `ref_text`, `text`, `instruction`, `cfg_scale`, and `seed`) and its 24 kHz mono signed-16 PCM response is converted with ffmpeg to WAV and OGG/Opus. A stale-safe bounded mkdir lock at a shared path serializes TTS requests, with bounded HTTP 409 retry/backoff. Provider output references are opaque (`tg-voice-output-v1-...`) and resolve only through the adapter-owned private store. The legacy `OmniVoiceSynthesizer` remains available only for explicit compatibility composition.

Telegram output voice uses bounded multipart `sendVoice`, preserves thread/reply IDs, retries transient failures, and cleans output artifacts after send. If synthesis or voice delivery fails, the caller may enable text fallback. Without explicit voice composition, voice-only input remains ignored by the normal text poller and outbound text remains unchanged.

## Delivery semantics and recovery

Telegram processing is deliberately **at least once**, not exactly once. The
poller advances its durable checkpoint only after normalized handling returns
successfully. If handling fails, the checkpoint remains retryable; if the
process crashes after a handler or outbound send succeeds but before the
checkpoint write is durable, the update may be delivered again after restart.

The channel router claims processable inbound identities before text or
attachment dispatch. Duplicate attachment-only deliveries are suppressed
within the running process, and failed attachment handling releases the claim
so Telegram can retry it. The default deduplicator is bounded and in-memory;
it is not a durable cross-restart idempotency ledger. Therefore duplicate
side effects after a crash/restart are an expected bounded consequence of the
never-lose-the-update policy. Exactly-once sends would require a durable
outbox/transaction boundary and are outside the current foreground scope.

During a quiet observation window, treat an explained duplicate after a
crash/restart as an expected recovery symptom, not an automatic rollback
trigger. Roll back for lost updates, checkpoint advancement after handler
failure, unauthorized acknowledgements, identity/allowlist violations,
route/session ownership changes, or unexplained repeated side effects.

## Recovery flags and observation rules

- `--inspect-pending` is read-only: it reports pending update IDs without
  acknowledging them.
- `--skip-pending` is destructive: it advances the checkpoint past currently
  pending updates without routing them. Use it only when the operator
  explicitly accepts dropping those updates during deliberate recovery. Never
  use it during live smoke testing, quiet observation, or ownership transfer.

Use a separate test bot, exact allowlists, fresh private checkpoint/session
roots, and recorded rollback criteria for live acceptance. Do not manually
advance checkpoints to hide a duplicate during observation.

## Safety and tests

All provider and Telegram HTTP calls are context-aware and bounded. Errors do not expose bot tokens, API keys, local paths, multipart contents, or provider response descriptions. Reference/audio/output files are owner-only and cleaned on success, failure, and cancellation.

Offline tests use `httptest.Server` and controlled ffmpeg seams for validation, request contracts, size limits, cancellation, artifact lifecycle, output multipart sending, fallback behavior, retries, deduplication, and race safety. Controlled live Telegram text acceptance passed on August 18, 2026 using a separate test bot and isolated state roots; no production-owner transfer occurred. Voice remains an explicit opt-in path and still requires its own live smoke acceptance before use.
