# MCP client

Bee can expose tools from explicitly configured Model Context Protocol servers
through its ordinary tool registry. MCP is an adapter boundary, not a second
authorization or execution system.

## Inventory and seam

Native tools implement `Tool`, publish a `ToolDefinition`, and enter a shared
`Registry`. `Executor.Execute` is the authoritative path for argument shape,
the plan gate, `GeneralAssistantPolicy`, owner approval, invocation timeout,
bounded output, and audit. The application-wide approval broker is attached to
that executor, so a projected MCP tool uses the same owner-decision flow as a
native mutating tool.

The existing Bee extension host is the closest lifecycle analogue: it owns an
explicitly configured subprocess and projects advertised tools into the same
registry. MCP uses the same `ExecutionScope` process-sandbox launcher as local
execution tools, including Bubblewrap write-root containment when enabled,
filtered environment variables, a dedicated process group, and descendant
termination. It does not use the Bee-specific extension wire protocol.

`Config.MCP` is loaded from the primary JSON config. All product roots already
call `Application.StartExtensions` and `Application.CloseExtensions`; those
lifecycle methods now cover both Bee extensions and MCP servers. A server that
fails to start, handshake, list tools, or register its namespace is recorded in
`Application.MCPFailures` using only a generic category. Its I/O is never put
in that status, and its failure does not fail the gateway or other servers.

## Supported protocol

This first phase supports local `stdio` servers using newline-delimited
JSON-RPC 2.0 and the handshake-based MCP `2025-11-25` revision:

1. Bee launches the configured command.
2. Bee sends `initialize` with protocol version `2025-11-25`.
3. Bee validates the selected version and server identity, then sends
   `notifications/initialized`.
4. If the server declares the `tools` capability, Bee obtains bounded,
   paginated declarations with `tools/list`.
5. Registered tools are invoked with `tools/call`.
6. Shutdown closes stdin, waits briefly, and then terminates the process group
   if necessary.

HTTP transports, resources, prompts, server-to-client requests, dynamic tool
list refresh, and the newer stateless MCP lifecycle are intentionally out of
scope. A server that requires one of those surfaces is unavailable rather than
partially emulated.

## Configuration

```json
{
  "mcp": {
    "servers": [
      {
        "name": "workspace",
        "enabled": true,
        "transport": "stdio",
        "command": "/absolute/path/to/example-mcp-server",
        "args": ["--root", "/srv/workspace"],
        "working_dir": "/srv/workspace",
        "env_allowlist": ["PATH"],
        "startup_timeout_seconds": 5,
        "request_timeout_seconds": 30,
        "max_frame_bytes": 1048576,
        "max_message_bytes": 4194304,
        "max_output_bytes": 262144,
        "max_tools": 64,
        "allow_tools": ["search", "read_file", "write_file"],
        "deny_tools": ["write_file"],
        "read_only_tools": ["search", "read_file"]
      }
    ]
  }
}
```

Only enabled entries run. Bee never scans for MCP servers. There may be at most
16 configured servers. Server and tool names use at most 64 ASCII letters,
digits, dots, dashes, or underscores. Exposed names are deterministic and
namespaced as `mcp_<server>_<tool>`; a projected name longer than 64 bytes or a
normalization collision rejects that server's declarations.

An empty `allow_tools` list permits every valid advertised tool; a non-empty
list permits only exact matches. `deny_tools` always wins. Each server may
advertise 64 tools by default and at most 128 when configured. Listing is also
limited to 16 pages and 1 KiB cursors. Descriptions are capped at 4 KiB and
input schemas at 64 KiB; the schema must be a JSON object schema with
`"type":"object"`. One malformed or duplicate declaration rejects the whole
server so Bee never exposes an ambiguous partial tool set. Projected tool
descriptions explicitly label the server-provided metadata and results as
untrusted data; schemas and descriptions cannot grant authority.

Startup defaults to five seconds and is capped at 300 seconds. Requests default
to 30 seconds and are capped at 3,600 seconds. Frames default to 1 MiB,
messages to 4 MiB, and both have a 16 MiB hard cap. MCP results are retained in
memory only within those bounds. A result defaults to a 256 KiB MCP-side cap,
has a 1 MiB hard cap, and is then subject to the executor's normally smaller
output ceiling (64 KiB by default).

## Trust and approval boundaries

MCP server annotations are untrusted hints and never grant authorization. Only
exact names in `read_only_tools` are projected as read-only. Every other MCP
tool is conservatively mutating/high-risk. In prompted policy mode it therefore
requires the same approval hook as a native mutating tool, and an active plan
gate applies normally. Trusted and locked policy retain their existing Bee
semantics; MCP does not redefine them.

The configured executable itself is operator-trusted code. The process starts
before any individual tool approval and can use the filesystem and network
available inside Bee's existing process sandbox. Configure only servers whose
code and launch arguments you trust. `env_allowlist` is empty by default, and
secrets are not added implicitly. stdout is protocol-only; stderr is drained
and discarded. Bee never logs server frames, stderr, tool arguments, tool
results, RPC error messages, or RPC error data. The shared audit still records
the projected tool name and authorization outcome, but substitutes a fixed
redaction marker for MCP arguments because provider-defined keys cannot be
classified safely.

If a running server exits or violates the protocol, any already-dispatched call
returns the generic error `MCP server is unavailable` and the application
removes that server's tools from the registry for later turns. Other tools and
sessions continue. Bee does not auto-restart MCP servers in this phase; restart
the Bee process after correcting the server or configuration.

## Manual verification procedure

1. Configure a harmless local stdio server with one read-only echo/search tool
   and start Bee through the normal TUI or gateway product root.
2. Use `/tools` or the equivalent structured tool inventory and confirm the
   `mcp_<server>_<tool>` name is present while denied tools are absent.
3. Invoke the read-only tool and confirm the bounded MCP result reaches the
   ordinary tool-result event.
4. Remove the tool from `read_only_tools`, run Bee in prompted policy mode, and
   confirm the normal owner approval appears before `tools/call` is sent.
5. Terminate the server, retry its tool, and confirm Bee reports only that MCP
   server as unavailable while the session and a native read-only tool still
   work.
6. Exit Bee and confirm the MCP process and any descendants have stopped.
