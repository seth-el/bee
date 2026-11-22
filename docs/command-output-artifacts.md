# Retained command-output artifacts

Source-only implementation note for September 25, 2026. This is not a deployment or live-acceptance claim.

Foreground synchronous `exec` and compatibility `run_command` share one retention contract. PTY/background terminal transcripts remain non-durable and are intentionally out of scope.

## Contract

- Ordinary command output remains bounded by the existing execution-scope output limit.
- When that bounded result is truncated and a durable session/run context plus `ArtifactStore` exists, Bee retains a private immutable `command-output.json` artifact.
- The payload is deterministic JSON with separate stdout/stderr fields encoded as base64. This preserves stream boundaries and arbitrary/non-UTF-8 bytes without invalid JSON.
- Retention is capped at 4 MiB of raw command bytes, with a 2 MiB prefix bound per stream. The ordinary result reports `artifact_id`, retained artifact size, retained raw output bytes, whether retention itself was capped, original truncation, and the safe `artifact_read` continuation action.
- A retention/store failure does not erase, fail, or falsely untruncate the command result. It returns a bounded generic unavailable diagnostic and never exposes storage paths.

## `artifact_read`

`artifact_read` is read-only and registered only in the ordinary parent composition when artifact storage is configured. It requires the current durable session from trusted tool context, rejects cross-session or guessed IDs, accepts `offset >= 0` and `1 <= limit <= 64 KiB`, and returns deterministic `offset`, `returned_bytes`, `next_offset`, `eof`, retained/original truncation metadata, and base64 page data. It never reruns the command and does not expose filesystem paths.

Locked, child, specialist, scout, extension, and process-child compositions do not inherit the capability. Child command execution without a durable parent context keeps its prior bounded behavior and does not fail because retention is unavailable.

Artifact creation uses the existing session-bound `ArtifactStore`, event, emitter, and index infrastructure. Command artifact payloads are immutable and remain private to the owning session.
