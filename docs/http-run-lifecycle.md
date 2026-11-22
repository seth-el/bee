# HTTP run lifecycle and recovery

The HTTP run lifecycle is an adapter projection of Bee's existing `RunEvent`
stream. It is not a second scheduler or run state machine, and a client never
has to infer tool or approval state from assistant text.

## Existing state boundary

The runtime emits `run_start`, assistant deltas, tool calls/results, approval
events, failures, and `run_end`. The TUI already passes those events through a
`RunEventDisplayProjector`, which enforces the effective owner display policy.
OpenAI streaming previously projected only assistant deltas into compatible
chat-completion chunks; its `: ping` comments carried transport liveness but no
run state. `Application.activeRuns` supported cancellation only and did not
retain replayable status. The separate legacy gateway run registry was not
reachable through the normal `bee --serve` OpenAI path.

An ordinary OpenAI request is owned by its HTTP request context, so a client
disconnect cancels it. Recovery needs the run to outlive that one
response transport, but only when the authenticated client asks for that
behavior explicitly.

## Opt-in projection

A client sends `X-Bee-Run-Lifecycle: 1` on streaming chat completions. Only that
request shape detaches the run context from transport cancellation; Bee's
existing request timeout remains the hard lifetime bound. Requests without the
header retain their prior cancellation and OpenAI SSE behavior. The opt-in is
rejected for non-streaming requests.

Bee passes the same core events through `RunEventDisplayProjector` and retains
a bounded, in-memory lifecycle projection. A record contains only:

- run and durable-session identifiers;
- a monotonic cursor and timestamp;
- `running`, `streaming`, `running_tool`, `waiting_for_approval`, `done`, or
  `failed`;
- a tool name only when owner display policy permits it; and
- approval status and a terminal flag.

It never contains prompts, model output, reasoning, tool arguments/results, or
approval summaries. The initial OpenAI stream carries these records as
`: bee-run {...}` comments, so existing OpenAI data-frame consumers continue
to see the same contract.

## Authenticated recovery seam

Both routes require the existing bearer-token authentication and the
client-owned `X-Session-Id`, set `Cache-Control: no-store`, and scope results to
that exact external conversation mapping:

- `GET /api/runs/current` returns the latest projected record or `idle`.
- `GET /api/runs/{id}/events?after=N` replays retained records after the cursor
  and then follows the run with SSE keep-alives. It ends at a terminal record
  or after ten minutes, at which point the client may reconnect explicitly.

The registry retains at most 128 runs and 256 transitions per run. A replay
cursor older than the retained window returns `409`; the client should then reload
durable display history instead of guessing at missing states.

Recovery performs only status/replay GETs. It never calls chat completions,
resurrects a run, or submits a second turn. Once the terminal event arrives,
a client can replace any partial transcript with Bee's bounded durable
user/assistant history. The durable session turn lease remains authoritative
throughout, and clients should show only approvals matching the selected
session's current projected run.

## Manual verification

1. Send a streaming chat completion with `X-Bee-Run-Lifecycle: 1` and confirm
   the `: bee-run {...}` comments move through running, streaming, and done
   without carrying response text.
2. Run a visible tool and confirm its policy-visible name appears; switch tool
   display off and confirm the tool name does not cross the seam.
3. Trigger owner approval and confirm `GET /api/runs/current` reports
   `waiting_for_approval` for that session only.
4. Disconnect during an active response, then call
   `GET /api/runs/{id}/events?after=N`. Confirm replay resumes from the cursor
   and no second completion is started.
5. Send a streaming request without `X-Bee-Run-Lifecycle`; disconnect it and
   confirm the original request-owned cancellation behavior is unchanged.
