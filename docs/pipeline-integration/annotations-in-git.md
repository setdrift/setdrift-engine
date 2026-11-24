# Annotations in Git

SetDrift treats a resource as your pipeline's when it has the annotation `setdrift.io/source-repo`, set to the Git repository it's deployed from. A second annotation, `setdrift.io/source-path`, is optional and names the directory in that repository. ([Source annotations](source-annotations.md) lists them all.)

```yaml
metadata:
  annotations:
    setdrift.io/source-repo: https://github.com/acme/shop
    setdrift.io/source-path: deploy/overlays/prod
```

Neither value changes from one deploy to the next, so you can add them to your manifests in Git once, and your pipeline doesn't change at all. SetDrift then shows the resources as `CI/CD https://github.com/acme/shop//deploy/overlays/prod`.

| How you deploy | What to add |
|---|---|
| `kubectl apply -k` | [A small file in the overlay](#kustomize) |
| `kubectl apply -f` | [The same file, and switch to `-k`](#plain-manifests), or the annotations in each manifest |
| `helm template` with your own chart | [A value in the chart](#your-own-helm-chart) |
| `helm template` with someone else's chart, or kubectl commands in a script | Not covered, [add the script to your pipeline](script-annotations.md) |

Committed annotations don't record each deploy's branch, commit or run, since those change every time. SetDrift doesn't need them to stop showing the resources as unmanaged. To record them too, [add the script to your pipeline](script-annotations.md).

## Kustomize

Add `setdrift-source.yaml` to the overlay, with your repository and the overlay's path from the repository's root.

```yaml
# says where these resources come from, so SetDrift doesn't show them as unmanaged
apiVersion: builtin
kind: AnnotationsTransformer
metadata:
  name: setdrift-source
annotations:
  setdrift.io/source-repo: https://github.com/acme/shop
  setdrift.io/source-path: deploy/overlays/prod
fieldSpecs:
  - path: metadata/annotations
    create: true
```

Name it in the overlay's `kustomization.yaml`.

```yaml
resources:
  - ../../base
transformers:
  - setdrift-source.yaml
```

The next `kubectl apply -k deploy/overlays/prod` adds the annotations. To preview them, run `kubectl kustomize deploy/overlays/prod`.

Keep the `fieldSpecs` lines. They put the annotations only on each resource's own metadata. Kustomize's `commonAnnotations` and `kustomize edit add annotation` also write them into every pod template, which restarts every pod.

## Plain manifests

Add a `kustomization.yaml` that lists the files, next to the same `setdrift-source.yaml`, and change `kubectl apply -f deploy/prod` to `kubectl apply -k deploy/prod`.

```yaml
resources:
  - deployment.yaml
  - service.yaml
transformers:
  - setdrift-source.yaml
```

Use `-k` from then on. `kubectl apply -f` would try to apply `setdrift-source.yaml` too, and fail.

If you can't switch to `-k`, add the annotations to each manifest instead. Every new file needs them too.

```yaml
metadata:
  name: web
  annotations:
    setdrift.io/source-repo: https://github.com/acme/shop
    setdrift.io/source-path: deploy/prod
```

## Your own Helm chart

Set the annotations once in `values.yaml`.

```yaml
sourceAnnotations:
  setdrift.io/source-repo: https://github.com/acme/shop
  setdrift.io/source-path: charts/web
```

Add them to the metadata of every template.

```yaml
metadata:
  name: {{ include "web.fullname" . }}
  {{- with .Values.sourceAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
```

If a template already has an `annotations:` block, put the `with` inside it instead.

```yaml
metadata:
  name: {{ include "web.fullname" . }}
  annotations:
    prometheus.io/scrape: "true"
    {{- with .Values.sourceAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
```
