# Security policy

## Supported versions

Until the project reaches a stable release, security fixes are made on the latest `main` branch and the latest tagged release when practical.

## Reporting a vulnerability

Please do **not** open a public issue for a vulnerability involving credential exposure, command execution outside the configured workspace, permission bypass, arbitrary library loading, or injection into the `agy` process.

Use GitHub's private vulnerability reporting / Security Advisory flow for this repository when available. Include:

- affected commit or version;
- operating system and architecture;
- CLIProxyAPI version/build;
- Antigravity CLI version;
- minimal reproduction steps;
- expected vs. actual behavior;
- impact and any suggested mitigation.

## Security boundaries

This plugin intentionally does not read Antigravity OAuth/session tokens or call private Antigravity endpoints. Authentication is delegated to the official `agy` executable.

The plugin **does** execute `agy` as the same OS user running CLIProxyAPI. Antigravity may read/write files or execute commands according to its own permission configuration. Treat `workdir`, CLIProxyAPI API keys, and the `dangerously_skip_permissions` option as security-sensitive configuration.
