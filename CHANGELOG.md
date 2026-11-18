# Changelog

All notable changes to this project will be documented here.

## [Unreleased]

## [0.3.2] - 2026-11-19

### Fixed

- Requests where `agy` spends its whole turn on built-in tools (`read_file`, `run_command`) instead of answering no longer return an empty completion with `finish_reason=stop`: the bridge now fails fast with a retryable 502 `empty_assistant_response` for non-streaming requests. For streaming requests, the stream closes with the same `empty_assistant_response` error after the response has started, so clients can retry or fall back to another model instead of looping on an identical empty reply. The bridge preamble also tells `agy` it has no filesystem/shell/network tools for the request and must never answer empty.

### Changed

- CI: lint (gofmt + vet), multi-OS tests, cross-builds, CodeQL scanning, and Dependabot updates for GitHub Actions.

## [0.3.1] - 2026-11-16

### Fixed

- Adding an account on macOS failed with "failed to generate authorization url": `agy` ignores `$BROWSER` there and wraps the sign-in link on screen, so the plugin saw a truncated URL with no `state`. The link is now read from `agy`'s OSC 8 hyperlink, which holds the complete URL.

## [0.3.0] - 2026-11-16

### Added

- Multiple Antigravity accounts: an `agy` auth file can set `home`, and the plugin runs `agy` with `HOME`/`USERPROFILE` set to that directory, so CLIProxyAPI rotates and cools down accounts per credential. Usage limits and `quota.fetch` are tracked per account.
- `accounts_dir` option: every signed-in account home in the directory gets its own auth file in `auth-dir`.
- "Antigravity accounts" page in the CLIProxyAPI management sidebar: lists accounts with their usage limits, and adds, disables and removes accounts.
- `auth_provider` capability: an "Antigravity CLI" card on the management OAuth page. Both it and the sidebar page run the official `agy` sign-in in a new account home and type the pasted Google code into it (Linux and macOS).
- Management route `GET /v0/management/agy/accounts`.

## [0.2.2] - 2026-11-16

### Added

- Automatic binary resolution across standard installation directories when `binary_path` is not in `$PATH` (e.g. non-interactive launchd/GUI environments on macOS, Apple Silicon `/opt/homebrew/bin`, `~/.local/bin/agy`, `~/bin/agy`, user app data, snap, WinGet, Scoop, and Chocolatey).
- User home directory (`~`) tilde expansion support in `binary_path` and `workdir` configurations.

## [0.2.1] - 2026-11-16

### Added

- Auto-synchronization of Antigravity auth card to CLIProxyAPI's `auth-dir`:
  - Automatically detects the active Antigravity OAuth identity from `~/.gemini/antigravity-cli/antigravity-oauth-token`.
  - Provisions `antigravity-<email>.json` with type `agy` in `auth-dir` (or `~/.cli-proxy-api`) during plugin register and reconfigure.
  - Automatically heals empty or missing credential files so CLIProxyAPI's management UI recognizes and displays the Antigravity provider card alongside Claude, Codex, and Opencode-go, complete with live quota management and refresh actions.

## [0.2.0] - 2026-11-16

Requires CLIProxyAPI v8 (tested against v8.0.72).

### Added

- Antigravity usage-limit tracking from `agy /usage` (weekly and 5-hour quota per model group), cached and refreshed every `limits_refresh_interval` and after quota errors:
  - Management API route `GET /v0/management/agy/limits`.
  - `X-AGY-Limit-*` response headers for the requested model's quota group.
  - CLIProxyAPI `quota_provider` capability (`quota.identifier`, `quota.describe`, `quota.fetch`).
  - Optional `enforce_limits` to reject requests for an exhausted group with a retryable 429 before starting `agy`.
- New config fields: `limits_tracking`, `limits_refresh_interval`, `enforce_limits`.

### Changed

- Compatible with CLIProxyAPI v8 (tested against v8.0.72):
  - Declares the `model_router` capability and routes `agy` models to its own executor. v8 has no auth record for `agy`, so requests previously failed with `auth_not_found: no auth available (providers=agy)`. An explicit `agy` auth record keeps the normal credential path.
  - Errors use CLIProxyAPI's routing codes: client payload faults are `request_scoped` (no credential cooldown), unknown models are `model_not_found`, and `enforce_limits` rejections include `retry_after_seconds`.
  - Streaming failures send a structured `error_detail` on `host.stream.close`, keeping their HTTP status (for example 429 for quota errors).
  - Config fields publish their defaults, and `reasoning_effort` is an `enum` field, for the v8 management UI.
  - CLIProxyAPI thinking suffixes such as `agy/gemini-3.8-flash(high)` are honoured when the host passes them through.
- Release builds pin the baseline CPU instruction set (`GOAMD64=v1`, `-march=x86-64` / `GOARM64=v8.0`, `-march=armv8-a`) so the library loads on older CPUs.
- README uses the v8 config key `requests.passthrough-headers`.

### Fixed

- Empty plugin config values (`reasoning_effort: ""`, as written by management clients) keep the default instead of failing to load the plugin.
- Client prompts beginning with `/` were expanded as `agy` slash commands (for example `/usage` or plugin/skill commands); `--disable-slash-commands` is now always passed.
- Removed the `claude-3-7-sonnet-thought` built-in alias, which `agy` rejects as an unknown model.
- Errors are classified from `agy`'s structured `AGY_ERROR` report: quota errors carry `retryable`, unknown models return 404 instead of 502, print and command timeouts return 504, and duplicated or `exit status N` messages are cleaned up.
- The plugin now sets the `retryable` flag on error envelopes.
- Streaming with client tools: a partial tool-call fence (such as a lone `` ` ``) was streamed as raw text before being recognised; text is now held back until the reply can be classified.
- Streaming tool calls with `stream_options.include_usage` lost `finish_reason: "tool_calls"` because it shared a chunk with usage (usage chunks have empty `choices`).
- Staged images are written owner-only (`0700` directory, `0600` files) instead of world-readable.

## [0.1.3] - 2026-09-23

### Added

- Human-readable model display names: automatically formats and humanizes model IDs (e.g. `Gemini 3.8 Flash`, `Claude 3.7 Sonnet`, `GPT-OSS 120B`) across both built-in static models and dynamically discovered `agy models`.
- Suffix-based reasoning effort splitting: automatically extracts `-high`, `-medium`, `-low` suffixes from model request names and maps them to `--effort` while routing to the clean base model name.
- Explicit base model support: enables selecting clean base model IDs (such as `agy/gemini-3.8-flash`) while specifying reasoning effort independently via request body, headers (`X-AGY-Effort`), or colon notation (`agy/default:high`).
- Intelligent reasoning effort fallbacks: automatically applies default reasoning effort (e.g. `high` for Gemini, `medium` for GPT-OSS) for models requiring an effort flag, while omitting `--effort` for models that do not support reasoning effort (e.g. Claude).

## [0.1.2] - 2026-09-22

### Added

- Full-platform build matrix: precompiled Windows binary releases (`windows_amd64.zip` / `cliproxy-antigravity.dll`) alongside Linux (`amd64`, `arm64`) and macOS (`amd64`, `arm64`).
- Strict security hardening and process isolation:
  - Process group lifecycle management (`Setpgid: true` on Unix, `CREATE_NEW_PROCESS_GROUP` on Windows) and Go 1.23+ `cmd.Cancel` hooks ensuring immediate termination of entire process trees upon context timeout or disconnect.
  - Binary allowlist validation: strictly validates `binary_path` against `agy` / `antigravity` executables, preventing arbitrary command invocation and PATH manipulation.
  - Path traversal and filesystem guardrails for configured `workdir`.
  - Added dedicated [SECURITY.md](SECURITY.md) covering the plugin's process sandboxing architecture, threat model, and vulnerability reporting.

## [0.1.1] - 2026-09-20

### Added

- Stdin prompt streaming: requests are now fed to `agy` standard input instead of command-line arguments, removing OS `ARG_MAX` length constraints and keeping prompts private from process listings.
- Multi-turn conversation resumption: pass `conversation_id` in request body or `X-AGY-Conversation-ID` header to resume ongoing sessions with `agy --conversation <id>`.
- Reasoning effort control: map `reasoning_effort` request field, `X-AGY-Effort` header, or model suffix (`agy/model:effort`) to `agy --effort` (`low`, `medium`, `high`).
- Multimodal image staging: base64 images passed via OpenAI `image_url` are automatically decoded, cached in `.cliproxy_cache/images/`, and referenced for `agy`.
- Client-side OpenAI tool calling emulation: parses tool definitions, instructs the model, and translates `tool_calls` responses with `finish_reason: "tool_calls"`.
- Built-in model aliases matching CLIProxyAPI native names: `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`, and `claude-3-7-sonnet-thought`.
- Configurable default reasoning effort in `config.yaml`.

## [0.1.0] - 2026-09-20

### Added

- Initial CLIProxyAPI standard dynamic-library plugin implementation (`cliproxy-antigravity`).
- Official `agy` CLI execution for non-streaming and streaming Chat Completions.
- Dynamic model discovery through `agy models` with `agy/default` fallback.
- OpenAI-compatible usage mapping and SSE streaming.
- Safe permission defaults and configurable sandbox/workdir/timeout options.
- Automated release packaging generating platform zip archives and `checksums.txt` compliant with CLIProxyAPI Plugin Store standards.
- Public contribution, security, licensing, and CI documentation.
