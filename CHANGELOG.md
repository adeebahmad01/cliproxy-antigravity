# Changelog

All notable changes to this project will be documented here.

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
