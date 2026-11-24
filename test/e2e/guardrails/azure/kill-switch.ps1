# The Azure kill switch, an Azure Automation runbook the budget's action group
# starts through a webhook. It stops all spending in the test subscription:
# first a policy that denies creating or changing any resource, then it deletes
# every resource group except the guard's own (setdrift-guard), so the kill
# switch and the reaper survive, Azure's own azure-signup-rg, which holds the
# account's free services, and any group tagged setdrift-keep=true. It runs
# as the Automation account's managed identity. Undo with remove-kill-switch in setup.sh.
param([object] $WebhookData)

$ErrorActionPreference = 'Stop'
$guard = 'setdrift-guard'

Connect-AzAccount -Identity | Out-Null
$sub = (Get-AzContext).Subscription.Id
Write-Output "Kill switch fired for subscription $sub"

# Deny every resource creation or change, outside the guard's resource group.
$definition = Get-AzPolicyDefinition -Name 'setdrift-deny-all' -ErrorAction SilentlyContinue
if (-not $definition) {
  $rule = '{"if": {"field": "type", "like": "*"}, "then": {"effect": "deny"}}'
  $definition = New-AzPolicyDefinition -Name 'setdrift-deny-all' -DisplayName 'SetDrift kill switch: deny all' -Policy $rule -Mode All
}
if (-not (Get-AzPolicyAssignment -Name 'setdrift-deny-all' -Scope "/subscriptions/$sub" -ErrorAction SilentlyContinue)) {
  New-AzPolicyAssignment -Name 'setdrift-deny-all' -DisplayName 'SetDrift kill switch: deny all' -Scope "/subscriptions/$sub" `
    -PolicyDefinition $definition -NotScope "/subscriptions/$sub/resourceGroups/$guard" | Out-Null
}
Write-Output 'Denying all new resources'

# Delete every other resource group, in parallel, keeping the guard's, Azure's
# signup group and any tagged setdrift-keep=true.
$keep = @($guard, 'azure-signup-rg')
# a group with no tags at all has a null Tags, which can't be indexed
$doomed = Get-AzResourceGroup | Where-Object { $_.ResourceGroupName -notin $keep -and -not ($_.Tags -and $_.Tags['setdrift-keep'] -eq 'true') }
# the jobs go in a list of their own, a message written inside the loop would land in it too
$jobs = @()
foreach ($rg in $doomed) {
  Write-Output "Deleting resource group $($rg.ResourceGroupName)"
  $jobs += Remove-AzResourceGroup -Name $rg.ResourceGroupName -Force -AsJob
}
if ($jobs.Count -gt 0) {
  $jobs | Wait-Job | Receive-Job
}
Write-Output 'Done'

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
