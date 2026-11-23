# setdrift snapshot

Works with the snapshot files [`export`](export.md) saves, one pair per export in its `snapshots/` directory, `.pb` (protobuf) and `.json`. A snapshot holds everything the export read, normalized and redacted, so it never holds a secret value. See [Snapshots](../snapshots.md).

## Synopsis

```sh
setdrift snapshot [flags]
```

## Subcommands

| Command | Does |
|---|---|
| [`setdrift snapshot json`](snapshot-json.md) | Prints a protobuf snapshot as readable JSON |

To compare a snapshot with a cluster or another snapshot, use [`diff`](diff.md), which reads either form.

## Examples

The subcommands of `snapshot`.

```sh
setdrift snapshot --help
```

## Flags

### `--help`, `-h`

*on/off · default off*

Prints the command's help and its subcommands.

## See also

- [Snapshots](../snapshots.md), what a snapshot holds and why it stays out of Git
- [`diff`](diff.md), comparing a snapshot with a cluster
