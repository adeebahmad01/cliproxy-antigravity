## Summary

Describe the change and the problem it solves.

## Testing

- [ ] `gofmt -w *.go`
- [ ] `go test ./...`
- [ ] `CGO_ENABLED=1 go build -buildmode=c-shared ...`
- [ ] ABI smoke test, when ABI/executor behavior changed

## Architecture / security

- [ ] Authentication remains inside the official `agy` CLI.
- [ ] No Antigravity OAuth/session tokens are read, exported, or proxied.
- [ ] No undocumented/private Antigravity endpoints are called.
- [ ] Permission-impacting changes are documented and preserve safe defaults.
- [ ] README/configuration documentation is updated where needed.
