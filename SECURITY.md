# Security Policy & Execution Sandboxing

`cliproxy-antigravity` is designed with strict security and process isolation standards for execution within the CLIProxyAPI host environment.

---

## 1. Architectural Purpose

`cliproxy-antigravity` deliberately operates as an external CLI runner for Google's official Antigravity CLI (`agy`).

**Why this architecture is chosen:**
- **Zero Token Extraction**: The plugin strictly avoids reading, persisting, exporting, or managing Antigravity OAuth tokens, refresh tokens, or session credentials.
- **Official Client Isolation**: Authentication, session state, access tokens, and API protocol handshakes remain contained entirely inside the official `agy` binary and Google's designated user cache.
- **Full Parity**: Ensures users get identical model behavior, session management, and official account handling without exposing credentials to third-party proxies.

---

## 2. Process Lifecycle & Orphan Prevention

Spawning host processes requires rigorous lifecycle management to prevent process leaks and zombie processes. `cliproxy-antigravity` enforces the following guarantees:

- **Dedicated Process Groups**: Every spawned `agy` execution is assigned to its own dedicated process group (`Setpgid: true` on POSIX/Unix systems; `CREATE_NEW_PROCESS_GROUP` on Windows).
- **Proactive Context Cancellation**: The execution is tied directly to the HTTP request context. Upon request completion, client disconnect, or timeout, Go's `cmd.Cancel` hook issues a termination signal to the entire process group (`SIGKILL` to `-pgid` on Unix), ensuring that neither `agy` nor any background workers/sub-tools linger.
- **Central Process Registry**: Every active command is tracked in a thread-safe registry. When the plugin is unloaded or CLIProxyAPI shuts down (`cliproxyPluginShutdown`), all registered process trees across the host are immediately and completely reaped.
- **Pipe Drain Deadlock Prevention**: Commands enforce a strict `WaitDelay` to guarantee that abandoned I/O pipes cannot cause process hangs.

---

## 3. Command-Path & Binary Validation

To prevent command injection, binary substitution, or PATH manipulation attacks:

- **Strict Binary Allowlist**: The `binary_path` setting strictly requires the executable base name to be `agy` or `antigravity` (or `.exe`). Any attempt to configure arbitrary executables (e.g. `/bin/sh`, `curl`, `cmd.exe`, `powershell.exe`) is rejected at configuration parsing time.
- **Metacharacter Rejection**: Paths containing shell metacharacters (`;`, `&`, `|`, `$`, `` ` ``, `<`, `>`, `\n`, `\r`) are rejected unconditionally.
- **No Intermediate Shell**: Commands are executed directly via `exec.CommandContext` without invoking an intermediate shell interpreter (`sh -c` or `cmd.exe /c`), preventing argument or shell injection vulnerabilities.
- **Stdin Streaming**: User prompts and chat history payloads are passed exclusively via standard input (`stdin`), never via command-line arguments, preventing exposure in process listings (`ps`) and bypassing OS `ARG_MAX` length constraints.

---

## 4. Filesystem & Working Directory Guardrails

- **Canonical Path Resolution**: Configured working directories (`workdir`) are cleaned and resolved to absolute paths (`filepath.Clean` and `filepath.Abs`) to prevent directory traversal (`../`).
- **Directory Verification**: The resolved path is verified to exist and be a valid directory before execution.
- **Scoped Image Cache**: Decoded base64 images are written strictly to `<workdir>/.cliproxy_cache/images/` using SHA-256 content hashes, avoiding filesystem contamination.

---

## 5. Sandboxing & Permission Model

- **Safe Defaults**: `dangerously_skip_permissions` defaults to `false`.
- **Sandbox Mode**: When `sandbox: true` is configured in `config.yaml`, the `--sandbox` flag is passed to `agy`, enforcing strict workspace boundaries on internal tool execution.

---

## Reporting Vulnerabilities

If you discover a potential security issue in this plugin, please report it via private GitHub Security Advisories on [adeebahmad01/cliproxy-antigravity](https://github.com/adeebahmad01/cliproxy-antigravity/security/advisories/new) or by opening an issue on the repository.
