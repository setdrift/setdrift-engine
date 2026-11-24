# Upgrade

Upgrade `setdrift` with the same tool you installed it with.

| Installed with | Upgrade to the latest release |
|---|---|
| Homebrew (macOS, Linux) | `brew upgrade setdrift/tap/setdrift` |
| curl (macOS, Linux, WSL) | `curl -fsSL https://setdrift.io/install.sh \| sh` |
| PowerShell (Windows) | `irm https://setdrift.io/install.ps1 \| iex` |
| Go 1.27+ (any OS) | `go install setdrift.io/setdrift-engine/cmd/setdrift@latest` |
| By hand | Download the new archive from the [releases](https://github.com/setdrift/setdrift-engine/releases) page and replace `setdrift` on your PATH with the one inside |

The curl and PowerShell scripts install over the old `setdrift` when it is in the same directory. If you installed with `SETDRIFT_INSTALL_DIR`, set it again, else the new one may go to another directory and the old one keep running.

## A given version

Set `SETDRIFT_VERSION` for the install scripts, or name the version with Go.

```sh
curl -fsSL https://setdrift.io/install.sh | SETDRIFT_VERSION=v0.2.0 sh
```

```powershell
$env:SETDRIFT_VERSION = 'v0.2.0'; irm https://setdrift.io/install.ps1 | iex
```

```sh
go install setdrift.io/setdrift-engine/cmd/setdrift@v0.2.0
```

## Before you upgrade

Before v1.0.0, a minor release (v0.1.x to v0.2.0) may change flags or output. Read the "Before you upgrade" notes of each release you skip, on the [releases](https://github.com/setdrift/setdrift-engine/releases) page.

## Check it worked

```sh
setdrift version
```

If it still shows the old version, `which setdrift` (`Get-Command setdrift` in PowerShell) shows which one is on your PATH first.
