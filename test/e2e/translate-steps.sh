#!/usr/bin/env bash
# The steps of a translation e2e run, sourced by aks.sh, gke.sh or translate.sh
# once the target cluster is up: translate the export for the target, follow
# the translation's RESTORE.md (the export's CRDs and controllers, then the
# overlay), check it with verify, and prove it with diff through the
# translation, against the export's snapshot, so the source cluster can be gone.
#
# The caller sets context (the target's kubeconfig context), E2E_EXPORT (the
# export to translate, copied first so it stays as it was) and E2E_TO (eks, aks
# or gke), and has sourced cloud-run.sh for say and check. Settings:
# E2E_SOURCE_CONTEXT (the source's context, for its Helm values, when it's
# still there; without it a release gets its chart's defaults),
# E2E_ACCEPT_UNENFORCED=1 (a target that doesn't enforce NetworkPolicies
# passes, said loudly), E2E_KEEP_DIR (where the results go).

steps_run() {
  local started out exp root tdir snap
  started=$(date +%s)
  say "building setdrift"
  make -C "$e2e_root" build >/dev/null
  out=${E2E_KEEP_DIR:-$HOME/.cache/setdrift-e2e}/translate-to-$E2E_TO-$(date -u +%Y%m%dT%H%M%SZ)
  mkdir -p "$out"
  cp -R "$E2E_EXPORT" "$out/export"
  exp=$out/export

  say "translating for $E2E_TO"
  local accept=()
  [[ ${E2E_ACCEPT_UNENFORCED:-} == 1 ]] && accept=(--accept-unenforced-network-policies)
  if ! "$setdrift" translate "$exp" --to "$E2E_TO" --unmatched keep-source ${accept[@]+"${accept[@]}"} >"$out/translate.log" 2>&1; then
    cat "$out/translate.log" >&2
    say "FAILED: translate"
    return 1
  fi
  root=$(ls -d "$exp"/clusters/*)
  # the translation for this target, its code ends in the target's letter, like te2a for AKS
  tdir=$(ls -d "$root"/translations/t*2"${E2E_TO:0:1}")
  say "translation $(basename "$tdir"), its decisions in $tdir/translate-decisions.yaml"

  say "following RESTORE.md: the export's CRDs and controllers"
  # the export's own steps for these, its translation's RESTORE.md points to them by name
  # the source's Helm values can't be read once it's gone, so a release whose values can't be read gets its chart's defaults
  # the fixture's local chart has no repository, so it's generated again as populate.sh made it
  helm create "$out/chart" >/dev/null
  sed -i.bak 's#repository: nginx#repository: public.ecr.aws/nginx/nginx#; s#tag: ""#tag: "1.27"#' "$out/chart/values.yaml"
  awk '/^## [0-9]+\. /{f = /\. (CustomResourceDefinitions|Controllers to install first)$/} f' "$root/RESTORE.md" |
    awk '/^```sh$/{c=1;next} /^```$/{c=0} c' |
    sed -E "s#<repo>/chart#$out/chart#; /get values/{s/[[:space:]]+#.*\$//; s/\$/ || true/;}" >"$out/restore-steps.sh"
  if ! (cd "$root" && SRC=${E2E_SOURCE_CONTEXT:-gone} DST=$context bash -e "$out/restore-steps.sh") >"$out/restore.log" 2>&1; then
    tail -20 "$out/restore.log" >&2
    say "FAILED: the export's CRDs and controllers"
    return 1
  fi

  say "applying the translation"
  local applied=0
  # a webhook that isn't serving yet refuses some objects, the apply is idempotent, so its tried again
  for _ in 1 2 3 4 5; do
    if kubectl --context "$context" apply --server-side -k "$tdir" >"$out/apply.log" 2>&1; then
      applied=1
      break
    fi
    sleep 20
  done
  check "the translation applies" test "$applied" = 1

  say "verifying"
  check "verify passes on the target" "$setdrift" verify --context "$context" -o "$out/verify" "$exp"

  say "diffing through the translation, against the export's snapshot"
  snap=$(ls "$exp"/snapshots/*.pb | head -1)
  "$setdrift" diff "$snap" "context:$context" --translation-dir "$tdir" >"$out/diff.txt" 2>&1 || true
  sed -n '/in both/,/^$/p' "$out/diff.txt" >&2
  check "diff finds no real differences" grep -q "No real differences" "$out/diff.txt"

  say "results in $out, $(( ($(date +%s) - started) / 60 )) minutes"
  [[ $fails == 0 ]] || { say "$fails checks failed"; return 1; }
}

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
