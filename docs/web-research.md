# Web search and fetch

For the bounded local-child research mode, see [web-research-scout.md](web-research-scout.md). It keeps raw search/browser tools internal and exposes compact `web_research` plus bounded known-URL `web_fetch` to the foreground assistant.

Bee's web research capability is separate from shell/SSH execution. It is
read-only, transient, and explicitly enabled in config.

## Configuration

The configured local SearXNG endpoint is:

```json
{
  "web": {
    "enabled": true,
    "search_endpoint": "https://searxng.example.com/search",
    "timeout_seconds": 15,
    "max_results": 10
  }
}
```

`web_search` and `web_fetch` are added to ordinary chat only when `web.enabled`
is true. They are not added to the coding registry or children by default.

## Optional browser rendering

The `web_browser` tool is an explicit opt-in for JavaScript-heavy
application shells and interactive internal services that static HTTP fetch
Camofox control service; `snapshot` is observational, while `open`, `navigate`,
`click`, and `press` can mutate browser state. It uses the separately configured,
owner-authenticated Camofox control service; Bee does not parse OpenClaw config or guess tokens.

```json
{
  "web": {
    "enabled": true,
    "browser": {
      "enabled": true,
      "camofox_url": "http://127.0.0.1:9377",
      "timeout_seconds": 15
    }
  }
}
```

Bee exposes two browser tools: read-only `web_browser` (`open`, `navigate`,
`snapshot`, `scroll`, `wait`, `close`, `status`) and mutating
`web_browser_act` (`click`, `type`, `press`, `hover`). The latter requires a
valid `bee_plan`; the read-only tool does not. Bee binds the current tab
internally to the durable Bee session, never accepts or returns `target_id`,
and isolates Camofox identities with a stable non-secret hash. Opening a new
page closes the previous binding best-effort; bindings are bounded and expire.
All browser output is a finite Bee-owned envelope marked `untrusted: true`; snapshot text appears once in canonical `content`, and Camofox owns snapshot windowing/pagination.

Camofox may be loopback without authentication. A non-loopback endpoint
requires an owner-only `access_key_file` (private regular file), HTTPS, and its
contents are sent as `Authorization: Bearer <key>`. Control-plane redirects
are not followed and responses are bounded. Expired Bee bindings are left to
Camofox's own tab/session reaper; Bee's explicit close and replacement paths
still close best-effort. HTTP `web_fetch` remains the static
fallback, and downloads are intentionally deferred until a safe artifact route
is available.

Private and link-local HTTP(S) destinations are allowed by default so a
configured internal search result such as `gitlab.example.com` resolving to
`203.0.113.220` remains fetchable. To opt out, set
`web.allow_private_targets` to `false`:

```json
{
  "web": {
    "enabled": true,
    "allow_private_targets": false
  }
}
```

The setting applies to direct fetches, redirect destinations, and URLs in
search results. Public-only fetches resolve and validate immediately before a
direct IP-pinned dial; proxy resolution is disabled so DNS rebinding cannot
silently redirect the request into a private network. An omitted setting means
allowed; an explicit `false` means
blocked.

## Optional media search

Media search is a separate opt-in capability. Set `web.media_search_enabled` to
`true` while keeping the same configured `search_endpoint`; it registers the
read-only `media_search` tool in the ordinary assistant only. The model-facing
tool result is a bounded, model-safe description containing rank/title/snippet/
source/duration/resolution only, plus an explicit notice that media was not
downloaded. A named/ranked follow-up may use `media_retrieve`; otherwise the
user can choose a candidate in the UI. It never exposes
candidate IDs, selection tokens, or retrieval URLs to the provider.

The web gateway receives a separate provider-neutral `media_search` run event
with the full UI-only selection bindings and retrieval metadata. For an
original prompt with explicit display/attachment intent (for example, “show
the bee image” or “attach a video clip”), the gateway marks the turn
`AutoRetrieveTop`. If `media_search` runs in that turn, only rank 1 from that
same session/run-bound result is consumed and passed through the existing
bounded `MediaService` and `ArtifactStore` path. Successful retrieval emits
the normal `media_progress` and `artifact` events; the artifact card is the
display and the chooser is suppressed. If rank 1 fails, its binding is still
consumed, the model receives a bounded failure notice, and the remaining
candidates stay available for explicit selection.

A later conversational follow-up in the same durable session can select one
prior result through the provider-neutral `media_retrieve` tool. The model
passes only a rank from 1 through 12 (for example, rank 3 for a named result);
Bee resolves that rank against the most recent session-bound `media_search`
batch, then enforces expiry, session binding, and single use before invoking
the same media/artifact pipeline. The tool returns bounded artifact metadata
and status only. It never accepts or returns a URL, token, candidate ID,
browser field, source HTML, or filesystem path. This one capability has a
narrow plan-gate exception because it only presents one immutable artifact
from parent-owned state; generic mutating tools remain plan-gated.

Broad “find/search/options/list” requests remain chooser mode and do not
auto-retrieve. The classifier is web-adapter-owned; Telegram and other
adapters do not set this metadata. No raw retrieval URL, browser token,
candidate binding, or artifact filesystem path reaches the model. The browser
renders ordinary bindings as text-only selection cards; it must not derive
cards by parsing `tool_result`, emit Markdown image links, or load remote
thumbnails.

Chooser-mode retrieval requires an explicit user selection: `POST
/api/sessions/{session_id}/media/select` with only `candidate_id` and `token`. Auto-attach turns do not use this endpoint for rank 1; the endpoint remains available for the remaining candidates and all broad-search turns.
The server verifies the durable session, expiry, single-use binding, and public
HTTP(S) retrieval URL, then starts the normal `/image` or `/video` command so
the existing media progress, artifact, failure, run-end, and SSE contracts
remain authoritative. The browser renders text-only cards and does not load
remote thumbnails. Candidate metadata and source URLs are untrusted
attribution data, not instructions. Telegram, child executors, and coding
compositions do not register `media_search` or `media_retrieve`.

Video search candidates with a known duration over three hours are filtered
before ranking or binding; candidates with unknown duration remain available for
the bounded retrieval validation step. Video retrieval asks `yt-dlp` for
bounded low-resolution video and audio streams under the dedicated 1 GiB
per-stream/final-artifact media cap, then retains a 2 GiB private temporary
work-directory bound. Video output is ingested through a streaming file-backed
artifact path, so a large MP4 is never loaded into RAM. Ordinary artifacts and
image retrieval remain capped at 8 MiB. Tool failures include a short sanitized
excerpt of the command output so format-selection errors remain actionable
without exposing paths or credentials.

Automatic retrieval is one-shot per assistant run. After the first explicit
attachment attempt, repeated `media_search` calls return bounded
`already_attached`/`no_additional_retrieval` state and do not create another
artifact or chooser card. Broad searches remain chooser mode, and explicit
`/video` and `/image` commands retain their independent behavior.

## Tools

`web_search` accepts a bounded query, optional result count, and language. It
returns a non-null `results` array plus explicit `degraded`, `retryable`,
`note`, and sanitized `unresponsive_engines` fields. A zero-result response
with engine failures is non-retryable and trips a bounded per-run circuit, so
changed-query loops do not hammer an unavailable SearXNG backend.

`web_fetch` accepts one HTTP(S) URL plus optional bounded rune-based `offset`
and `max_chars`. Transport is capped at 4 MiB; readable output defaults to a
64 KiB window and is capped at 128 KiB. HTML extraction removes head, scripts,
styles, nav, and noscript content and decodes entities. Results include
pagination metadata (`total_chars`, `has_more`, `next_offset`, `truncated`);
continue with the returned offset and do not repeat unchanged arguments.
Bee currently normalizes invalid/non-UTF-8 payloads through Go string decoding;
it does not claim charset transcoding for legacy encodings.

GitLab-aware fetch is a read-only fallback for GitLab project and blob URLs
whose first response is only the JavaScript application shell. JavaScript is
never executed. For project pages Bee derives the project path from the URL,
uses bounded GitLab REST calls to resolve metadata and a recursive tree, then
fetches a README through the raw-file endpoint when the tree contains one. For
blob pages Bee derives the ref and repository path from the URL and fetches the
corresponding raw file. Repository paths are validated and are never accepted
from model-generated follow-up arguments. Readable README/blob responses are
returned in full; metadata/tree calls retain their structural safety bounds.
Authentication is not invented or forwarded, so an unauthorized GitLab
response is reported as an explicit fetch error.

All results are marked `untrusted: true`. External text is reference data, not
Bee instructions, policy, memory, or approval. Bee never executes fetched text
or stores it as durable memory automatically.

## Network safety

- only `http`/`https` URLs;
- no URL userinfo, cookies, or authorization forwarding;
- bounded timeout, result count, and URL/query sizes;
- redirect count limited to five;
- redirect destinations are revalidated and follow `allow_private_targets`;
- direct fetch and search-result URLs follow `allow_private_targets`, which
  allows private/link-local/multicast targets by default and can explicitly
  block them;
- the explicitly configured SearXNG search endpoint may be private/local;
- no file upload, local-file scheme, shell, or provider call.

The local SearXNG endpoint was live-accepted on August 20, 2026: a game-server
search returned three results and Bee fetched the first public result into
untrusted HTML text.
