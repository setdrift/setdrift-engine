#!/usr/bin/env bash
# Sets up the cost guardrails for SetDrift's Azure test subscription: a monthly
# budget that emails as spending grows and, at KILL_PERCENT of it, fires the
# kill switch (kill-switch.ps1), which denies all new resources and deletes
# every resource group but setdrift-guard, Azure's azure-signup-rg and any
# tagged setdrift-keep=true; and an hourly reaper (reaper.ps1)
# that deletes test resource groups past their time to live. See ../README.md.
#
#   setup.sh setup                create or update all of it (safe to run again)
#   setup.sh status               the budget's spend so far, the kill switch's state
#   setup.sh test                 fire the kill switch on purpose, then check it worked
#   setup.sh remove-kill-switch   lift the deny-all policy after it fired
#
# Settings: AZURE_SUBSCRIPTION (required, its name must contain "setdrift",
# since the kill switch deletes everything in it), ALERT_EMAIL (required),
# BUDGET (100, in the billing currency), KILL_PERCENT (50), LOCATION (eastus).
#
# Azure evaluates budgets once a day on cost data 8 to 24 hours old, so the
# kill switch can fire a day or two after the spending. That is why it fires
# at half the budget by default, and why the reaper is the guard that acts in
# hours.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
sub=${AZURE_SUBSCRIPTION:?set AZURE_SUBSCRIPTION to the test subscription id or name}
email=${ALERT_EMAIL:-}
budget=${BUDGET:-100}
kill=${KILL_PERCENT:-50}
location=${LOCATION:-eastus}
guard=setdrift-guard
arm=https://management.azure.com

say() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The subscription, checked before anything changes: the kill switch deletes
# every resource group in it, so it must be the test subscription.
az account set --subscription "$sub"
name=$(az account show --query name -o tsv)
sub=$(az account show --query id -o tsv)
[[ $(printf %s "$name" | tr "[:upper:]" "[:lower:]") == *setdrift* ]] || die "subscription \"$name\" is not named for SetDrift; the kill switch deletes everything in the subscription it guards, so give the test subscription a name containing setdrift"
rg=$arm/subscriptions/$sub/resourceGroups/$guard
aa=$rg/providers/Microsoft.Automation/automationAccounts/$guard

put() { az rest --method put --url "$1" --body "$2" --output none; }

runbook() { # name file: create or update a PowerShell 7.2 runbook and publish it
  put "$aa/runbooks/$1?api-version=2023-11-01" \
    "{\"location\": \"$location\", \"properties\": {\"runbookType\": \"PowerShell72\", \"logProgress\": false, \"logVerbose\": false}}"
  az rest --method put --url "$aa/runbooks/$1/draft/content?api-version=2023-11-01" \
    --headers Content-Type=text/powershell --body @"$2" --output none
  az rest --method post --url "$aa/runbooks/$1/publish?api-version=2023-11-01" --output none
}

setup() {
  [[ -n $email ]] || die "set ALERT_EMAIL to the address budget alerts go to"
  say "subscription $name ($sub)"

  # a new subscription has these resource providers unregistered
  for ns in Microsoft.Automation Microsoft.Insights Microsoft.Consumption; do
    say "resource provider $ns"
    az provider register --namespace "$ns" --wait --output none
  done

  say "resource group $guard"
  az group create --name "$guard" --location "$location" --tags setdrift-guard=true --output none

  say "Automation account $guard, with a managed identity"
  put "$aa?api-version=2023-11-01" \
    "{\"location\": \"$location\", \"identity\": {\"type\": \"SystemAssigned\"}, \"properties\": {\"sku\": {\"name\": \"Basic\"}}}"
  principal=$(az rest --method get --url "$aa?api-version=2023-11-01" --query identity.principalId -o tsv)

  # Contributor deletes resource groups; Resource Policy Contributor assigns
  # the deny-all policy, which Contributor alone may not.
  for role in Contributor "Resource Policy Contributor"; do
    say "role $role for the Automation account"
    az role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal \
      --role "$role" --scope "/subscriptions/$sub" --output none 2>/dev/null || true
  done

  say "runbooks kill-switch and reaper"
  runbook kill-switch "$here/kill-switch.ps1"
  runbook reaper "$here/reaper.ps1"

  say "the reaper, every hour"
  start=$(date -u -d '+10 minutes' +%Y-%m-%dT%H:%M:00Z 2>/dev/null || date -u -v+10M +%Y-%m-%dT%H:%M:00Z)
  put "$aa/schedules/hourly?api-version=2023-11-01" \
    "{\"name\": \"hourly\", \"properties\": {\"startTime\": \"$start\", \"frequency\": \"Hour\", \"interval\": 1, \"timeZone\": \"UTC\"}}"
  existing=$(az rest --method get --url "$aa/jobSchedules?api-version=2023-11-01" \
    --query "value[?properties.runbook.name=='reaper'] | length(@)" -o tsv)
  if [[ $existing == 0 ]]; then
    put "$aa/jobSchedules/$(uuidgen | tr 'A-Z' 'a-z')?api-version=2023-11-01" \
      '{"properties": {"schedule": {"name": "hourly"}, "runbook": {"name": "reaper"}}}'
  fi

  # The webhook's URL is its secret: it goes straight into the action group
  # and is never printed. Azure shows it only once, so a new one is made each
  # time setup runs, and the action group gets the new one.
  say "webhook for the kill switch"
  url=$(az rest --method post --url "$aa/webhooks/generateUri?api-version=2015-10-31" -o tsv)
  put "$aa/webhooks/budget?api-version=2015-10-31" \
    "{\"name\": \"budget\", \"properties\": {\"isEnabled\": true, \"uri\": \"$url\", \"expiryTime\": \"2036-01-01T00:00:00Z\", \"runbook\": {\"name\": \"kill-switch\"}}}"

  say "action group setdrift-kill, emailing $email and calling the kill switch"
  az monitor action-group create --resource-group "$guard" --name setdrift-kill --short-name sdkill \
    --action email alert "$email" --action webhook kill-switch "$url" --output none
  unset url
  group=$(az monitor action-group show --resource-group "$guard" --name setdrift-kill --query id -o tsv)

  say "budget setdrift-monthly: $budget a month, an email at 25%, the kill switch at $kill%, an email when the month is forecast to pass the budget"
  first=$(date -u +%Y-%m-01T00:00:00Z)
  put "$arm/subscriptions/$sub/providers/Microsoft.Consumption/budgets/setdrift-monthly?api-version=2023-05-01" "$(cat <<JSON
{"properties": {
  "category": "Cost", "amount": $budget, "timeGrain": "Monthly",
  "timePeriod": {"startDate": "$first", "endDate": "2036-12-31T00:00:00Z"},
  "notifications": {
    "Actual_GreaterThanOrEqualTo_25_Percent": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": 25, "thresholdType": "Actual", "contactEmails": ["$email"]},
    "Actual_GreaterThanOrEqualTo_${kill}_Percent": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": $kill, "thresholdType": "Actual", "contactEmails": ["$email"], "contactGroups": ["$group"]},
    "Forecasted_GreaterThanOrEqualTo_100_Percent": {"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": 100, "thresholdType": "Forecasted", "contactEmails": ["$email"]}
  }
}}
JSON
)"
  say "done; run setup.sh test to fire the kill switch once and check it works"
}

status() {
  say "subscription $name"
  az rest --method get --url "$arm/subscriptions/$sub/providers/Microsoft.Consumption/budgets/setdrift-monthly?api-version=2023-05-01" \
    --query '{budget: properties.amount, spentThisMonth: properties.currentSpend.amount, currency: properties.currentSpend.unit}' -o table
  if az policy assignment show --name setdrift-deny-all --scope "/subscriptions/$sub" >/dev/null 2>&1; then
    say "the kill switch has fired: new resources are denied (setup.sh remove-kill-switch lifts it)"
  else
    say "the kill switch has not fired"
  fi
  az group list --tag setdrift-e2e=true --query '[].{group: name, created: tags."created-at", ttlHours: tags."ttl-hours"}' -o table
}

test_kill() {
  say "this fires the kill switch: every resource group in $name is deleted, except $guard, azure-signup-rg and any tagged setdrift-keep=true"
  read -r -p "type the subscription name to go on: " answer
  [[ $answer == "$name" ]] || die "not confirmed"
  az group create --name setdrift-killtest --location "$location" --output none
  job=$(uuidgen | tr 'A-Z' 'a-z')
  put "$aa/jobs/$job?api-version=2023-11-01" '{"properties": {"runbook": {"name": "kill-switch"}}}'
  say "kill switch job $job started, waiting for it"
  for _ in $(seq 60); do
    state=$(az rest --method get --url "$aa/jobs/$job?api-version=2023-11-01" --query properties.status -o tsv)
    [[ $state == Completed || $state == Failed || $state == Stopped ]] && break
    sleep 10
  done
  say "job $state"
  [[ $state == Completed ]] || die "the kill switch job ended $state; see its output in the portal under Automation account $guard > Jobs"
  az policy assignment show --name setdrift-deny-all --scope "/subscriptions/$sub" --output none || die "the deny-all policy is not assigned"
  for _ in $(seq 30); do
    [[ $(az group exists --name setdrift-killtest) == false ]] && break
    sleep 10
  done
  [[ $(az group exists --name setdrift-killtest) == false ]] || die "setdrift-killtest was not deleted"
  say "the kill switch works; lifting the deny-all policy"
  remove_kill_switch
}

remove_kill_switch() {
  az policy assignment delete --name setdrift-deny-all --scope "/subscriptions/$sub" --output none 2>/dev/null || true
  say "new resources are allowed again"
}

case ${1:-} in
setup) setup ;;
status) status ;;
test) test_kill ;;
remove-kill-switch) remove_kill_switch ;;
*) sed -n '2,15p' "$0" >&2; exit 2 ;;
esac

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
