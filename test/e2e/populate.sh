#!/usr/bin/env bash
# Fills the e2e cluster with what the export has to handle: Argo CD, Flux
# and External Secrets Operator with real apps, a Helm release, objects
# written by client-side apply, server-side apply and imperative kubectl,
# a CRD with a custom resource, finished and running Jobs, bare Pods, and an
# object stuck in deletion. Safe to run again.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
context=${E2E_CONTEXT:-$("$here/cluster.sh" context)}
region=${E2E_REGION:-${AWS_REGION:-us-east-2}}
k() { kubectl --context "$context" "$@"; }

ARGOCD_VERSION=v3.6.1
FLUX_VERSION=v2.10.0
ESO_CHART_VERSION=2.15.0

"$here/cluster.sh" touch

# GitOps tools and External Secrets Operator.
k create namespace argocd --dry-run=client -o yaml | k apply -f -
k apply -n argocd --server-side --force-conflicts \
  -f "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/install.yaml" >/dev/null
k apply --server-side --force-conflicts \
  -f "https://github.com/fluxcd/flux2/releases/download/$FLUX_VERSION/install.yaml" >/dev/null
# Installed through the setdrift-chart-repo post-renderer, so the export
# can name the chart repository (sd-chart below is not: RESTORE.md then
# shows the chart's hints).
export PATH="$here/../../bin:$PATH"
[[ -x $here/../../bin/setdrift ]] || make -C "$here/../.." build >/dev/null
helm plugin list 2>/dev/null | grep -q '^setdrift-chart-repo' ||
  helm plugin install "$here/../../contrib/helm/setdrift-chart-repo" >/dev/null
helm --kube-context "$context" upgrade --install external-secrets external-secrets \
  --repo https://charts.external-secrets.io --version "$ESO_CHART_VERSION" \
  --post-renderer setdrift-chart-repo --post-renderer-args --chart-repo=https://charts.external-secrets.io \
  -n external-secrets --create-namespace --wait >/dev/null
k wait --for condition=established --timeout=120s \
  crd/applications.argoproj.io crd/gitrepositories.source.toolkit.fluxcd.io \
  crd/kustomizations.kustomize.toolkit.fluxcd.io crd/clustersecretstores.external-secrets.io

# Fixtures.
k apply -f "$here/fixtures/10-cluster.yaml"
k wait --for condition=established --timeout=60s crd/widgets.example.com
k apply -f "$here/fixtures/20-apps.yaml"
k apply --server-side -f "$here/fixtures/30-ssa.yaml"
k apply -f "$here/fixtures/40-argocd.yaml"
k apply -f "$here/fixtures/50-flux.yaml"
sed "s/REGION/$region/" "$here/fixtures/60-external-secrets.yaml" | k apply -f -

# Imperative objects: no last-applied annotation, one field manager per command.
ns=sd-imperative
if ! k -n $ns get deployment hello >/dev/null 2>&1; then
  k -n $ns create deployment hello --image=public.ecr.aws/nginx/nginx:1.27 --replicas=2
  k -n $ns expose deployment hello --port 80
  k -n $ns create configmap flags --from-literal=beta=true
  k -n $ns create secret generic api-key --from-literal=key=imperative-test-value
  k -n $ns run toolbox --image=public.ecr.aws/docker/library/busybox:1.36 --restart=Never -- sleep infinity
  k -n $ns scale deployment hello --replicas=3
  k -n $ns set image deployment/hello nginx=public.ecr.aws/nginx/nginx:1.27.3
  k -n $ns label deployment hello tier=frontend
  k -n $ns annotate deployment hello owner=team-a
  k -n $ns set env deployment/hello MODE=prod
  k -n $ns rollout restart deployment/hello
  # A client-side-applied object edited afterwards.
  k -n sd-apps scale deployment web --replicas=3
  k -n sd-apps rollout restart deployment/web
fi

# Stuck in deletion behind its finalizer.
k -n sd-apps delete configmap stuck --wait=false --ignore-not-found

# A Helm release.
if ! helm --kube-context "$context" status sd-chart -n sd-helm >/dev/null 2>&1; then
  chart=$(mktemp -d)
  helm create "$chart/chart" >/dev/null
  sed -i.bak 's#repository: nginx#repository: public.ecr.aws/nginx/nginx#; s#tag: ""#tag: "1.27"#' "$chart/chart/values.yaml"
  helm --kube-context "$context" install sd-chart "$chart/chart" -n sd-helm --create-namespace >/dev/null
  rm -rf "$chart"
fi

"$here/cluster.sh" touch
echo "Populated $context"

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
