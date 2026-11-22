# Bounded web-research scout

Bee can optionally expose one parent-facing `web_research` tool instead of the
raw web tools. Enable it only after configuring `web.enabled` and a local child
provider profile (the default is `local-child-8082`):

```json
{
  "web": {
    "enabled": true,
    "scout": {
      "enabled": true,
      "timeout_seconds": 240,
      "max_steps": 10,
      "max_output_bytes": 12288,
      "max_tokens": 2048,
      "max_sources": 10,
      "max_concurrency": 2,
      "disable_thinking": true
    }
  }
}
```

When enabled, the ordinary foreground assistant sees `web_research` plus the
existing bounded `web_fetch`; it does not see `web_search`, `web_browser`, or
`web_browser_act`. Internal compatibility paths retain the
raw read-only web executor. Media search/retrieve are unaffected. Locked mode
exposes no web scout.

Each call starts a fresh session-free local-child execution in two phases. Phase
A is a bounded collector Agent with exactly `web_search`, `web_fetch`, and
read-only `web_browser`; it never receives browser actions, filesystem,
command, memory, delegation, extensions, or the scout itself. It uses static
fetch before browser rendering, allows at most two upstream searches, and
produces only optional draft/evidence. Phase B is a fresh tool-free Agent on the
same local child route. It receives only a bounded untrusted evidence packet,
grounded source labels, and the optional collector draft, so it cannot recurse
or emit raw tool dumps. Browser cleanup occurs between phases.

The parent-owned result is compact JSON containing the synthesized brief, source URLs
observed in actual successful tool results, tool-call counts, elapsed time,
status, and fixed limitations. Inspected fetch/browser sources outrank
search-only candidates; search relevance/order is preserved within each class.
Child prose URLs are sanitized against the observed source set, so prose cannot
invent source links. Evidence is bounded to 24 KiB, labels inspected sources
above search-only sources, and redacts sensitive/transient URL parameters.
Browser tabs are closed best-effort after success, timeout, cancellation, or
provider failure. A result is `complete` when synthesis produces a brief with
at least one grounded source; expected collector search/step stops may be
reported as limitations. Degraded backends, synthesis failure/timeout, and
missing grounded evidence remain `partial`.
Results are untrusted reference data and do not become durable session history
or memory.

Scout limits are intentionally bounded: timeout 30–600 seconds, 2–16 tool
steps, 4096–32 KiB output, 512–4096 generation tokens, 1–20 sources, and 1–3
concurrent calls. Zero config values select the documented defaults. The
configured values are hard ceilings; callers cannot select a child model,
endpoint, or larger budget from the tool arguments. The default is 10 collector
steps (maximum 16). The overall timeout starts before queue acquisition and
reserves synthesis time; the upstream search gate permits at most two actual
searches.
