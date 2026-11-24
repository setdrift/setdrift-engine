# Choose a comparison

A difference comes from time (a cluster changed) or
from location (two clusters are set up differently); each comparison holds one
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

## What counts as a difference

Both sides are normalized as export reads them, so server defaults, status,
controller state, replicas a HorizontalPodAutoscaler sets and secret values
never show up; Secrets compare by their keys. A value that differs only in
each cluster's own name, region or account is shown apart and not counted
(`--exit-code` too):

| East | West | Counts? |
|---|---|---|
| `111111111111.dkr.ecr.us-east-1.amazonaws.com/app:v1.4.2` | `222222222222.dkr.ecr.us-west-2.amazonaws.com/app:v1.4.2` | No: only the region and account differ |
| `111111111111.dkr.ecr.us-east-1.amazonaws.com/app:v1.4.2` | `222222222222.dkr.ecr.us-west-2.amazonaws.com/app:v1.4.3` | Yes: the tag differs |
| `arn:aws:sqs:us-east-1:111111111111:east-jobs` | `arn:aws:sqs:us-west-2:222222222222:west-jobs` | No: region, account and cluster name |
| `harbor-a.example.com/app:v1.4.2` | `harbor-b.example.com/app:v1.4.2` | Yes: the hosts hold none of the clusters' own values |

The region comes from the cluster's nodes (`topology.kubernetes.io/region`)
on any platform, or from the EKS cluster's ARN. The account comes from EKS
only for now: a GCP project or an Azure subscription is not yet set apart.
