# CLIProxy Antigravity

A CLIProxyAPI dynamic provider that runs Google's **official Antigravity CLI (`agy`)** instead of reusing Antigravity OAuth tokens or calling private Antigravity endpoints.

The plugin presents `agy` as an OpenAI-compatible Chat Completions provider inside CLIProxyAPI:

```text
OpenAI-compatible client
        |
        v
    CLIProxyAPI
        |
        v
cliproxy-antigravity plugin
        |
        | spawn official CLI
        v
       agy
        |
        v
Google Antigravity
```

Authentication, account state, model access, permissions, tools, and the Antigravity runtime remain owned by the official CLI.

> [!IMPORTANT]
> This project is an independent community integration. It is not affiliated with, endorsed by, or sponsored by Google, Antigravity, CLIProxyAPI, or Synara. Provider terms can change. This architecture intentionally uses the documented `agy` headless interface, but users remain responsible for complying with the terms that apply to their accounts and usage.

## Status

**v0.1** is intentionally narrow and conservative:

- OpenAI Chat Completions-compatible text requests
- non-streaming responses through `agy --output-format json`
- streaming responses through `agy --output-format stream-json`
- dynamic model discovery through `agy models`
- Antigravity token usage mapped to OpenAI-compatible usage fields
- no Antigravity OAuth/token extraction
- no direct calls to private Antigravity services
- no fake CLIProxyAPI Antigravity credential
- no client-side OpenAI tool-call emulation

The plugin registers under the provider key **`agy`**, not `antigravity`, so it does not replace CLIProxyAPI's built-in Antigravity provider.

## Requirements

- A recent CLIProxyAPI build with standard dynamic-library plugin support enabled
- The official Antigravity CLI installed and available as `agy`, or its path configured with `binary_path`
- An authenticated Antigravity CLI session (`agy` should work normally before using the plugin)
- Go 1.23+ and a C compiler to build the plugin from source

CLIProxyAPI's Linux `no-plugin` build cannot load dynamic-library plugins.

## Build

### Linux

```bash
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o cliproxy-antigravity.so .
```

### macOS

```bash
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o cliproxy-antigravity.dylib .
```

The Go toolchain also creates a `.h` file. CLIProxyAPI only needs the dynamic library.

You can also run:

```bash
make test
make build
```

## Install in CLIProxyAPI

1. Build the plugin for the same operating system and architecture as CLIProxyAPI.
2. Copy the resulting library into CLIProxyAPI's plugin directory. The basename must remain `cliproxy-antigravity`.
3. Enable the plugin in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cliproxy-antigravity:
      enabled: true
      binary_path: "agy"
      workdir: "/absolute/path/to/your/project"
      print_timeout: "30m"
      dangerously_skip_permissions: false
      sandbox: false
```

4. Start CLIProxyAPI and inspect `/v1/models`.

At minimum the plugin publishes:

```text
agy/default
```

When `agy models` succeeds, it also publishes discovered models using names such as:

```text
agy/gemini-3.8-flash-high
agy/gemini-3.8-flash-medium
```

`agy/default` omits the `--model` flag and lets your installed Antigravity CLI choose its default model.

## Example request

Use the same API key and base URL you normally use for CLIProxyAPI:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_CLIPROXY_API_KEY' \
  -d '{
    "model": "agy/default",
    "messages": [
      {"role": "user", "content": "Explain what this repository does in one paragraph."}
    ]
  }'
```

Streaming works with the normal Chat Completions shape:

```json
{
  "model": "agy/default",
  "stream": true,
  "stream_options": {"include_usage": true},
  "messages": [{"role": "user", "content": "Write a short Go example."}]
}
```

## How requests are executed

For a non-streaming request the plugin effectively runs:

```bash
agy --output-format json --print-timeout 30m -p "<converted conversation>"
```

For a streaming request it runs:

```bash
agy --output-format stream-json --print-timeout 30m -p "<converted conversation>"
```

A selected model adds:

```bash
--model <agy-model-slug>
```

The plugin parses `agent_response` deltas from Antigravity's NDJSON stream and emits OpenAI-compatible SSE chunks through CLIProxyAPI's plugin stream bridge.

## Configuration

| Field | Default | Description |
| --- | --- | --- |
| `binary_path` | `agy` | Official Antigravity CLI executable or absolute path. |
| `workdir` | empty | Working directory given to the `agy` process. Set this to the project that Antigravity is allowed to work in. |
| `print_timeout` | `30m` | Passed to `--print-timeout`. |
| `dangerously_skip_permissions` | `false` | Adds `--dangerously-skip-permissions`. This auto-approves all Antigravity tool permission requests. |
| `sandbox` | `false` | Adds Antigravity's `--sandbox` flag. |

### Permission safety

`dangerously_skip_permissions` is deliberately **off by default**. When enabled, Antigravity can auto-approve tool calls including file writes and shell commands. Prefer Antigravity's scoped permission rules when possible.

Do not expose a CLIProxyAPI instance using this plugin to untrusted users unless you understand the consequences. A remote prompt may cause the `agy` process to operate on the configured `workdir` according to the permissions of the OS user running CLIProxyAPI and the permission policy configured in Antigravity.

## Compatibility and limitations

### OpenAI tools

This plugin does **not** translate OpenAI `tools[]` into external client-executed tool calls. Antigravity is itself an agent runtime with its own tools. If a client sends tool schemas, the plugin leaves execution to Antigravity and returns a normal assistant response.

This is useful for clients that primarily need an OpenAI-compatible transport, but it is not yet a drop-in replacement for a raw LLM endpoint whose tool calls must be executed by the client.

### Conversation state

v0.1 sends the full Chat Completions `messages[]` history to a fresh headless `agy -p` invocation. It does not yet map client session IDs onto Antigravity `conversation_id` values or maintain persistent `--input-format stream-json` processes.

### System messages

The Antigravity print interface receives a prompt, not OpenAI's separate system-message channel. The plugin preserves role ordering and labels system/developer messages clearly inside the prompt, but this is not identical to a provider-native system instruction API.

### Images

Image content blocks are rejected in v0.1 instead of being silently discarded. Text content blocks are supported.

### Working directory

`workdir` is currently global for the plugin configuration. If one CLIProxyAPI instance serves several unrelated repositories, use separate instances/configurations or wait for per-request workspace routing support.

## Why this approach?

The project deliberately avoids turning an Antigravity account session into a reusable OAuth-backed HTTP provider. The plugin shells out to the official `agy` executable, and `agy` remains responsible for authentication and service access.

Google's public Antigravity CLI documentation describes headless mode as the programmatic interface for scripting, CI pipelines, machine-readable JSON/NDJSON output, model selection, permissions, and persistent stdin-driven sessions. This plugin builds on those documented interfaces rather than reverse-engineering the service.

## Development

```bash
gofmt -w *.go
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/cliproxy-antigravity.so .
python3 scripts/abi_smoke.py /tmp/cliproxy-antigravity.so
```

No third-party Go dependencies are required for the plugin itself.

## Roadmap

Likely next steps:

- persistent `agy --input-format stream-json --output-format stream-json` process pool
- safe session-to-`conversation_id` mapping
- per-request/project working-directory routing
- richer Antigravity tool/step metadata
- structured-output support
- optional reasoning-effort aliases
- release binaries for supported platforms

Client-side OpenAI tool-call bridging should only be added if it can preserve a clear separation between the client agent runtime and Antigravity's own agent runtime.

## Acknowledgements

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) for the dynamic plugin ABI and reference examples.
- [Synara](https://github.com/Emanuele-web04/synara) for demonstrating a clean architecture where the official `agy` CLI remains responsible for Antigravity authentication, models, permissions, and runtime behavior. The implementation in this repository is independently written against the public CLI and CLIProxyAPI plugin interfaces.

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for licensing notes.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first. Security reports should follow [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
