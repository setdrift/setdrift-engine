#!/usr/bin/env bash
# The AKS e2e run: creates a small AKS cluster in a resource group of its
# own, runs the shared checks (cloud-run.sh), and deletes the resource group
# when it ends, however it ends: passed, failed, or stopped with ctrl-c.
# Azure bills by the minute and out of pocket, so nothing is left running;
# the guardrails' reaper (guardrails/azure) is only a backstop for a run that
# crashed before it could clean up.
#
#   aks.sh            create, test, delete
#
# Settings: AZURE_SUBSCRIPTION (required, the setdrift-e2e subscription),
# AKS_NETWORK_POLICY (none; cilium, calico or azure turns network policy
# enforcement on), E2E_STEPS (a file of steps to run instead of the shared
# checks, its run function called with the cluster up, see translate.sh),
# LOCATION (eastus), AKS_VM_SIZE (Standard_D2as_v7: 2 vCPUs and 8 GiB, enough
# for Argo CD, Flux and External Secrets Operator on one node).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
sub=${AZURE_SUBSCRIPTION:?set AZURE_SUBSCRIPTION to the setdrift-e2e subscription id}
location=${LOCATION:-eastus}
size=${AKS_VM_SIZE:-Standard_D2as_v7}
stamp=$(date -u +%Y%m%d%H%M%S)
rg=setdrift-e2e-aks-$stamp
name=setdrift-e2e-aks
context=$name
cloud=azure
distribution=aks
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
# shellcheck source=cloud-run.sh
source "$here/cloud-run.sh"

az account set --subscription "$sub"
account=$(az account show --query id -o tsv)
[[ $(az account show --query name -o tsv | tr '[:upper:]' '[:lower:]') == *setdrift* ]] ||
  { say "the subscription is not the SetDrift test subscription"; exit 1; }

# a new subscription has the providers AKS uses unregistered; registering is free, once, a no-op after
for ns in Microsoft.ContainerService Microsoft.Compute Microsoft.Network Microsoft.Storage; do
  az provider register --namespace "$ns" --wait --output none
done

started=$(date +%s)
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  say "deleting resource group $rg, and with it the cluster and its node resource group"
  az group delete --name "$rg" --yes --output none 2>/dev/null || true
  # AKS deletes the node resource group it made (MC_...) on its own, a little after
  local node_rg=MC_${rg}_${name}_${location}
  for _ in $(seq 60); do
    [[ $(az group exists --name "$node_rg") == false ]] && break
    sleep 10
  done
  [[ $(az group exists --name "$rg") == false && $(az group exists --name "$node_rg") == false ]] &&
    say "deleted; nothing is left running" || say "WARNING: $rg or $node_rg still exists, delete it: az group delete --name <it> --yes"
  say "the cluster existed for $(( ($(date +%s) - started) / 60 )) minutes"
  rm -rf "$work"
  exit "$rc"
}
trap cleanup EXIT INT TERM

say "resource group $rg, tagged for the reaper as a backstop"
az group create --name "$rg" --location "$location" --output none \
  --tags setdrift-e2e=true created-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" ttl-hours=1

policy=()
# checking if a network policy engine was asked for, if so its turned on, cilium needs Azure's cilium dataplane
case ${AKS_NETWORK_POLICY:-} in
  "") ;;
  cilium) policy=(--network-dataplane cilium --network-policy cilium) ;;
  calico | azure) policy=(--network-policy "$AKS_NETWORK_POLICY") ;;
  *) say "AKS_NETWORK_POLICY must be cilium, calico or azure"; exit 1 ;;
esac
say "creating AKS cluster $name: free tier, one $size node, Azure CNI overlay${AKS_NETWORK_POLICY:+, network policy $AKS_NETWORK_POLICY}"
az aks create --resource-group "$rg" --name "$name" --location "$location" --tier free \
  --node-count 1 --node-vm-size "$size" --node-osdisk-size 30 \
  --network-plugin azure --network-plugin-mode overlay ${policy[@]+"${policy[@]}"} --no-ssh-key --output none

az aks get-credentials --resource-group "$rg" --name "$name" --file "$KUBECONFIG" --context "$context" --output none
say "cluster ready"

# checking if steps were given in place of the shared checks, if so theyre run, the cleanup above deletes the cluster either way
if [[ -n ${E2E_STEPS:-} ]]; then
  # shellcheck source=/dev/null
  source "$E2E_STEPS"
  steps_run
else
  cloud_run
fi

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
