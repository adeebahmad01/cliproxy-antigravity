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
- Reasoning effort control via `agy --effort` (`low`, `medium`, `high`, `xhigh`, `max`)
- Multimodal image support via automatic workspace staging
- Client-side OpenAI `tools[]` calling emulation (`tool_calls`)
- Built-in model name aliasing (`gemini-3.8-flash-high`, `gemini-3.8-flash-low`, etc.)
- Antigravity usage-limit tracking (weekly and 5-hour quota per model group) via `agy /usage`
- Dynamic model discovery through `agy models`
- Antigravity token usage mapped to OpenAI-compatible usage fields
- No Antigravity OAuth/token extraction
- No direct calls to private Antigravity services
- No fake CLIProxyAPI Antigravity credential: the plugin's `model_router` capability sends its models straight to its own executor, so no auth file is needed

The plugin registers under the provider key **`agy`**, and supports requests addressed to `agy/*`, `antigravity/*`, or unprefixed models.

Because `agy` manages its own login, CLIProxyAPI has no auth record to schedule for these models. The plugin therefore routes `agy/*` models, its published unprefixed models, and `antigravity/*` models (only while CLIProxyAPI's built-in Antigravity provider has no credentials) to itself. If you do add an auth record with provider `agy`, routing is left to CLIProxyAPI's normal credential path. Requests can arrive through any CLIProxyAPI entry protocol (`/v1/chat/completions`, `/v1/messages`, `/v1/responses`); the host translates them to Chat Completions for the plugin.

## Requirements & Platform Support

- CLIProxyAPI v8 with dynamic-library plugin support (tested against v8.0.72)
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
      reasoning_effort: "high" # optional default: low | medium | high | xhigh | max
      limits_tracking: true # track Antigravity usage limits via agy /usage
      limits_refresh_interval: "1m"
      enforce_limits: false # reject requests with 429 when the model's quota group is exhausted
```

3. Start CLIProxyAPI and inspect `/v1/models`.

The plugin publishes standard built-in model aliases matching CLIProxyAPI conventions:

```text
gemini-3.8-flash-high
gemini-3.8-flash-medium
gemini-3.8-flash-low
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

On `/v1/chat/completions`, save the `conversation_id` returned in the JSON response or in the final stream chunk. Pass it back in the JSON body or via `X-AGY-Conversation-ID` to resume on the same account:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_CLIPROXY_API_KEY' \
  -d '{
    "model": "agy/gemini-3.8-flash-high",
    "conversation_id": "YOUR_PREVIOUS_CONVERSATION_ID",
    "messages": [
      {"role": "user", "content": "What was the previous answer?"}
    ]
  }'
```

The plugin sends only the new turn to `agy`. It won't resume an account's native session on a different account, even if the original account is disabled or out of quota. Legacy raw session IDs still work with the direct, default-HOME login. With managed accounts, omit a legacy ID and send the history once to get a new, account-bound ID.

Clients that always send the full `messages` history can leave out the ID. Each request then starts a fresh native session and follows the configured account routing strategy.

#### Continue on another account

Send the saved ID with `X-AGY-Continue-On-Another-Account: true`, plus either the prior conversation in `messages` or an `agy_replay_summary` and the latest turn:

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_CLIPROXY_API_KEY' \
  -H 'X-AGY-Continue-On-Another-Account: true' \
  -d '{
    "model": "agy/gemini-3.8-flash-high",
    "conversation_id": "YOUR_PREVIOUS_CONVERSATION_ID",
    "agy_replay_summary": "We compared two designs and chose the simpler one.",
    "messages": [{"role": "user", "content": "What should I implement first?"}]
  }'
```

This starts a new native session on a different eligible account. It replays only the context you supply; it does not move files, settings, or native session state. Save the **replacement** `conversation_id` and omit the handoff header on later turns. If no other account can serve the model, the request fails rather than starting over on the original account. The management page manages accounts, not chats; a chat client can use this API to offer its own handoff action.

Account-bound IDs are signed by this proxy. Keep its user-config `cliproxy-antigravity/conversation.key` file across reinstalls or moves if you need existing IDs to work, along with the original account homes. CLIProxyAPI currently activates only one scheduler plugin; Antigravity's scheduler must be active for reliable account selection on native resumes and handoffs.

### Reasoning Effort

Reasoning effort can be configured via:

1. **Embedded Model Suffix**: Selecting models like `gemini-4-hydrogen-xhigh`, `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, or `gemini-3.8-flash-low` automatically splits the last part (`low`, `medium`, `high`, `xhigh`, `max`) into reasoning effort and routes to the clean base model (`gemini-4-hydrogen`, `gemini-3.8-flash`).
2. **Base Model Selection**: Selecting clean base models directly (e.g. `agy/gemini-4-hydrogen`, `agy/gemini-3.8-flash`, `agy/gpt-oss-120b`) and configuring reasoning effort via UI selectors or request body (`"reasoning_effort": "low" | "medium" | "high" | "xhigh" | "max"`).
3. **Explicit Overrides**: CLIProxyAPI thinking suffixes such as `agy/gemini-4-hydrogen(xhigh)`, or colon suffixes such as `agy/gemini-4-hydrogen:max`, `agy/gemini-3.8-flash:high`, and `agy/default:xhigh`.
4. **Headers & Metadata**: Request headers (`X-AGY-Effort`, `X-Reasoning-Effort`) or metadata fields.
5. **Intelligent Fallbacks**: If no effort is specified for models requiring an effort flag, sensible defaults (such as `high` for Gemini and `medium` for GPT-OSS) are applied automatically, while models that do not support reasoning effort (such as Claude) safely omit the flag. Requested levels must be supported by the target model.

## How requests are executed

For a non-streaming request the plugin runs:

```bash
printf "%s" "<prompt>" | agy --output-format json --print-timeout 30m --disable-slash-commands [--conversation <id>] [--effort <effort>] [--model <model>]
```

For a streaming request it runs:

```bash
printf "%s" "<prompt>" | agy --output-format stream-json --print-timeout 30m --disable-slash-commands [--conversation <id>] [--effort <effort>] [--model <model>]
```

The prompt is piped directly to `agy`'s standard input rather than passed as a command-line argument, preventing issues with command-line length limits (`ARG_MAX`) and keeping prompt contents private from system process tables.

`--disable-slash-commands` is always passed so that client text such as `/usage` or a plugin command is treated as a plain prompt and never expanded into an `agy` slash command or skill.

## Configuration

| Field | Default | Description |
| --- | --- | --- |
| `binary_path` | `agy` | Official Antigravity CLI executable or absolute path. |
| `workdir` | empty | Working directory given to the `agy` process. Set this to the project that Antigravity is allowed to work in. |
| `print_timeout` | `30m` | Passed to `--print-timeout`. |
| `dangerously_skip_permissions` | `false` | Adds `--dangerously-skip-permissions`. This auto-approves all Antigravity tool permission requests. |
| `sandbox` | `false` | Adds Antigravity's `--sandbox` flag. |
| `reasoning_effort` | empty | Default reasoning effort passed to `--effort` (`low`, `medium`, `high`, `xhigh`, `max`). |
| `limits_tracking` | `true` | Track Antigravity usage limits by running `agy /usage` (no tokens consumed). |
| `limits_refresh_interval` | `1m` | How long cached limits stay fresh (minimum `10s`). Limits are also refreshed after any quota error. |
| `enforce_limits` | `false` | Reject requests with HTTP 429 (`agy_quota_exhausted`, retryable) when the model's quota group is known to be exhausted, without starting `agy`. Off by default because paid plans may continue on AI credits after a limit is reached. |
| `accounts_dir` | empty | Directory holding one agy home per account. Accounts added from the panel go here (`~/.agy-accounts` when empty). See [Multiple accounts](#multiple-accounts). |

### Usage limits

Antigravity groups models (for example *Gemini Models* and *Claude and GPT models*) and gives every group a weekly limit and a 5-hour limit. With `limits_tracking` enabled the plugin reads them from `agy /usage` and surfaces them in three places:

- **Management API** — `GET /v0/management/agy/limits` (requires the CLIProxyAPI management key; add `?refresh=1` to bypass the cache):

  ```json
  {
    "provider": "agy",
    "tracking": true,
    "fetched_at": "2026-11-15T14:20:00Z",
    "groups": [
      {
        "name": "Gemini Models",
        "description": "Models within this group: Gemini 3.8 Flash",
        "buckets": [
          {"id": "gemini-weekly", "window": "weekly", "remaining_fraction": 0.93, "reset_time": "2026-11-21T06:20:27Z"},
          {"id": "gemini-5h", "window": "5h", "remaining_fraction": 1, "reset_time": "2026-11-15T19:16:01Z"}
        ]
      }
    ]
  }
  ```

- **Response headers** on every completion, for the requested model's group: `X-AGY-Limit-Group`, `X-AGY-Limit-Weekly-Remaining`, `X-AGY-Limit-Weekly-Reset`, `X-AGY-Limit-5h-Remaining`, `X-AGY-Limit-5h-Reset`. CLIProxyAPI forwards them to clients only when `requests.passthrough-headers: true` is set (`passthrough-headers: true` in legacy config files).
- **CLIProxyAPI quota API** — the plugin implements the `quota_provider` capability (`quota.fetch`), so the host's `/v0/management/quota/*` and `/v0/management/plugins/cliproxy-antigravity/quota` endpoints work for any auth record whose provider is `agy` (they require an `auth_index`).

Quota errors from `agy` are returned as HTTP 429 with `retryable` set from Antigravity's own error report, so CLIProxyAPI can cool down and retry correctly. When `enforce_limits` rejects a request, the error also carries `retry_after_seconds` until the bucket resets, which CLIProxyAPI turns into a `Retry-After` header.

Errors use the codes CLIProxyAPI's routing pipeline understands: malformed client payloads are `request_scoped` (HTTP 400, no credential cooldown), and models `agy` does not recognise are `model_not_found` (HTTP 404). Streaming failures are reported through `host.stream.close` with a structured `error_detail`, so they keep their HTTP status instead of collapsing into a generic 500.

### Multiple accounts

agy keeps its login, settings and conversations in `$HOME/.gemini/antigravity-cli`. To use several Antigravity accounts, give each its own home directory. The plugin runs `agy` with `HOME` (and `USERPROFILE` on Windows) set to the home of the account CLIProxyAPI selected.

#### Adding accounts from the CLIProxyAPI panel

The plugin adds an **Antigravity accounts** entry to the management panel's sidebar. It lists every account with its usage limits and lets you add, disable and remove accounts:

1. Click **Add account**, then **Open Google sign-in**, and sign in with the account to add.
2. Google then shows an authorization code. Copy it and paste it into the panel.

An **Antigravity CLI** card also appears on the panel's OAuth login page. It runs the same flow, but the panel's callback box there expects the full `antigravity.google/oauth-callback` URL rather than just the code, so the sidebar page is the easier way.

Behind the scenes the plugin starts the official `agy` sign-in in a new home under `accounts_dir` (default `~/.agy-accounts`) and types the pasted code into it. The tokens are exchanged and stored by `agy` itself; the plugin never sees them. CLIProxyAPI then saves an auth file pointing at the new home. Adding accounts from the panel requires Linux or macOS.

The accounts page opens without a key prompt. A panel build with accounts-page session support can reuse the existing management-panel session even with **Remember password** off. Without that support, the accounts page requires a remembered login on the same origin; it accepts the current CPAMC `enc::v2::` storage format as well as the legacy format. Without an authenticated connection, the page shows the routing guide but does not load private account data or allow changes.

Use **Hide email addresses** before sharing your screen. The page shows numbered accounts and hides auth filenames and home paths, which can also contain email addresses. The preference is kept in your browser; it does not redact the management API or change how accounts are stored.

#### Adding accounts by hand

1. Sign in to each account in its own home:

   ```bash
   mkdir -p ~/.agy-accounts/work ~/.agy-accounts/personal
   HOME=~/.agy-accounts/work agy       # complete the login, then exit
   HOME=~/.agy-accounts/personal agy
   ```

2. Point the plugin at the directory:

   ```yaml
   plugins:
     configs:
       cliproxy-antigravity:
         accounts_dir: "~/.agy-accounts"
   ```

When the plugin is registered or reconfigured, it writes an `antigravity-<email>.json` auth file to CLIProxyAPI's `auth-dir` for every signed-in account that doesn't have one yet:

```json
{
  "type": "agy",
  "email": "work@example.com",
  "label": "work@example.com",
  "home": "/home/me/.agy-accounts/work",
  "disabled": false
}
```

You can also write these files yourself instead of using `accounts_dir`. An agy auth file without `home` uses the CLIProxyAPI process's own HOME. When `accounts_dir` is set, that default account gets an auth file only if it is signed in.

For requests without a native conversation ID, CLIProxyAPI chooses an eligible account using `routing.strategy`. `round-robin` (the default) rotates requests between eligible accounts of the same priority; `fill-first` prefers the first eligible account. On hosts that support `weighted-round-robin`, set that strategy and add a numeric `weight` to each auth file (for example, `3` and `1` for a roughly 75/25 request split). This divides requests, not tokens. Usage limits and quota cards are separate per account.

Things to know:

- New chats follow your routing strategy. [Native resumes](#conversation-resumption) stay with the original account unless your client explicitly replays the chat on another account.
- **Disable** keeps an auth record out of rotation without removing its sign-in. With no enabled agy auth records, the plugin can still use the sign-in in the CLIProxyAPI process's HOME. Disable the plugin to stop serving agy requests entirely.
- **Remove** deletes the auth file but keeps the signed-in agy home. If `accounts_dir` scans that home, a later plugin reconfiguration can import it again.
- Model discovery (`agy models`) and `GET /v0/management/agy/limits` use the CLIProxyAPI process's HOME.
- Each home has its own agy settings, MCP servers and plugins. The `workdir` is shared.

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
