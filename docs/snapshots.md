# Snapshots

Each export writes a snapshot of everything it read, as
protobuf (`.pb`) and JSON (for reading, or `jq`; `setdrift diff` reads either): every
resource, normalized and redacted, including what the repository leaves out
(the platform's own objects, and Jobs that already finished, which would
run again if applied), who manages each, who wrote each and when (its field managers, names and times only, never the fields they own), every finding, and the
inventory (volumes, Helm releases, load balancers, nodes, and with `--aws`
the AWS settings). They hold no secret values and are readable by you only.
Exports into the same directory build a history, one pair per export. The
export's `.gitignore` keeps `snapshots/` out of Git: they change with every
export, and the repository holds what is applied. `--snapshot` and
`--snapshot-json` take a file or a directory; an existing file is replaced
only with `--force`.
