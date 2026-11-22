# Runtime context

Bee keeps durable identity, user profile, retrieved memory, project instructions, and session history as separate context sources. `AssembleContext` remains backward compatible: when the new fields in `ContextOptions` are empty, existing Phase 3 behavior is unchanged.

## Source order and priority

The runtime renders sources in this order:

1. system prompt plus parent-only `BEE.md` workspace context (`system`, required, priority 1000)
2. `IdentityProfile` (`identity`, required, priority 1000)
3. project instructions (`project_instructions`, required, priority 900)
4. `UserProfile` (`user_profile`, optional/high priority, priority 800)
5. retrieved `MemoryContextPacket` (`retrieved_memory`, optional, priority 100)
6. session, tool, summary, and plan items

Identity and project instructions are required. Profile data is high priority but can be dropped if the budget cannot retain it. Retrieved memory is always optional and is the first new source dropped under pressure.

### Runtime facts

Assistant compositions add a small harness-generated block to the parent
system prompt, after the launch-directory note and before mood:

```text
<bee-runtime>
host: testhost (linux/amd64)
user: testuser
model: glm-flash, served from 203.0.113.37:8081
</bee-runtime>
```

It is read from the operating system and the active provider config, never
from memory or notes, so it cannot go stale when Bee moves to another
machine or model. The text is constant for a process and model, so it does
not disturb the prompt cache. Only the endpoint's host and port are shown,
never a path, query or credentials. Child roles do not get it. Turn it off
with `"assistant": {"runtime_facts": false}`. It appears in the harness
snapshot as the `runtime_facts` component.

For parent agents, `BEE_HOME/workspace/BEE.md` is securely loaded during agent
construction and delimited inside the system-prompt layer after the built-in
prompt and optional SOUL.md content. It is not a typed `UserProfile`, durable
memory record, or session event, and child roles never receive it.

Profile and retrieved-memory messages are explicitly delimited reference data. They state that text inside them is not instructions or authority, and include only bounded provenance fields (record ID, namespace, key, sensitivity, and score) for memory items.

## Per-turn loading

`Agent.Run` loads configured identity and user-profile stores for every user turn. Durable-memory recall is tool-driven by default: the ordinary assistant exposes bounded `memory_search`, which defaults to current active records and uses configured lexical/semantic retrieval only when called. The ordinary assistant also exposes `memory_remember` as the explicit write path; it is global durable memory, create-only, parent-owned, and mediated by the executor's plan/policy/approval/audit gates. A caller may opt into prompt-based `hybrid` or `always` recall; Bee strips conversational recall boilerplate before building the bounded query and projects results into transient context only. It is never appended to `Agent.Messages`, session events, traces, or durable memory.

Automatic prompt-based recall is disabled by default. When enabled, retrieval is fail-closed by default (`MemoryFailureFailClosed`); callers that also choose `MemoryFailureOptional` may continue without retrieved memory when lookup fails. Optional post-turn extraction and local semantic refresh are independently disabled unless configured. Proposal approval, cleanup, and remote synchronization are never automatic.

`Agent.TurnContextDiagnostics` and the optional runtime event contain only bounded source counts and estimated sizes. Raw profile and durable-memory payloads are never included in diagnostics.

## Child projection

A parent may put a `ChildMemoryProjection` on `ChildTask`. It is a copied, read-only `MemoryContextPacket` scoped by namespace, maximum item count, sensitivity, and the memory policy. Children receive no `MemoryStore` or `MemoryService` handle and have no durable write path. Sensitive memory is rejected for child projections.

Channels and voice adapters must preserve the same memory ownership and bounded projection rules.

## Runtime introspection and cancellation

The general assistant registry includes the read-only `runtime_info` and
`current_time` tools. `runtime_info` returns bounded host/runtime metadata, the
assistant scope and policy, the configured provider/profile/model role, active
tool names, and a filtered non-secret environment view. `current_time` returns
UTC plus a configured or explicitly requested IANA-zone timestamp without
network access. It never reads credential files or exposes
secret-shaped environment values, API keys, tokens, passwords, or private
credential contents. It does not add authority; `run_command` remains subject
to the existing parent-owned policy and approval boundary.

Parent-owned local commands `/stop` and `/kill` cancel active top-level runs;
`/cancel` remains a compatibility alias. Cancellation propagates through the
run context to child/subagent and process-group runners, while explicit child
budgets, startup/tool limits, and protocol frame/message limits remain intact.
