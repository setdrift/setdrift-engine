#!/usr/bin/env bash
# The steps an AKS or GKE e2e run shares, sourced by aks.sh and gke.sh once
# their cluster is up and KUBECONFIG names it: populate it as populate.sh
# does for EKS, export it, check what the export found about the platform and
# the cluster profile, verify the export against the cluster, and diff the
# snapshot with the live cluster. Each phase prints the time, for pricing a run.
#
# The caller sets context (the kubeconfig context), cloud (azure or gcp),
# distribution (aks or gke), account (the subscription or project the
# profile should find) and work (a scratch directory).

say() { printf '==> %s  %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
fails=0
check() { # description, then a command that must succeed
  local what=$1
  shift
  if "$@"; then
    say "ok: $what"
  else
    say "FAILED: $what"
    fails=$((fails + 1))
  fi
}

e2e_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
setdrift=$e2e_root/bin/setdrift

cloud_run() {
  say "building setdrift"
  make -C "$e2e_root" build >/dev/null

  say "populating $context"
  E2E_CONTEXT=$context E2E_NO_TOUCH=1 "$e2e_root/test/e2e/populate.sh" >&2

  say "exporting"
  "$setdrift" export --context "$context" -o "$work/export" >&2
  local snap
  snap=$(ls "$work"/export/snapshots/*.json | head -1)

  # what the export found, from the snapshot
  field() { python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); print(eval(sys.argv[2], {}, {"d": d}))' "$snap" "$1"; }
  check "the platform is $distribution, high confidence" \
    test "$(field 'd["cluster"]["classification"].get("distribution","")+"/"+d["cluster"]["classification"].get("confidence","")')" = "$distribution/high"
  profile='d["inventory"]["profile"]'
  check "the profile's cloud is $cloud" test "$(field "$profile[\"cloud\"].get(\"provider\",\"\")")" = "$cloud"
  check "the profile found the account" test "$(field "$profile[\"cloud\"].get(\"account\",\"\")")" = "$account"
  check "the profile found the network plugin" test -n "$(field "$profile[\"network\"].get(\"cni\",\"\")")"
  check "the profile found the pod network" test -n "$(field "\",\".join($profile[\"network\"].get(\"podCidrs\",[]))")"
  check "the profile found the service network" test -n "$(field "\",\".join($profile[\"network\"].get(\"serviceCidrs\",[]))")"
  check "the profile found components" test "$(field "len($profile.get(\"components\",[]))")" -gt 0
  check "the export wrote cluster_profile/" test -s "$(ls -d "$work"/export/clusters/*/cluster_profile)/README.md"

  # the run's snapshots and verify report are kept outside the repository, they
  # are what the platform rule packs are built from; they hold no secret values
  # but do name the cloud account, so they never go into Git
  keep=${E2E_KEEP_DIR:-$HOME/.cache/setdrift-e2e}/$distribution-$(date -u +%Y%m%dT%H%M%SZ)
  mkdir -p "$keep"
  cp "$work"/export/snapshots/* "$keep"/

  say "verifying"
  # verify is reported but not counted until the platform's rule pack exists:
  # without it, the platform's own objects (refused by the platform, or not
  # patchable) count against the export
  if "$setdrift" verify --context "$context" -o "$keep/verify" "$work/export" >&2; then
    say "ok: verify passes"
  else
    say "NOTE: verify did not pass, not counted until the $distribution platform rule pack exists (report in $keep/verify)"
  fi

  say "diffing the snapshot with the live cluster"
  "$setdrift" diff "$(ls "$work"/export/snapshots/*.pb | head -1)" "context:$context" >&2 || true

  say "the cluster profile:"
  sed -n '/^## Cloud/,/^## Storage/p' "$(ls -d "$work"/export/clusters/*/cluster_profile)/README.md" >&2

  say "kept the snapshots and verify report in $keep"
  [[ $fails == 0 ]] || { say "$fails checks failed"; return 1; }
  say "all checks passed"
}

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
