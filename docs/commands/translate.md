# setdrift translate

Writes an export for another platform, like an EKS export for GKE. The export's generic set stays as it is, and the translation is an overlay beside it, in `translations/<code>` (`translations/te2g` for EKS to GKE), with what that platform reads differently rewritten.

## Synopsis

```sh
setdrift translate <path-to-export> --to <platform> [flags]
```

## Where translations go

Each translation is in the export's `translations/<code>/`, named by its direction, `t`, the source platform's letter, `2`, the target's.

| Code | Translation |
|---|---|
| `te2g` | EKS to GKE |
| `te2a` | EKS to AKS |
| `tg2e` | GKE to EKS |
| `tg2a` | GKE to AKS |
| `ta2e` | AKS to EKS |
| `ta2g` | AKS to GKE |

A source on no known platform is `x`, like `tx2g`. The directory holds the overlay, its `translation.yaml` (from, to and when), README.md, RESTORE.md and `translate-decisions.yaml`. Every object the overlay applies carries the annotation `setdrift.io/translation` with the code, so the target cluster's objects say they arrived through a translation, and a translated StorageClass also carries `setdrift.io/source-class`, its original, see [Mapping isn't always 1:1](#mapping-isnt-always-11-or-both-ways). `diff` leaves both out of a comparison.

A translation isn't an export, and the two aren't interchangeable. Given a translation or the export's own platform set, `translate` says so and names the export to give instead. Exporting again keeps the translations and their decisions, and says to run `translate` again so each is in step with the new export.

## What it rewrites

Objects keep their kind, names and intent. Only what the target reads differently changes, and the overlay's README.md lists every change and why.

| What | On the target |
|---|---|
| StorageClasses of another platform's provisioner | The target's class of the same kind of storage, under the same name, so claims need no change. EKS gp3 becomes GKE's dynamic disk (Hyperdisk Balanced where the machine supports it, pd-balanced where not) or AKS Premium SSD v2, and the IOPS and throughput set on it carry over where the target's class takes them. The target keeps its own default class. See [Mapping isn't always 1:1](#mapping-isnt-always-11-or-both-ways) |
| Load balancer Services | Internal stays internal and public stays public, written explicitly in the target's annotation, since each platform defaults differently. Another platform's load balancer class and annotations go |
| Node pool and spot labels in Pod templates | The target's labels, keeping the pool name, with the toleration the target's spot taint needs. Other node labels of another platform go, and the README says the placement was relaxed |
| Another platform's finalizers | Removed, since nothing on the target would ever remove them |
| Ingresses of another platform's controller | The target's controller, with internal kept internal and public kept public, written explicitly (GKE's `gce-internal` class, the ALB's `internal` scheme). The settings of the old load balancer, like certificates, health checks and WAF, are listed to set again by hand. AKS's Ingress controller is public only, so an internal Ingress for AKS is a decision |
| IngressClasses of another platform's controller | Left out, the target's controller installs its own |
| VolumeSnapshotClasses | The target's block storage driver |
| SecretProviderClasses | The target's provider, keeping the secret names (renamed where the target's secret manager doesn't allow a character, like `/`), with its own settings, like the vault name or project, left to fill in. SetDrift never copies the values |
| Pods using a cloud identity kept on the source | The projected token and settings that cloud's webhook gives on its own platform, so its SDK works unchanged from the target |
| Objects that manage real cloud resources, from the export's `cloud-resources/` | A decision for each, below. Kept with the source cluster, left out, or for a bucket, written as the target's own bucket (an ACK `Bucket`, a Config Connector `StorageBucket`, or an Azure Service Operator storage account and container), new and empty, with the name, versioning and public access block carried over. What doesn't carry over, like tags or lifecycle rules, is listed to set by hand, and its location (and on Azure, its resource group) is left to fill in |

The overlay also holds its own RESTORE.md, the cross-cloud version of the export's. It says what comes over and what doesn't (a volume's data, since the target can't attach the source's disks, comes over separately, and a cloud resource kept with the source stays there, with the setting that keeps it if that cluster's object is ever deleted). It also has the Kubernetes version check, turning on network policy enforcement when the source enforced its NetworkPolicies, the commands that make a kept identity's cloud trust the new cluster, and when to apply.

## Mapping isn't always 1:1, or both ways

**A translation is the closest equivalent the target has, not always an exact one, and the more a setting is tuned to its cloud, the less of it carries over.** Clouds don't offer the same things, a disk type one has another doesn't, a setting one reads another ignores. translate never drops a setting silently. What doesn't carry over is listed in the overlay's README.md, under "Before you apply", and what's lost outright is in bold in RESTORE.md's "What comes over, and what doesn't". Read both before applying.

Translating back gives the original. A translated StorageClass carries the annotation `setdrift.io/source-class`, a record of the class it was translated from, and a translation back to that platform restores it exactly, whatever was lost on the way, while a translation on to a third platform starts from it. One way, though, each step gets only what its target offers. The table below is storage between EKS and AKS.

| Source class | On the target | Comes over |
|---|---|---|
| EKS gp3, with its IOPS and throughput | AKS Premium SSD v2 | Exactly, both clouds set them the same way (3,000 IOPS and 125 free, up to 80,000 and 2,000) |
| EKS io1 or io2 | AKS Premium SSD v2 with its IOPS, or Ultra Disk above 80,000 | Exactly, up to each disk's limit |
| AKS Ultra Disk | EKS io2 with its IOPS | Up to io2's 256,000 IOPS, capped and said above that |
| AKS Premium SSD (v1) or Standard SSD | EKS gp3 | As the same or better, AWS's cheapest SSD is faster than Standard SSD, and Premium SSD (v1) sets its speed by size, which gp3 doesn't |
| AKS Standard HDD | EKS st1 when every claim of the class is at least 125 GiB, st1's smallest, otherwise gp3 | As the closest that works, st1 is built for throughput, and a smaller claim would fail on it |
| AKS zone-redundant disks (ZRS) | EKS gp3 | **Not their zone redundancy.** An EBS volume is in one zone, so a zone outage takes it down. RESTORE.md says so in bold. Replicate the data across zones yourself, like a database's replicas |
| EKS gp3's IOPS and throughput, to GKE | GKE's dynamic disk | **Not the IOPS and throughput**, GKE's class doesn't take them, so set them on the disks by hand if the workload needs them. Translated back to EKS, the class has them again |

Settings outside a class's kind, like EBS's `encrypted` or a KMS key, don't carry over to another cloud and are listed to set by hand, with a key of the target cloud's. The CSI settings every driver reads, like `csi.storage.k8s.io/fstype`, carry over as they are.

## Decisions

What has no 1:1 equivalent is a decision, kept in `translate-decisions.yaml` in the overlay's directory. Each entry says what the feature is and its options.

| Choice | Means |
|---|---|
| `keep-source` | Keep using the source cloud's service, like an AWS IAM role through federation, or for an object that manages a cloud resource (an ACK Bucket), leave it to the source cluster |
| `equivalent` | The target's nearest equivalent, where SetDrift knows one |
| `leave-out` | Leave it out of the translation |
| `require` | For the export's NetworkPolicies, the default. `setdrift verify` fails on a target that doesn't enforce them |
| `accept-unenforced` | For the export's NetworkPolicies, in an emergency. `setdrift verify` passes on a target that doesn't enforce them, and says so loudly every run. The policies are applied either way |

A choice must be one its entry offers, and translate refuses a file it can't read, saying what's wrong. A decision for something the export no longer has, like an object deleted since, is dropped, and translate says so.

Where there's a safe default, like `keep-source` for a cloud identity or an External Secrets store, or `require` for NetworkPolicies the source enforced, translate takes it and lists it. Where there isn't, like an object that creates cloud resources, translate stops and writes the file for you to fill in. Run translate again after choosing, and your choices carry over to every later run.

`setdrift verify` on a cluster of the target platform checks the translation, and counts what was decided in the file as explained.

## Examples

Translate an EKS export for GKE.

```sh
setdrift translate ./setdrift-export --to gke
```

Then apply it on the GKE cluster, and check it.

```sh
kubectl --context gke-prod apply --server-side -k ./setdrift-export/clusters/prod/translations/te2g
setdrift verify --context gke-prod ./setdrift-export
```

Translate for EKS in an emergency, before the target enforces NetworkPolicies, keeping that choice on record.

```sh
setdrift translate ./setdrift-export --to eks --accept-unenforced-network-policies
```

Translate for AKS, leaving out everything with no equivalent instead of stopping.

```sh
setdrift translate ./setdrift-export --to aks --unmatched leave-out
```

## Flags

### `--help`, `-h`

*on/off · default off*

Prints the command's help.

### `--to`

*text · default none*

The platform to translate for, `eks`, `aks` or `gke`, or its cloud's name, `aws`, `azure` or `gcp`. Required. An export can hold a translation for each, and translating for the platform it came from is refused.

### `--unmatched`

*text · default none*

One choice, `keep-source` or `leave-out`, for every undecided feature that offers it, instead of stopping for them. Choices already made in `translate-decisions.yaml` stay as they are.

### `--accept-unenforced-network-policies`

*on/off · default off*

`--accept-unenforced-network-policy` is the same flag. The NetworkPolicies themselves are applied either way: the flag only decides whether an unconfirmed enforcement passes. Chooses `accept-unenforced` for the export's NetworkPolicies in `translate-decisions.yaml`, so `setdrift verify` passes on a target that doesn't enforce them. Meant for an emergency: translate and verify both say, loudly, that the policies aren't enforced. Without NetworkPolicies the source enforced, there's nothing to accept, and translate says so.

## See also

- [setdrift export](export.md), which writes the export
- [setdrift verify](verify.md), which checks a translation on a cluster of its platform
