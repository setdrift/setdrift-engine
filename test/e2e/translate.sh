#!/usr/bin/env bash
# A translation e2e run in one line: create a fresh cluster of the target
# platform, translate the export for it, follow the translation's RESTORE.md,
# verify it and diff through the translation (translate-steps.sh), and delete
# the cluster however the run ends.
#
#   test/e2e/translate.sh <path-to-export> --to <eks|aks|gke>
#
# The export can come from a cluster that's gone, like the one aks.sh or
# gke.sh keeps in ~/.cache/setdrift-e2e/<platform>-<time>/export: the diff is
# against its snapshot. Each target's own settings apply: AWS_PROFILE for EKS,
# AZURE_SUBSCRIPTION (and AKS_NETWORK_POLICY) for AKS, GCP_PROJECT for GKE, and
# translate-steps.sh's settings, like E2E_ACCEPT_UNENFORCED. Azure costs the
# user money: an AKS target is deleted the moment the run ends.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
usage() { echo "usage: $0 <path-to-export> --to <eks|aks|gke>" >&2; exit 2; }
[[ $# == 3 && $2 == --to ]] || usage
export E2E_EXPORT E2E_TO E2E_STEPS
E2E_EXPORT=$(cd "$1" 2>/dev/null && pwd) || { echo "$1 doesn't exist" >&2; exit 2; }
E2E_TO=$3
E2E_STEPS=$here/translate-steps.sh
[[ -f $E2E_EXPORT/.gitignore || -d $E2E_EXPORT/clusters ]] || { echo "$1 isn't an export (no clusters/ in it)" >&2; exit 2; }

case $E2E_TO in
  aks) exec "$here/aks.sh" ;;
  gke) exec "$here/gke.sh" ;;
  eks)
    # EKS's cluster.sh only creates and deletes, so the run is wrapped here, the cluster deleted however it ends
    # shellcheck source=cloud-run.sh
    source "$here/cloud-run.sh"
    export E2E_CLUSTER=setdrift-e2e-translate
    started=$(date +%s)
    cleanup() {
      local rc=$?
      trap - EXIT INT TERM
      say "deleting cluster $E2E_CLUSTER"
      "$here/cluster.sh" down >&2 || say "WARNING: deleting $E2E_CLUSTER failed, run: E2E_CLUSTER=$E2E_CLUSTER make e2e-down"
      say "the cluster existed for $(( ($(date +%s) - started) / 60 )) minutes"
      exit "$rc"
    }
    trap cleanup EXIT INT TERM
    say "creating EKS cluster $E2E_CLUSTER"
    "$here/cluster.sh" up >&2
    context=$("$here/cluster.sh" context)
    # shellcheck source=translate-steps.sh
    source "$E2E_STEPS"
    steps_run
    ;;
  *) usage ;;
esac

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
