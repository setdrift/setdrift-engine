# End-to-end tests on EKS

A disposable EKS Auto Mode cluster to run `setdrift export` and `verify`
against real clusters. It is created on demand and torn down automatically
after two hours without use.

| File | What it is |
|---|---|
| [eks-cluster.yaml](eks-cluster.yaml) | The cluster stack: VPC, cluster and node roles, metrics-server, and access entries for an admin, a cluster-wide reader and a one-namespace reader, plus a Pod Identity role for External Secrets Operator |
| [reaper.yaml](reaper.yaml) | Deployed once per account and region. A Lambda that runs every 15 minutes, deletes cluster stacks idle past `IdleMinutes`, and cleans up what the cluster's controllers left outside CloudFormation (load balancers, security groups, network interfaces, EBS volumes, test secrets) |
| [cluster.sh](cluster.sh) | Creates, waits for, keeps alive and tears down the cluster |
| [restore.sh](restore.sh) | The restore experiment: exports the cluster and follows the RESTORE.md it writes into a second e2e cluster, then checks the result (`make e2e-restore`) |
| [populate.sh](populate.sh) | Installs Argo CD, Flux and External Secrets Operator with real apps, a Helm release and the [fixtures](fixtures) |

## Use

```sh
export AWS_PROFILE=setdrift      # credentials with admin rights in the test account
make e2e-setup                   # once: deploy the reaper
make e2e-populate                # create the cluster if needed (~15 min), wait, populate
setdrift export --context setdrift-e2e-137-admin --cluster-name setdrift-e2e-137 -o out
setdrift verify --context setdrift-e2e-137-admin out
make e2e-status                  # state and time until teardown
make e2e-down                    # tear down now
```

`make e2e-up` starts creating the cluster in the background and returns at
once, so it can be started before it is needed.

## The restore experiment

`make e2e-restore` checks that RESTORE.md works: it exports the e2e
cluster with `--aws`, creates a second cluster (`<cluster>-restore`), runs
the playbook's commands there in order, and checks that Argo CD, Flux and
External Secrets Operator run, that both volumes kept their data (a marker
written on the source before the snapshot), that no Pod is failing, and
that `setdrift verify` passes and notes the different cluster. It fails on
any command that fails, other than "already exists" for settings the
target's own stack made.

Where the playbook offers a choice it follows the platform's playbooks
(platform/aws/eks, platform/aws) and applies the EKS set, restores Secrets
through External Secrets Operator (option C: `setdrift secrets export`
copies the values into Secrets Manager, and the restored Secrets must match
the source's by hash), copies other objects holding secret values from the
source (option A), and copies volumes from snapshots. It skips the IRSA
trust update (the fixture's role is in an account no one can read), and the
options that need real secret values typed in. The fixture's local Helm chart has
no repository, so it is generated again. Afterwards the source's volumes
get their reclaim policy back, the snapshots and test secrets are deleted and
the target is torn down (`E2E_KEEP=1` keeps it). About half an hour, mostly creating the
second cluster. Run it before releases.

## Idle teardown

"Use" is recorded in the SSM parameter `/setdrift-e2e/<cluster>/last-used`
by every `cluster.sh` command except `status`, and by `populate.sh`.
`kubectl` and `setdrift` alone do not record use: run
`test/e2e/cluster.sh touch` during long manual sessions. The reaper deletes
the stack once the parameter is older than `IdleMinutes` (default 120), at
most 15 minutes late. Deleting the stack ends all cost except the reaper's,
which is a few Lambda invocations an hour.

The cluster stack is created and deleted with the reaper's CloudFormation
service role, so a deletion the reaper starts has the permissions to finish.
If a deletion fails (usually on a security group a load balancer controller
left in the VPC), the next run removes what is left in the VPC and retries.

Deleting a cluster leaves its dynamically provisioned EBS volumes; the reaper
deletes them (and their data) on its next run once no stack of that name
exists. To keep data across a teardown, snapshot the volumes first, as
RESTORE.md's source-side step does.

## Settings

`E2E_VERSION` (1.37; also 1.36 and 1.35 for EKS n-2), `E2E_CLUSTER`
(`setdrift-e2e-<version>`), `E2E_REGION` (`AWS_REGION`, else us-east-2),
`E2E_ADMIN_ARN` (the caller), `E2E_IDLE_MINUTES` (120; applied by
`make e2e-setup`). Cluster stack names must start with `setdrift-e2e-`: only
those are reaped.

## Cost

While a cluster exists: the EKS control plane, the Auto Mode nodes the
workloads need, a NAT gateway and the EBS volumes, a few dollars a day.
