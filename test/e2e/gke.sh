#!/usr/bin/env bash
# The GKE e2e run: creates a small zonal GKE cluster, runs the shared checks
# (cloud-run.sh), and deletes the cluster when it ends, however it ends:
# passed, failed, or stopped with ctrl-c. The guardrails' reaper
# (guardrails/gcp) is only a backstop for a run that crashed before it could
# clean up.
#
#   gke.sh            create, test, delete
#
# Settings: GCP_PROJECT (required, the setdrift-e2e project), ZONE
# (us-central1-a), GKE_MACHINE_TYPE (e2-standard-2: 2 vCPUs and 8 GiB). One
# zonal cluster's management fee is covered by GKE's free tier.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
project=${GCP_PROJECT:?set GCP_PROJECT to the setdrift-e2e project id}
zone=${ZONE:-us-central1-a}
machine=${GKE_MACHINE_TYPE:-e2-standard-2}
name=setdrift-e2e-gke
context=gke_${project}_${zone}_${name}
cloud=gcp
distribution=gke
account=$project
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
# shellcheck source=cloud-run.sh
source "$here/cloud-run.sh"

[[ $project == *setdrift* ]] || { say "the project is not the SetDrift test project"; exit 1; }
# kubectl and setdrift sign in to GKE through gke-gcloud-auth-plugin, found on
# PATH; a Homebrew install keeps it in the SDK's own bin, off PATH
if ! command -v gke-gcloud-auth-plugin >/dev/null; then
  PATH=$(gcloud info --format='value(installation.sdk_root)')/bin:$PATH
  export PATH
fi
command -v gke-gcloud-auth-plugin >/dev/null || { say "install the GKE auth plugin: gcloud components install gke-gcloud-auth-plugin"; exit 1; }

# GKE needs Compute Engine's API besides its own; on once, a no-op after
gcloud services enable compute.googleapis.com container.googleapis.com --project "$project" >/dev/null

started=$(date +%s)
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  say "deleting cluster $name"
  gcloud container clusters delete "$name" --project "$project" --zone "$zone" --quiet 2>/dev/null || true
  if gcloud container clusters describe "$name" --project "$project" --zone "$zone" >/dev/null 2>&1; then
    say "WARNING: $name still exists, delete it: gcloud container clusters delete $name --zone $zone --project $project"
  else
    say "deleted; nothing is left running"
  fi
  say "the cluster existed for $(( ($(date +%s) - started) / 60 )) minutes"
  rm -rf "$work"
  exit "$rc"
}
trap cleanup EXIT INT TERM

say "creating GKE cluster $name: zonal, one $machine node, labeled for the reaper as a backstop"
gcloud container clusters create "$name" --project "$project" --zone "$zone" \
  --num-nodes 1 --machine-type "$machine" --disk-size 30 --disk-type pd-standard \
  --labels setdrift-e2e=true,ttl-hours=1 --release-channel regular --quiet >/dev/null

gcloud container clusters get-credentials "$name" --project "$project" --zone "$zone" >/dev/null
say "cluster ready"

cloud_run

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
