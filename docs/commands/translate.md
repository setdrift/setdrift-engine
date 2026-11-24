# setdrift translate

Writes an export for another platform, like an EKS export for GKE. The export's generic set stays as it is, and the translation is an overlay beside it, at the target's platform directory (`platform/gcp/gke` for GKE), with what that platform reads differently rewritten.

## Synopsis

```sh
setdrift translate EXPORT --to PLATFORM [flags]
```

## What it rewrites

Objects keep their kind, names and intent. Only what the target reads differently changes, and the overlay's README.md lists every change and why.

| What | On the target |
|---|---|
| StorageClasses of another platform's provisioner | The target's class of the same kind of storage, under the same name, so claims need no change. EKS gp3 becomes GKE's dynamic disk (Hyperdisk Balanced where the machine supports it, pd-balanced where not) or AKS Premium SSD v2. The target keeps its own default class |
| Load balancer Services | Internal stays internal and public stays public, written explicitly in the target's annotation, since each platform defaults differently. Another platform's load balancer class and annotations go |
| Node pool and spot labels in Pod templates | The target's labels, keeping the pool name, with the toleration the target's spot taint needs. Other node labels of another platform go, and the README says the placement was relaxed |
| Another platform's finalizers | Removed, since nothing on the target would ever remove them |

## Decisions

What has no 1:1 equivalent is a decision, kept in `translate-decisions.yaml` in the overlay's directory. Each entry says what the feature is and its options.

| Choice | Means |
|---|---|
| `keep-source` | Keep using the source cloud's service, like an AWS IAM role through federation, or for an object that manages a cloud resource (an ACK Bucket), leave it to the source cluster |
| `equivalent` | The target's nearest equivalent, where SetDrift knows one |
| `leave-out` | Leave it out of the translation |

Where there's a safe default, like `keep-source` for a cloud identity or an External Secrets store, translate takes it and lists it. Where there isn't, like an object that creates cloud resources, translate stops and writes the file for you to fill in. Run translate again after choosing, and your choices carry over to every later run.

`setdrift verify` on a cluster of the target platform checks the translation, and counts what was decided in the file as explained.

## Examples

Translate an EKS export for GKE.

```sh
setdrift translate ./setdrift-export --to gke
```

Then apply it on the GKE cluster, and check it.

```sh
kubectl --context gke-prod apply --server-side -k ./setdrift-export/clusters/prod/platform/gcp/gke
setdrift verify --context gke-prod ./setdrift-export
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

The platform to translate for, `eks`, `aks` or `gke`. Required. An export can hold a translation for each, and translating for the platform it came from is refused.

### `--unmatched`

*text · default none*

One choice, `keep-source` or `leave-out`, for every undecided feature that offers it, instead of stopping for them. Choices already made in `translate-decisions.yaml` stay as they are.

## See also

- [setdrift export](export.md), which writes the export
- [setdrift verify](verify.md), which checks a translation on a cluster of its platform
