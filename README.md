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
        | spawn official CLI with stdin
        v
       agy
        |
        v
Google Antigravity
```

> [!IMPORTANT]
> **Authentication, account sessions, model access, permissions, and communication with Antigravity remain managed by the official agy CLI.**
>
> Unlike alternative proxy integrations that extract, copy, or directly handle Google's OAuth credentials or session tokens, `cliproxy-antigravity` interacts exclusively through the official `agy` CLI's documented headless interface.

> [!NOTE]
> This project is an independent community integration. It is not affiliated with, endorsed by, or sponsored by Google, Antigravity, CLIProxyAPI, or Synara. Provider terms can change. This architecture intentionally uses the documented `agy` headless interface, but users remain responsible for complying with the terms that apply to their accounts and usage.

## Status

**v0.1** features:

- OpenAI Chat Completions-compatible text & vision requests
- Non-streaming responses through `agy --output-format json`
- Streaming responses through `agy --output-format stream-json`
- Prompt streaming via stdin (avoiding OS `ARG_MAX` and command-line length limits)
- Multi-turn conversation resumption through `agy --conversation <id>`
- Reasoning effort control via `agy --effort` (`low`, `medium`, `high`)
- Multimodal image support via automatic workspace staging
- Client-side OpenAI `tools[]` calling emulation (`tool_calls`)
- Built-in model name aliasing (`gemini-3.8-flash-high`, `claude-3-7-sonnet-thought`, etc.)
- Dynamic model discovery through `agy models`
- Antigravity token usage mapped to OpenAI-compatible usage fields
- No Antigravity OAuth/token extraction
- No direct calls to private Antigravity services
- No fake CLIProxyAPI Antigravity credential

The plugin registers under the provider key **`agy`**, and supports requests addressed to `agy/*`, `antigravity/*`, or unprefixed models.

## Requirements & Platform Support

- A recent CLIProxyAPI build with standard dynamic-library plugin support enabled
- The official Antigravity CLI installed and available as `agy` (or `agy.exe` on Windows), or configured via `binary_path`
- An authenticated Antigravity CLI session (`agy` should work normally before using the plugin)
- Precompiled binary release assets are provided for all tier-1 supported operating systems:
  - **Linux**: `linux_amd64.zip`, `linux_arm64.zip` (`.so`)
  - **macOS / Darwin**: `darwin_arm64.zip`, `darwin_amd64.zip` (`.dylib`)
  - **Windows**: `windows_amd64.zip` (`.dll`)

See [SECURITY.md](SECURITY.md) for full details on process lifecycle management, process group isolation, and execution sandboxing.

CLIProxyAPI's Linux `no-plugin` build cannot load dynamic-library plugins.

## Install in CLIProxyAPI

### From CLIProxyAPI Plugin Store or Prebuilt Release

Download the archive for your platform from the [Releases](https://github.com/adeebahmad01/cliproxy-antigravity/releases) page or install through CLIProxyAPI:

```bash
# Each archive contains the dynamic library at root:
# Linux: cliproxy-antigravity.so
# macOS: cliproxy-antigravity.dylib
```

Place `cliproxy-antigravity.so` (Linux) or `cliproxy-antigravity.dylib` (macOS) into your CLIProxyAPI `plugins/` directory.

### Build from Source

#### Linux

```bash
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o cliproxy-antigravity.so .
```

#### macOS

```bash
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o cliproxy-antigravity.dylib .
```

You can also run:

```bash
make test
make build
```

### Configuration

1. Ensure the library is located in CLIProxyAPI's plugin directory. The basename must remain `cliproxy-antigravity`.
2. Enable the plugin in `config.yaml`:

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
      reasoning_effort: "high" # optional default: low | medium | high
```

3. Start CLIProxyAPI and inspect `/v1/models`.

The plugin publishes standard built-in model aliases matching CLIProxyAPI conventions:

```text
gemini-3.8-flash-high
gemini-3.8-flash-medium
gemini-3.8-flash-low
claude-3-7-sonnet-thought
agy/default
```

When `agy models` succeeds, it also discovers and publishes installed models.

## Example request

Use the same API key and base URL you normally use for CLIProxyAPI:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_CLIPROXY_API_KEY' \
  -d '{
    "model": "gemini-3.8-flash-high",
    "messages": [
      {"role": "user", "content": "Explain what this repository does in one paragraph."}
    ]
  }'
```

Streaming works with the normal Chat Completions shape:

```json
{
  "model": "gemini-3.8-flash-high",
  "stream": true,
  "stream_options": {"include_usage": true},
  "messages": [{"role": "user", "content": "Write a short Go example."}]
}
```

### Multimodal Vision / Image Support

Clients can pass images using OpenAI-format `image_url` blocks (including base64 data URLs):

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "Describe this architecture diagram."},
    {
      "type": "image_url",
      "image_url": {"url": "data:image/png;base64,iVBORw0KGgo..."}
    }
  ]
}
```

The plugin decodes the image and stages it locally into `<workdir>/.cliproxy_cache/images/` for `agy` to inspect.

### Client-Side Tool Calling

When client applications (like Cline, RooCode, or Cursor) provide OpenAI `tools[]`, the plugin activates structured tool-calling mode. If the model invokes a tool, the response returns standard OpenAI `tool_calls`:

```json
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "call_12345_0",
        "type": "function",
        "function": {
          "name": "get_weather",
          "arguments": "{\"location\":\"Paris\"}"
        }
      }]
    }
  }]
}
```

The client application can execute the function and pass the result back with `role: "tool"`.

### Conversation Resumption

To resume an existing conversation in Antigravity, pass `conversation_id` in the JSON body or via the `X-AGY-Conversation-ID` header:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_CLIPROXY_API_KEY' \
  -d '{
    "model": "gemini-3.8-flash-high",
    "conversation_id": "YOUR_PREVIOUS_CONVERSATION_ID",
    "messages": [
      {"role": "user", "content": "What was the previous answer?"}
    ]
  }'
```

When resuming an existing session, the plugin only sends the new turn to `agy`, avoiding duplicating previous conversation history already stored in the session.

### Reasoning Effort

Reasoning effort can be set in four ways:

1. Model name alias: `"model": "gemini-3.8-flash-high"` or `"gemini-3.8-flash-low"`
2. Suffix: `"model": "agy/default:high"`
3. Request body: `"reasoning_effort": "high"`
4. Request header: `X-AGY-Effort: high`

## How requests are executed

For a non-streaming request the plugin runs:

```bash
printf "%s" "<prompt>" | agy --output-format json --print-timeout 30m [--conversation <id>] [--effort <effort>] [--model <model>]
```

For a streaming request it runs:

```bash
printf "%s" "<prompt>" | agy --output-format stream-json --print-timeout 30m [--conversation <id>] [--effort <effort>] [--model <model>]
```

The prompt is piped directly to `agy`'s standard input rather than passed as a command-line argument, preventing issues with command-line length limits (`ARG_MAX`) and keeping prompt contents private from system process tables.

## Configuration

| Field | Default | Description |
| --- | --- | --- |
| `binary_path` | `agy` | Official Antigravity CLI executable or absolute path. |
| `workdir` | empty | Working directory given to the `agy` process. Set this to the project that Antigravity is allowed to work in. |
| `print_timeout` | `30m` | Passed to `--print-timeout`. |
| `dangerously_skip_permissions` | `false` | Adds `--dangerously-skip-permissions`. This auto-approves all Antigravity tool permission requests. |
| `sandbox` | `false` | Adds Antigravity's `--sandbox` flag. |
| `reasoning_effort` | empty | Default reasoning effort passed to `--effort` (`low`, `medium`, `high`). |

### Permission safety

`dangerously_skip_permissions` is deliberately **off by default**. When enabled, Antigravity can auto-approve tool calls including file writes and shell commands. Prefer Antigravity's scoped permission rules when possible.

Do not expose a CLIProxyAPI instance using this plugin to untrusted users unless you understand the consequences. A remote prompt may cause the `agy` process to operate on the configured `workdir` according to the permissions of the OS user running CLIProxyAPI and the permission policy configured in Antigravity.

## Compatibility and limitations

### Working directory

`workdir` is currently global for the plugin configuration. If one CLIProxyAPI instance serves several unrelated repositories, set `workdir` to a shared base path or configure per-instance plugins.

### Latency

Because each interaction invokes the official `agy` executable, response startup time is typically ~800ms–1.2s compared to ~150ms for raw persistent HTTP connections.

## Why this approach?

The project deliberately avoids turning an Antigravity account session into a reusable OAuth-backed HTTP provider. **Authentication, account sessions, model access, permissions, and communication with Antigravity remain managed by the official agy CLI.** The plugin executes the official `agy` executable, and `agy` handles authentication, sessions, and upstream service communication.

Google's public Antigravity CLI documentation describes headless mode as the programmatic interface for scripting, CI pipelines, machine-readable JSON/NDJSON output, model selection, permissions, and persistent stdin-driven sessions. This plugin builds on those documented interfaces rather than reverse-engineering the service or capturing session tokens.

## Development

```bash
gofmt -w *.go scripts/*.go
CGO_ENABLED=1 go test -v ./...
make smoke
```

No third-party Go dependencies are required for the plugin itself.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) first. Security reports should follow [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
