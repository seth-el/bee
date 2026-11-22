# Orchestration

Bee's orchestration boundary is parent-owned. The parent owns task contracts, provider credentials, approvals, dependency decisions, aggregate budgets, cancellation, and artifact collection. Children do not communicate with one another, and completion packets remain descriptive rather than executable.

## Sequential compatibility

`ChildObservableExecutor` extends the unchanged `ChildExecutor` interface with optional lifecycle, provider, tool, approval, and completion observations. `TraceSink` and `JSONLTraceSink` persist bounded orchestration/task/child records without appending raw child logs to session context.

`OrchestrationPlan` validates task identity, parent ownership, dependency references, cycles, scopes, budgets, and non-overlapping root-contained artifact directories. `SequentialOrchestrator` chooses ready tasks by lexical task ID, runs exactly one child at a time, propagates dependency blocks and cancellation, applies aggregate timeout/step/output budgets, and permits one attempt per task. Existing Phase 6C callers and behavior remain valid.

## Bounded read-only parallel slice

`ChildTask.Mutability` declares `read_only` or `mutable`. An omitted value is inferred as read-only only when the task has no tools or only the built-in read-only tools `read_file`, `list_files`, `find_files`, and `grep`. Mutation-capable built-ins (`write_file` and `run_command`) or unknown/custom tools are never guessed to be read-only. A task explicitly marked read-only that includes such a tool is rejected. This conservative validation is performed before any child starts.

`OrchestrationPlan.MaxConcurrent` bounds the number of independent tasks in flight. Zero means one. `ParallelOrchestrator`:

1. selects ready tasks deterministically by lexical task ID;
2. starts no more than `MaxConcurrent` children;
3. forwards observations through the existing `TraceSink`;
4. applies one aggregate timeout, step budget, and output budget to the plan;
5. fans cancellation out through the shared context;
6. blocks dependents after non-successful tasks;
7. honors `stop_on_error` versus `continue_independent`; and
8. records task and orchestration completion state in the same bounded trace format.

Shared roots are allowed for read-only tasks when each task has a distinct artifact directory. An artifact directory that overlaps another task's scope is rejected because the child boundary writes `completion.json` there. Mutable tasks are supported only when the plan serializes execution (`MaxConcurrent <= 1`); a plan that could run two mutable children concurrently fails closed. The one-slot parallel mode delegates to `SequentialOrchestrator`, preserving Phase 6C ordering and semantics.

A typical caller supplies a child executor and optional durable sink:

```go
sink, err := bee.NewJSONLTraceSink("generated/orchestration/orch-1.jsonl")
if err != nil { /* fail closed */ }
orchestrator, err := bee.NewParallelOrchestrator(childExecutor, sink)
plan.MaxConcurrent = 3
result, err := orchestrator.Run(ctx, plan)
```

### Real runner capacity

`ParallelOrchestrator` can be backed by `ChildRunnerPool` or
`ProcessChildRunnerPool` when real children must overlap. Each pool has a
parent-owned bounded channel of independent single-flight runner slots. The
pool capacity is a second defensive limit in addition to
`OrchestrationPlan.MaxConcurrent`; acquiring a slot observes cancellation and
the slot is returned on every success, failure, timeout, or cancellation path.

```go
pool, err := bee.NewChildRunnerPool(app, 3)
if err != nil { /* fail closed */ }
orchestrator, err := bee.NewParallelOrchestrator(pool, sink)
```

In-process and process-backed slots do not share per-child lifecycle state,
output buffers, session state, or plan gates. Parent-owned provider, policy,
approval, cancellation, and trace boundaries remain authoritative. A child
plan gate is fresh per slot/task; a child never inherits another child's
mutation authorization. The real pool integration tests prove two in-process
children and two subprocess children reach a provider barrier concurrently.

The supported concurrent boundary remains read-only work. Mutable tasks still
require serialized orchestration (`MaxConcurrent <= 1`) or the separate
isolated Git-worktree path; pools do not grant concurrent writers permission
or add automatic merging.

## Leased mutable workspaces

`WorkspaceManager` and `WorkspaceLease` are provider-neutral contracts for mutable execution. `GitWorkspaceManager` validates the configured repository top-level, a resolvable base commit, a repository-contained task scope, a clean parent worktree, safe task/attempt IDs, a dedicated Bee marker root, and non-overlapping lease paths before calling `git worktree add --detach`. A managed root inside the repository is accepted only when Git explicitly ignores it; an external managed root is also supported. Git is invoked through bounded argv arguments; no shell strings are used.

`WorktreeOrchestrator` keeps the existing `ParallelOrchestrator` path for read-only plans. Explicit mutable tasks require a workspace manager and are isolated per task/attempt, including serialized mutable tasks. Child scopes and artifact directories are translated into the isolated worktree. Lease traces record creation, inspection, proposal collection, preservation, and removal. A failed or cancelled child is preserved by default, and successful children are also preserved because this slice has no automatic integration step. `Cleanup` is explicit and destructive only to the leased worktree.

`IntegrationProposal` is descriptive and review-only. It includes task/attempt IDs, base commit, workspace path, changed and untracked files, a bounded diff artifact, conflicts/errors, child tests/evidence/artifacts, and `ReadyForReview`. The proposal is never applied, cherry-picked, merged, or delegated automatically. Missing Git, a dirty parent repository, a non-repository scope, orphaned worktree registration, malformed/stale lease metadata, path ambiguity, or a bounded-diff failure fails closed.

## Deferred work

This slice does **not** implement automatic merging/cherry-picking, retries, background scheduling, automatic approval delegation, or durable memory. Those require separate contracts and operational review after the review-only proposal boundary is proven.
