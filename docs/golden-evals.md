# Golden-task evaluation

`bee-eval` runs deterministic, network-free scenarios against Bee's real
application, executor, approval, MCP, lifecycle-hook, durable-session,
recurring-scheduler, and mutation-journal seams:

```sh
go run ./cmd/bee-eval run testdata/eval
```

Fixtures are strict, bounded JSON under `testdata/eval`. Each turn supplies a
prompt and scripted fake-provider responses. The expected trace contains only
structural behavior: session/turn boundaries, tool-call order, canonical JSON
arguments, approval categories, bounded tool outcomes, hook point/outcome, and
mutation-journal paths. It intentionally excludes assistant prose, hook
output, full tool results, timestamps, and generated identifiers.

The committed set covers a plain tool call, a gated mutation and approval,
stdio MCP invocation, lifecycle hooks, a two-turn durable-session resume, and
a stubbed-clock recurring firing that records its fresh durable session.
The runner exits nonzero on any mismatch and prints a compact unified diff of
the changed structural JSON window. GitLab CI runs it after the ordinary unit
suite; `make eval` is the matching local command.

Adding a scenario should exercise an existing core seam rather than inventing
an eval-only executor. The fake provider is the only behavior stub. Hook
process output is deterministic in the harness, while unit tests separately
cover the supervised OS-process boundary. MCP uses a real stdio child served
by the `bee-eval` executable itself.
