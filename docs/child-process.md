# Isolated child process protocol (`bee.child.v1`)

This process transport is supported on Linux only. Windows is explicitly out of scope; the Windows process-group file is a deliberate deferred stub, not a supported fallback.

Phase 6B adds one process-backed implementation of the Phase 6A `ChildExecutor` boundary. Phase 6C adds optional observation forwarding for that boundary. It remains deliberately **one child per run**: there is no scheduler, swarm, parallel fan-out, worktree management, retries, or automatic approval delegation.

## Lifecycle

The parent starts an explicitly configured command with:

- a dedicated Unix process group (`setpgid`); cancellation kills the group with a negative-PID signal;
- an explicit working directory contained by the `ChildTask.Scope.Root`;
- an allowlisted environment; secret-shaped names such as `*_KEY`, `*_TOKEN`, `*_SECRET`, and `*_PASSWORD` are rejected;
- startup/request timeouts and bounded newline-delimited JSON frames/messages.

The child receives exactly one `initialize` request followed by one `task` request. The initialize payload contains the `ChildTask` and minimal runtime configuration. `ProviderConfig.APIKeyFile` is always blank on the wire. The child exits after returning one validated `ChildCompletionPacket` as the `task` response.

Notifications use `{"jsonrpc":"2.0","method":"event",...}` and are descriptive only. The child emits lifecycle, provider-delta, assistant, tool, approval, usage, and completion observations; the completion packet is still sent exactly once as the task response. A parent may forward these through `ChildObserver` to a bounded `TraceSink`.

## Parent-owned broker seams

The child never receives provider credentials. When its `Agent` needs a streamed provider response it sends a `provider/stream` request containing only the normalized provider-neutral `Request`. The parent invokes its configured `Provider` and returns a bounded response plus collected deltas.

High-risk child tools use an `approval/request` request. The parent invokes the parent executor's `ApprovalHook`; the child cannot approve itself or widen the task/tool/scope contract. If no parent approval hook exists, the request fails closed.

The broker is intentionally narrow. Custom and extension tool implementations, custom policy/audit serialization, and provider capability delegation remain deferred; process mode currently supports the built-in coding tools only. If the parent has a custom policy or auditor configured, process mode fails closed rather than silently bypassing it. Parent-side approval remains authoritative.

Phase A also adds a parent-owned shared local-child capacity broker. It is
separate from the process runner's single-flight lifecycle and coordinates
child, specialist, recon-review, and child-backed memory endpoint work by
bounded priority lanes with cancellation-aware queueing. A process child does
not recursively acquire the broker for its parent-brokered run; provider,
tools, approvals, mutations, and durable writes remain parent-owned.

## Failure handling

The parent rejects malformed JSON, invalid protocol identity, oversized frames/messages, unknown or duplicate response IDs, crashes, and incomplete/mismatched completion packets without treating child data as instructions. Cancellation first sends `$/cancelRequest`; after the bounded grace period the parent kills the entire Unix process group. Windows process-group cleanup is unsupported and outside the project target.

The child receives no parent session history or durable memory. It receives only the task and minimal runtime configuration supplied during initialize.
