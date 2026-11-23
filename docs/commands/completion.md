# setdrift completion

Prints the script that gives your shell tab completion for `setdrift`, its commands and flags. Homebrew installs it for you, so this is for other installs.

## Synopsis

```sh
setdrift completion [flags]
```

## Inputs

One subcommand, the shell to complete in.

| Shell | Load it for this session | Example of loading it for every session |
|---|---|---|
| bash | `source <(setdrift completion bash)` | `setdrift completion bash > /etc/bash_completion.d/setdrift` |
| zsh | `source <(setdrift completion zsh)` | `setdrift completion zsh > "${fpath[1]}/_setdrift"` |
| fish | `setdrift completion fish \| source` | `setdrift completion fish > ~/.config/fish/completions/setdrift.fish` |
| PowerShell | `setdrift completion powershell \| Out-String \| Invoke-Expression` | add that line to your `$PROFILE` |

Each shell's own help says more, as in `setdrift completion zsh --help`. In zsh, completion must be turned on once with `autoload -U compinit; compinit` if it isn't already.

## Examples

zsh, for every new shell.

```sh
setdrift completion zsh > "${fpath[1]}/_setdrift"
```

bash, for the current session.

```sh
source <(setdrift completion bash)
```

## Flags

### `--help`, `-h`

*on/off · default off*

Prints the command's help and the shells it supports.

## See also

- [Install](../install.md)
