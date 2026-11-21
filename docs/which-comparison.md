# Which comparison?

A difference comes from time (a cluster changed) or
from place (two clusters are set up differently); each comparison holds one
fixed, so you can tell which:

| Command | Answers |
|---|---|
| `setdrift diff <snapshot> prod` | What changed in prod since the snapshot, and who changed it (drift, incidents) |
| `setdrift diff staging prod` | How two clusters differ right now (before a release, DR readiness, blue/green, regions) |
| `setdrift verify <export>` | Whether applying the export would change the cluster: is the export right, did the restore work |

Comparing an old export of one cluster with another cluster mixes the two:
each difference could be either. `diff` accepts any combination of contexts,
snapshots and directories; two contexts is the same as exporting both now
and comparing the snapshots.
