# Install

One command for each tool, for Intel/AMD and ARM:

| Tool | Command |
|---|---|
| Homebrew (macOS, Linux) | `brew install setdrift/tap/setdrift` |
| curl (macOS, Linux, WSL) | `curl -fsSL https://setdrift.io/install.sh \| sh` |
| PowerShell (Windows) | `irm https://setdrift.io/install.ps1 \| iex` |
| Go 1.27+ (any OS) | `go install setdrift.io/setdrift-engine/cmd/setdrift@latest` |

## The install scripts

The curl and PowerShell scripts download the latest release for your OS and
CPU, check it against the release's `checksums.txt`, and install `setdrift`:

- `install.sh` installs to `/usr/local/bin` when it can write there, else
  `~/.local/bin`.
- `install.ps1` installs to `%LOCALAPPDATA%\Programs\setdrift` and adds it to
  your user PATH.

`SETDRIFT_VERSION=v0.1.0` installs a given release, and
`SETDRIFT_INSTALL_DIR` sets the directory. Neither script needs administrator
rights.

## By hand

Every archive is on the
[releases](https://github.com/setdrift/setdrift-engine/releases) page: a
`.tar.gz` for macOS and Linux, a `.zip` for Windows. Unpack it and put
`setdrift` on your PATH.

## Checking a release

GitHub attests where each archive was built:

```sh
gh attestation verify <archive> --repo setdrift/setdrift-engine
```

## As a kubectl plugin

`setdrift` also runs as `kubectl setdrift` when installed as
`kubectl-setdrift` on the PATH (`make plugin` builds it).

## Check it works

```sh
setdrift version
```

To upgrade later, see [Upgrade](upgrade.md).
