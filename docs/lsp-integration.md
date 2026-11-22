# Language-server integration

## Inventory

Bee's deterministic file mutations use the operation-driven `write_file` tool.
Every operation commits through the mutation journal before returning. `Executor` then invokes
the session-bound `post_tool_call` lifecycle seam and appends bounded hook
observations to the tool result. This means an observer can inspect a committed
file, but cannot authorize, modify, or roll back the mutation.

Local MCP and extension processes already establish the child-process trust
boundary: commands are operator configured, environment inheritance is
default-deny, configured write roots are enforced by the process sandbox,
protocol input is bounded, stderr is discarded, and child failure is reported
without including child-controlled text. LSP uses those same process helpers;
it does not use a shell or discover binaries.

## Configuration and authority

Language servers are declared in the primary config. A server is selected
only by its configured writable workspace plus file patterns. Tool arguments
contain a file and position, never a command or server name.

```json
{
  "lsp": {
    "servers": [
      {
        "name": "go",
        "enabled": true,
        "command": "gopls",
        "args": [],
        "working_dir": "/srv/work/bee",
        "language_id": "go",
        "file_patterns": ["*.go"],
        "env_allowlist": ["PATH"],
        "startup_timeout_seconds": 5,
        "request_timeout_seconds": 5,
        "max_frame_bytes": 4194304,
        "max_file_bytes": 1048576,
        "max_output_bytes": 32768
      }
    ]
  }
}
```

`working_dir` is required, must exist, and must be within one of the
assistant's configured write roots. File patterns, source size, protocol
frames, diagnostics, timeouts, and model-visible output all have hard bounds.
Overlapping server declarations fail the affected request rather than letting
the model resolve the ambiguity.

## Lifecycle and behavior

Each configured server starts lazily on the first matching edit or read-only
query. Bee performs the LSP initialize handshake, synchronizes a bounded UTF-8
document, and supports:

- post-commit diagnostics for successful `write_file` calls;
- `lsp_hover`;
- `lsp_definition`;
- `lsp_references`.

The three explicit tools are read-only. Code actions, formatting, rename,
workspace edits, and server-initiated edits are not exposed. A server request
to `workspace/applyEdit` is refused. Diagnostics and query results are
untrusted observations and do not alter policy or approval decisions.

Servers are closed with the owning application. A missing executable, crash,
timeout, malformed frame, or oversized response makes that server unavailable
for the rest of the application activation. Post-edit failure is fail-soft: the
already committed mutation remains successful and no diagnostic observation is
added. No child stderr or protocol error text is logged or returned.
