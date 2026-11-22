# Live plan and acceptance tracking

Bee's ordinary trusted/default assistant exposes three related model-facing
planning capabilities:

- `bee_plan` records the bounded plan and initializes acceptance criteria.
- `plan_status` reads the current session-scoped status.
- `plan_update` applies a bounded partial update to existing criteria.

Criteria use exactly these states:

- `pending`
- `in_progress`
- `passed`
- `failed`
- `blocked`

A criterion may be marked `passed` only when it includes at least one bounded,
parent-verified evidence reference with `provenance: "observed"`. Observed
references currently identify a successful durable `tool_result` call ID or a
durable `artifact` ID from the same session. Evidence with
`provenance: "model_claim"` is retained separately for transparency and never
counts as observed verification. Bee does not silently convert a model claim
into an observation.

Each provider-facing tool result includes its `call_id` in the structured
result envelope. `plan_status` also returns up to 12 newest `recent_evidence`
entries, newest first, containing only the source, opaque reference, and tool
name. Failed calls and plan-bookkeeping tools are not advertised, and no tool
arguments, output, artifact names, or host paths are included.

`plan_update` may copy an exact `source` and `reference` from that list. For a
successful tool result it may instead supply an observed `tool_result` with
`selector: {"kind":"latest_successful_tool_result","tool_name":"..."}` and
omit `reference`. The session store resolves the selector under the same lock
as the update, validates the resolved call, and persists only the exact call
ID. Missing matches fail closed.

Updates are durable `plan_update` events in the existing private session JSONL
store. The store's per-session lock and atomic append path serialize concurrent
updates, and status is rebuilt from the session event stream. The update tool
cannot select another session: it uses the durable session bound to the current
run and fails closed without one.

`plan_status` intentionally returns only bounded plan identity, redacted and
truncated criterion text, states, evidence provenance, opaque references, and
the bounded recent-evidence index described above. It does not return raw plan
goals, prompts, evidence summaries, credentials, tool payloads, or filesystem
contents. Evidence attached to criteria is capped per provenance class and
marked truncated when additional references exist.

Three consecutive evidence-validation failures in one turn stop the agent loop
even when the model rewords the invalid reference or interleaves `plan_status`
calls. A successful non-bookkeeping tool result or plan update resets this
semantic guard.

The tools are composed only for ordinary assistant turns. Locked compositions
have no executor; child runners and specialist-style agents do not expose live
plan tracking. Existing policy, approval, mutation, and `bee_plan` gates remain
authoritative for workspace changes.

This document records the source contract only. It is not deployment or live
acceptance evidence.
