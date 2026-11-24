# setdrift version

Prints the version of `setdrift` and the commit it was built from.

## Synopsis

```sh
setdrift version [flags]
```

## Examples

```sh
setdrift version
```

```
setdrift v0.1.0 (cbf4da0)
```

A build from a source checkout shows the version Go works out from Git instead, like `v0.1.1-0.20261123222108-2f26c6810ba3+dirty`, with the commit it was built from.

## Flags

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

## See also

- [Install](../install.md)
- [Upgrade](../upgrade.md)
- [Releasing](../releasing.md), how versions are numbered
