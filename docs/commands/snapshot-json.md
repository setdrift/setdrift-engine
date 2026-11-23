# setdrift snapshot json

Prints a protobuf snapshot as readable JSON, the same JSON `export` writes beside each `.pb`. Each resource's manifest is inlined as an object, so the output works with `jq`. It holds no secret values, since snapshots never do.

## Synopsis

```sh
setdrift snapshot json FILE [flags]
```

## Inputs

`FILE` is a protobuf snapshot, a `.pb` file. The `.json` beside it is already this JSON.

| Argument | Example |
|---|---|
| A snapshot `export` saved | `setdrift snapshot json setdrift-export/snapshots/2026-11-16T090000Z-prod.pb` |
| A snapshot saved elsewhere with `--snapshot` | `setdrift snapshot json ~/snapshots/2026-11-16T090000Z-prod.pb` |

## Examples

A snapshot as JSON.

```sh
setdrift snapshot json setdrift-export/snapshots/2026-11-16T090000Z-prod.pb
```

```
{
  "capturedAt": "2026-11-16T09:00:00Z",
  "cluster": {
    "name": "prod"
  },
  "producer": {
    "name": "setdrift-cli",
    "version": "v0.1.0"
  },
  "resources": [
    ...
```

Every Deployment's name and who manages it, with `jq`.

```sh
setdrift snapshot json prod.pb | jq -r '.resources[] | select(.kind == "Deployment") | "\(.namespace)/\(.name) \(.provenance.source)"'
```

The cluster profile alone.

```sh
setdrift snapshot json prod.pb | jq .inventory.profile
```

## Flags

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

## See also

- [Snapshots](../snapshots.md)
- [`snapshot`](snapshot.md)
- [`diff`](diff.md), which reads `.pb` and `.json` snapshots directly
