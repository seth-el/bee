# Optional specialist helpers

Bee can compose an optional parent-owned `SpecialistService` for bounded helper
work. Configure it under `specialist` in the JSON config. The service resolves
an explicitly named local provider profile, or the configured child profile when
`profile` is omitted. Manual/cloud profiles are rejected.

Safe defaults are enabled, two concurrent calls, a 3-second timeout, bounded
input/output/token budgets, no tools, and disabled thinking. Helper failures are
fail-open: the original user turn, opaque attachment references, durable tool
results, and `run_failure`/`run_end` lifecycle events remain authoritative.

Helpers include image interpretation when the main provider has no declared
image capability, explicit image-sidecar routing when
`specialist.image_sidecar_enabled` is true, oversized observation compression,
repeated tool-failure diagnosis, and optional transient session digest/title
metadata. Session digest/title extraction is disabled by default; set
`session_digest_enabled` to `true` to opt in. `SummarizeTranscript` is an
explicit bounded seam for callers that already have a ready transcript; it is
not automatically invoked by ordinary attachment turns. The dedicated Whisper
worker remains the audio-transcription path, so turns do not duplicate
transcription or wait on an optional model summary. Image/audio output is never
stored as a replacement for the original attachment metadata. Observation
compression is applied only to the provider-facing copy; durable session state
remains lossless. Structured retrieval results from `memory_search`,
`memory_show`, and `conversation_search`, plus observations from the
read-only web tools `web_fetch`, `web_browser`, and `web_search`, bypass
local-child observation compression entirely: the main model receives their tool
output directly, byte-for-byte. Prose-like observations from `run_command`,
`read_file`, and log-style tools use thresholded deterministic compression in
automatic historical request projection, making the same durable bytes produce
the same prefix after process restart. Explicit `CompressObservation` callers
may still opt into bounded model compression.
Digest/title output is emitted as `session_digest` metadata and does not mutate
the durable session title.

Example:

```json
{
  "specialist": {
    "enabled": true,
    "profile": "local-child-8082",
    "timeout_seconds": 3,
    "observation_compression_timeout_seconds": 10,
    "max_input_bytes": 16384,
    "max_output_bytes": 8192,
    "max_tokens": 4096,
    "max_concurrency": 2,
    "observation_threshold_bytes": 8192,
    "observation_max_bytes": 4096,
    "failure_threshold": 3,
    "failure_window_seconds": 60,
    "image_sidecar_enabled": false,
    "image_max_bytes": 104857600,
    "image_max_dimension": 1536,
    "image_max_pixels": 1500000,
    "session_digest_enabled": false,
    "evidence_enabled": true,
    "evidence_timeout_seconds": 60
  }
}
```

### Local endpoints on the LAN

The specialist, memory automation and the web scout run only on a local
provider. Loopback endpoints (`127.0.0.1`, `localhost`, `::1`) are local
automatically. A model server elsewhere on your own network is not, unless
its provider declares `"local": true`:

```json
{"name": "local-child-8083", "role": "child",
 "provider": {"base_url": "http://203.0.113.37:8083", "model": "gemma", "local": true}}
```

The flag is owner config: Bee never infers it from the address, so a
private-range address you didn't mark stays treated as remote.

### Switches

- `observation_compression` (default `true`): trims tool observations over
  `observation_threshold_bytes` in the provider-facing request. The trim is
  deterministic, so it keeps the prompt cache stable, but it drops detail.
  Set `false` to send observations in full and leave context pressure to
  compaction.
- `image_fallback` (default `true`): when the main provider has no image
  input, the child describes image attachments for it. This needs a child
  profile with image input; set `false` to turn it off.
  `image_sidecar_enabled` is the separate path that forces every image
  through the child even when the main model can see.

Raw specialist prompts/results are not appended to session history or emitted
as logs; only bounded derived metadata may be visible in the run stream.

## Explicit image sidecar routing

Set `image_sidecar_enabled` to `true` to force every image attachment through
the configured local child specialist (normally `local-child-8082`) before the
main turn. The selected child profile must be a configured local child and its
provider must explicitly declare `"image_input": true`; the 8081 loopback
multimodal compatibility default is not accepted for this path. Invalid
composition fails during config validation/startup.

The specialist receives the opaque attachment reference through
`InterpretImage` and returns bounded `{summary,claims}` JSON as transient,
explicitly untrusted observation context. Image references remain authoritative in durable session/attachment
metadata, but are removed from the main provider-facing request, including
when the specialist fails. A failed sidecar adds only a concise bounded note
and the text turn continues; image bytes are never sent to the main provider
as a fallback. Text, transcript, and other non-image attachment references
retain their existing behavior.

The child-facing image payload is bounded independently from the durable
attachment. `image_max_bytes` defaults to 100 MiB and rejects an original image
when its payload exceeds the configured limit (the `max+1` sentinel is never
forwarded). `image_max_dimension` defaults to 1536 and `image_max_pixels`
defaults to 1.5 megapixels (configurable up to 4 megapixels); proportional
scaling satisfies both limits. Source images are capped at 25 megapixels before
full decode, and preprocessing is serialized through one bounded worker.
JPEG, PNG, GIF, and WebP are recognized. JPEG EXIF orientations are normalized
before resizing/encoding, while an ordinary JPEG with no required normalization
is forwarded byte-for-byte. JPEG remains JPEG; resized non-JPEG images are
encoded as PNG. Static GIFs and WebP are converted to PNG when resized;
animated GIF/WebP resizing is rejected rather than silently dropping frames.
Unsupported or malformed image formats return a bounded sidecar error; the
main provider still receives the text turn without image bytes. These changes
affect only the transient child request; the original artifact, attachment
reference, and durable session record remain unchanged.

## Phase A child capacity and evidence seams

Specialist, recon-review, child-runner, and child-backed post-turn memory
provider calls may share the parent-owned `child_broker` capacity broker. It
bounds endpoint concurrency, queues by purpose/priority, honors parent
cancellation while queued, and exposes non-secret runtime counters. The broker
is capacity only: the parent still owns tools, approvals, mutations, final
answers, durable writes, and recursive orchestration. Broker-aware operations
must not acquire reentrantly.

Explicit specialist observation compression first applies deterministic
duplicate/boilerplate preprocessing while retaining errors, exit codes,
identifiers, and head/tail context. Model compression runs only if the
preprocessed observation remains over threshold; deterministic fallback remains
available. Automatic replay projection always uses the deterministic path so it
cannot rewrite a historical provider prefix. Lossless and structured
observation tools bypass compression.

When `specialist.evidence_enabled` is true, `EvidenceExtractor` provides a
bounded, tool-free JSON evidence packet with exact supplied anchors,
uncertainties, errors, and verification targets. Its output is descriptive and
non-authoritative. Large eligible prose observations may use it after deterministic
compaction; short turns and lossless structured tools do not add a child call.
`delegate_recon` also uses this seam after the parent deterministically collects
only the requested bounded files, patterns, and grep matches.

`verify_claims` is a separate explicit tool. It sends one tool-free,
high-priority call through the shared broker to a configured local child
profile (normally 8082); legacy/no-profile main fallback is unavailable for
this feature. The request contains only bounded `draft`, `source_text`, and
evidence items whose anchors are validated as exact substrings of `source_text`
before dispatch. The strict response is advisory-only, contains no unknown
fields or trailing JSON, requires non-null arrays, and represents
contradiction/stale findings as exact-anchor records. It cannot invent claims or
introduce ungrounded paths, endpoints, URLs, or secret-like text.

Memory reranking is disabled by default and is not part of ordinary assistant
turns. When explicitly enabled through the Application evaluation method,
`MemoryReranker` calls the resolved configured local child specialist once at
background priority with only parent-filtered opaque IDs and sanitized excerpts.
It has no memory-store authority, requires a complete permutation of existing
IDs, rejects unsafe IDs and legacy/no-profile fallback, and bounds reasons.
`DeterministicMemoryRerankBaseline` is a separately named offline comparator,
not the enabled path.

Phase B changes evidence packets to strict anchored items: each item contains
`claim`, exact supplied `anchor`, `category` (`fact`, `inference`, or
`uncertainty`), and numeric `confidence` in `[0,1]`. Unknown fields, trailing
JSON, malformed values, oversized/secret-like text, and ungrounded paths or
URLs are rejected. If opt-in evidence extraction fails, large eligible prose
observations use deterministic signal-preserving fallback; ordinary short
turns and lossless structured tool outputs do not add a model call.
