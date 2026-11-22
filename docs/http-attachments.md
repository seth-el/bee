# HTTP attachment serving contract

## Inventory and decision

Before this work, `/v1/chat/completions` decoded a bounded 1 MiB JSON envelope
and accepted message content as a string or an array containing only `text` or
`input_text` parts. It did not accept browser-supplied image parts. Bee already
had the durable machinery needed after ingress: `AttachmentStore` keeps private
artifact bytes and metadata, `RunRequest.AttachmentIDs` resolves session-owned
references, durable `Message` records keep `AttachmentRef` metadata only, and
the OpenAI-compatible provider converts image references to standard
`image_url` data-URI parts after resolving the private artifact. The configured
the main profile explicitly declares `image_input: true`; the provider
allowlist is PNG, JPEG, GIF, and WebP with at most four images per call.

The selected browser contract is therefore authenticated multipart upload plus
opaque references, not inline base64 and not remote URLs. The exact routes and
limits are documented in [OpenAI-compatible HTTP serving](openai-serving.md).
This adapter reuses the single attachment-aware agent/provider path.

## Concurrency and history

Upload verifies the existing external-conversation-to-durable-session mapping
and briefly acquires the session turn lease. A busy session returns `409`; no
run is cancelled or modified. Chat validates each reference again after route
resolution. Original content is never logged or serialized into session
events.

User messages retain attachment references in durable history. The
history projection returns only safe metadata and authenticated preview paths.
Preview generation is dimension-, pixel-, and byte-bounded; a client should load
only a bounded recent subset and keep filename indicators when a preview
fails.

## Manual verification

1. Upload a PNG/JPEG/GIF/WebP under 8 MiB with a valid bearer token and
   `X-Session-Id`, and verify the response carries an attachment reference.
2. Send a chat completion that names the reference, send, and verify the sent message keeps its image indicator and
   the provider responds to the image content.
4. Select a wrong type, an image over 8 MiB, and a file whose bytes do not
   match its declared image type; verify clear structured errors.
5. Fetch the session history; verify the durable user message shows its
   attachment metadata and bounded preview path.
6. Keep the target session busy elsewhere and attempt an upload; verify `409`
   and no cancellation.
