# Lifecycle hooks

Bee can run owner-configured local observations at four fixed lifecycle
points: `session.start`, `session.end`, `pre_tool_call`, and
`post_tool_call`. Hooks observe; they never authorize, deny, rewrite, or retry
an operation. The plan gate, policy, and owner approval all finish before a
pre-tool hook runs.

Configuration is declaration ordered and immutable for the application
lifetime:

```json
{
  "hooks": {
    "commands": [
      {
        "point": "pre_tool_call",
        "command": "/opt/bee-hooks/trace",
        "args": ["--format=json"],
        "working_dir": "/srv/bee",
        "env_allowlist": ["PATH"],
        "timeout_seconds": 5,
        "max_output_bytes": 16384
      }
    ]
  }
}
```

`command` is executed directly with `args`; Bee never interpolates a shell
string. A hook receives one bounded JSON object on stdin. Tool hooks receive
`point`, `session_id`, `tool_name`, and serialized `arguments`; post-tool hooks
also receive only the bounded outcome category `succeeded`, `failed`,
`timed_out`, or `canceled`, never the tool payload.

Hooks reuse Bee's supervised child boundary: filtered environment, no PTY or
background mode, a per-call timeout, bounded stdout/stderr, process-group
termination, read-only host files, and no network namespace. A host hook may
inspect the session transcript on disk, but cannot persist changes through
the hook sandbox. Output is recorded as a typed session event and presented
to the model only as explicitly labeled untrusted observation context. A
`session.start` observation is projected as a matched synthetic tool result,
never as a system message, and the context budget keeps or drops that
assistant/tool pair atomically. Pre/post-tool observations occupy a separate,
bounded `hook_observation` field in Bee's tool-result envelope, so adding a
hook cannot truncate or corrupt a tool's already-bounded structured output.
Bee does not log hook input or output.

A nonzero exit, timeout, cancellation, or startup failure disables only that
configured hook for the current process activation of that session. Other
hooks at the same point continue in config order. Hook failure never changes
the observed tool/session operation's result. `session.start` runs after a
session has been initialized and before its first provider turn in that
application activation; `session.end` runs during clean `Application.Close`.
