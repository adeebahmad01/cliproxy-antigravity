# Contributing

Contributions are welcome. The main constraint is architectural: this project should remain a wrapper around the **official `agy` CLI**, not become an unofficial Antigravity OAuth or private-API client.

## Before opening a pull request

1. Open an issue first for large behavioral changes.
2. Keep authentication inside `agy`; do not add code that reads, exports, forwards, or reuses Antigravity OAuth/session tokens.
3. Do not add calls to undocumented/private Antigravity service endpoints.
4. Preserve safe defaults. In particular, `--dangerously-skip-permissions` must remain opt-in.
5. Add or update tests for protocol parsing, request conversion, model discovery, and error handling.
6. Update README documentation when configuration or behavior changes.

## Development

Requirements:

- Go 1.23+
- a working C toolchain for `c-shared` builds
- Python 3 for the ABI smoke test

Run:

```bash
gofmt -w *.go
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/cliproxy-antigravity.so .
python3 scripts/abi_smoke.py /tmp/cliproxy-antigravity.so
```

Tests must not require a real Antigravity account. Keep unit and ABI tests deterministic and offline.

## Pull requests

Keep PRs focused. Explain:

- what problem is being solved;
- whether CLIProxyAPI ABI behavior changes;
- whether `agy` command-line arguments change;
- any security/permission impact;
- how the change was tested.

Do not commit generated `.so`, `.dylib`, `.dll`, or `.h` build outputs.

## Coding style

Use standard Go formatting and prefer the standard library unless a dependency clearly reduces complexity or risk. Protocol parsing should fail explicitly rather than silently dropping unsupported input.

## Compatibility

When adding support for a new `agy` capability, prefer documented flags and machine-readable output. If a behavior is version-dependent, feature-detect it or document the minimum supported CLI version instead of assuming every installation has it.
