# Bee GLM-5.3 Flash chat-template candidate

`example-model.jinja` is a **candidate**, not an applied live-service change.

## Provenance

- Base template: the live GLM-5.3 Flash model's embedded `chat_template`, captured from
  `GET http://127.0.0.1:8081/props` on **September 15, 2026**.
- Model: a GLM-5.3 Flash GGUF served by llama.cpp.
- Comparison references:
  - `/home/user/src/llama.cpp/models/templates/example-model.jinja`
  - `/home/user/src/llama.cpp/models/templates/example-model.jinja`

## Deliberate differences

- Preserves the live GLM-5.3 reasoning controls and behavior: `reasoning_effort` (`low`/`high`, otherwise `max`), `clear_thinking` defaulting to `false`, multimodal markers, and the current generation prompt (`<|assistant|><think>`).
- Keeps the live GLM-5.3 tool declaration formatting, including unwrapping OpenAI-style `function` entries and excluding deferred/strict metadata from the serialized schema.
- Replaces the live GLM-5.3 ID-based tool-result sorting/reordering block with the simpler GLM-4.6-compatible sequential block: assistant calls use `<tool_call>...<arg_key>...<arg_value>...</tool_call>`, and tool results use `<|observation|>` plus `<tool_response>...</tool_response>` in message order.
- Retains the GLM-4.6 safety check that rejects non-mapping tool arguments instead of emitting an ambiguous call.

This is intended to reduce parser surprises for Bee's ordinary tool-call loop. It has **not** been runtime-tested against the live model or service here.

## Launching a separate llama-server test

Do not replace or restart the live Bee service for this candidate. For an isolated test process, add the file option to a copy of the live command:

```bash
/path/to/llama.cpp/build/bin/llama-server \
  -m /path/to/model.gguf \
  --jinja \
  --chat-template-file /path/to/bee/templates/example-model.jinja \
  --chat-template-kwargs '{"enable_thinking":true,"reasoning_effort":"high"}' \
  --host 127.0.0.1 --port 18081
```

Use the exact llama.cpp build and non-conflicting port chosen by the operator. The command above is documentation only; it was not launched as part of this change.

## Checks run

- Extracted the embedded template from live `/props` JSON and compared its reasoning/generation sections with the candidate.
- Compared the tool-call and tool-result sections with local `example-model.jinja` and `example-model.jinja`.
- Performed static delimiter/structure checks, a Jinja syntax parse, and a generic Jinja render smoke test with synthetic tool-call data; no model load or candidate request was performed.
