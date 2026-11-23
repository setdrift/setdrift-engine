# The Azure reaper, an Azure Automation runbook that runs every hour. It
# deletes each resource group tagged setdrift-e2e=true whose created-at tag is
# older than its ttl-hours tag (default 2), so a test cluster someone forgot
# never runs past a few hours. It runs as the Automation account's managed
# identity.
$ErrorActionPreference = 'Stop'

Connect-AzAccount -Identity | Out-Null
$now = [DateTime]::UtcNow
foreach ($rg in Get-AzResourceGroup -Tag @{ 'setdrift-e2e' = 'true' }) {
  $created = $rg.Tags['created-at']
  $ttl = if ($rg.Tags['ttl-hours']) { [double]$rg.Tags['ttl-hours'] } else { 2 }
  if (-not $created) {
    Write-Output "Skipping $($rg.ResourceGroupName): no created-at tag"
    continue
  }
  $age = ($now - [DateTime]::Parse($created).ToUniversalTime()).TotalHours
  if ($age -ge $ttl) {
    Write-Output ("Deleting {0}, {1:N1} hours old, ttl {2}" -f $rg.ResourceGroupName, $age, $ttl)
    Remove-AzResourceGroup -Name $rg.ResourceGroupName -Force -AsJob | Out-Null
  }
}

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
