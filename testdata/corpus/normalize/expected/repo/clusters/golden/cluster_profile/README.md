# Cluster profile: golden

What the cluster is built on and with, as SetDrift found it in the cluster's
system components, their settings, its nodes and, with `--aws`, the EKS API.
It is where recreating the cluster on another platform or cloud starts.
profile.json holds the same for tools, and the last section says where each
value was found. A range found only from what is in use ("within") is the
part the cluster uses now: the network it is in is that size or larger.

## Cloud

| | |
|---|---|
| Cloud | not found |
| Account | not found |
| Region | not found |
| Zones | not found |
| Platform | not found |
| Kubernetes | v1.35.0 |

## Network

| | |
|---|---|
| Pod network | not found |
| Most pods per node | not found |
| Service network | not found |
| IP families | not found |
| Node addresses | not found |
| Network plugin (CNI) | not found |
| CNI settings | not found |
| kube-proxy mode | not found |
| VPC / network | not found |
| Security groups | not found |
| API endpoint | not found |

## DNS

| | |
|---|---|
| Cluster domain | not found |
| Cluster DNS address | not found |
| Upstreams | not found |
| Managed domains (external-dns) | not found |
| Host names served | not found |

## Storage

| Class | Provisioner | Default | Claims | Settings |
|---|---|---|---|---|
| gp3 | ebs.csi.aws.com |  | 2 | type=gp3, reclaim Delete, binding WaitForFirstConsumer |

## Components

| Role | Component | Version | Runs as |
|---|---|---|---|
| observability | fluent-bit | latest | apps/DaemonSet shop/log-agent |

## Where each value was found

| Found | Where |
|---|---|
| observability fluent-bit latest | apps/DaemonSet shop/log-agent |
