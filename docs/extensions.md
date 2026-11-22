# Bee extension protocol

Bee extensions are explicitly configured subprocesses. Bee does not scan `PATH`, discover executables, or allow an extension to create its own tools outside Bee's registry and executor.

## Transport and version

The transport is newline-delimited JSON over the extension process's stdin/stdout:

- stdout is protocol-only;
- stderr is diagnostics and is never parsed as protocol;
- every frame is one JSON-RPC 2.0 request, response, or notification;
- the version is `bee.extension.v1`;
- frame and message sizes, startup, and request timeouts are bounded by configuration.

This is distinct from Bee's Phase 4 client-facing `RPCServer` methods. An extension process speaks the extension protocol; a client speaks the application RPC protocol.

## Configuration

```json
{
  "extensions": [
    {
      "name": "example",
      "enabled": true,
      "command": "/absolute/path/to/bee-extension-example",
      "args": [],
      "working_dir": "/tmp",
      "env_allowlist": ["PATH"],
      "startup_timeout_seconds": 5,
      "request_timeout_seconds": 30,
      "max_frame_bytes": 1048576,
      "max_message_bytes": 4194304
    }
  ]
}
```

The configured name and command are required for enabled entries. Duplicate extension names, duplicate advertised capability names, invalid tool definitions, and malformed limits are rejected. Disabled entries remain inert and are not started.

## Product composition and lifecycle

Both shipped product roots (`cmd/bee` and `cmd/bee-telegram`) compose
`bee.ComposeGeneralAssistant`. Enabled extensions are started only for an
actual assistant product run, after composition, and their advertised tools
are registered in that assistant's ordinary executor. The roots close every
started extension on return, including RPC/JSON and Telegram shutdown paths.
Inspection-only commands such as session listing, session inspection, and
Telegram pending-update inspection do not start extension subprocesses.

Locked assistant composition has no executor and therefore does not launch
configured extensions.

## Handshake

Bee sends a request with a string correlation ID:

```json
{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocol":"bee.extension.v1","host":"bee","version":"5"}}
```

The extension returns an identity/manifest and capabilities:

```json
{"jsonrpc":"2.0","id":"1","result":{"manifest":{"name":"example","version":"1.0.0","protocol":"bee.extension.v1"},"capabilities":{"tools":[{"name":"echo","description":"Echo text","parameters":{"type":"object"},"risk":"low"}],"notifications":["example/ready"]}}}
```

The manifest name must match the configured allowlist name. `providers`, `hooks`, and `context` are discovery seams only in Phase 5; Bee does not execute them. Extension notification/context data is untrusted and cannot inject system instructions or bypass context budgets.

## Tool calls and errors

Bee calls advertised tools with:

```json
{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}
```

A successful result is:

```json
{"jsonrpc":"2.0","id":"2","result":{"output":"hello"}}
```

A protocol failure uses a structured JSON-RPC error:

```json
{"jsonrpc":"2.0","id":"2","error":{"code":4001,"message":"extension rejected call","data":{"retryable":false}}}
```

The host detects malformed JSON, unknown or duplicate response IDs, oversized frames, timeouts, and process crashes. These failures are isolated to the extension and are returned to the normal Bee tool executor. The executor still applies root scope, policy, approval, output limits, audit, session, and run-event behavior.

## Cancellation and shutdown

When a call context is canceled, Bee returns the context error and sends a notification:

```json
{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":"2"}}
```

Bee requests graceful shutdown before closing stdin and killing a process that does not exit within the shutdown deadline:

```json
{"jsonrpc":"2.0","id":"3","method":"shutdown"}
```

A minimal standard-library example process is available at `cmd/bee-extension-example`.
