#!/usr/bin/env bash
#
# The restore experiment, end to end: export the e2e cluster, follow the
# RESTORE.md it writes into a second, empty e2e cluster, and check the result.
# It runs the playbook's own commands, in order, so a regression in the
# playbook fails here.
#
#   test/e2e/restore.sh          (or make e2e-restore)
#
# Where the playbook offers a choice, this takes one:
#   - Secrets: option C, External Secrets Operator. The test does by hand what
#     a user does: it runs the playbook's create-secret commands, filling each
#     <value> from the source's fixture Secret (never via disk), and the
#     target's operator (Pod Identity) restores them; the check compares them
#     with the source's. SetDrift itself never copies a value. Other objects holding secret values: option A, copied from the
#     source (the values never touch disk).
#   - volumes: copied from snapshots, not reused, so the source keeps its own
# and it skips what a test can't do:
#   - the IRSA trust policy update: the fixture's role is in an account no
#     one can read (E2E_RESTORE_IRSA=1 runs it anyway)
#   - filling in redacted files (option B)
# The remote secrets are named after the target ($target/...), which its
# operator's role may read, and deleted afterwards.
# Settings the target's own stack already made (its add-on, access entry
# and Pod Identity association) fail with "already exists", which counts as
# done. The one chart with no repository, the fixture's local sd-chart, is
# generated again as populate.sh made it.
#
# Settings: E2E_SOURCE (setdrift-e2e-137), E2E_TARGET (<source>-restore),
# E2E_KEEP=1 keeps the target cluster afterwards, as a failed run does (the
# reaper still removes it when idle), E2E_SETTLE_SECONDS (900) for the
# workloads to come up, AWS_PROFILE. Cost: a second cluster for about half an hour.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
source_cluster=${E2E_SOURCE:-setdrift-e2e-137}
target_cluster=${E2E_TARGET:-$source_cluster-restore}
region=${E2E_REGION:-${AWS_REGION:-us-east-2}}
work=$(mktemp -d "${TMPDIR:-/tmp}/setdrift-restore.XXXXXX")
# The work directory holds Helm values files taken from the source, which can hold secrets.
trap 'cd / && rm -rf "$work"' EXIT
started=$(date +%s)
failures=()

say() { printf '\n== %s\n' "$*" >&2; }
fail() { failures+=("$*"); printf 'FAIL: %s\n' "$*" >&2; }
elapsed() { echo "$(( $(date +%s) - started ))s"; }
cluster() { E2E_CLUSTER=$1 "$here/cluster.sh" "${@:2}"; }

export PATH="$repo/bin:$PATH"
make -C "$repo" build >/dev/null

say "source $source_cluster and target $target_cluster ($(elapsed))"
cluster "$target_cluster" up >/dev/null # in the background, while the source is prepared
E2E_CLUSTER=$source_cluster make -C "$repo" e2e-populate >/dev/null
SRC=$(cluster "$source_cluster" context)
k_src() { kubectl --context "$SRC" "$@"; }

# Data to find again after the restore, one marker per volume.
marker="restore-$(date +%s)"
k_src -n sd-apps wait --for=condition=Ready pod/db-0 pod/scratch-user --timeout=300s >/dev/null
k_src -n sd-apps exec db-0 -- sh -c "echo $marker > /data/marker && sync"
k_src -n sd-apps exec scratch-user -- sh -c "echo $marker > /s/marker && sync"

say "export ($(elapsed))"
setdrift export --context "$SRC" --cluster-name "$source_cluster" --aws -o "$work/export" \
  --secret-key-template "$target_cluster/{{.Namespace}}/{{.Name}}" >"$work/export.log" 2>&1 ||
  { cat "$work/export.log"; exit 1; }
root=$work/export/clusters/$source_cluster

# checks the least-privilege IAM policy in docs/permissions-aws.md against the source cluster's real resources
# it asks IAM's policy simulator whether the policy allows each call export --aws makes, on each resource it makes it on, nothing is created
# the actions themselves are kept in step with the code by pkg/awsinventory's TestPermissionsDocMatchesCalls
check_least_privilege() {
  local account policy cluster_arn decision pairs=() p action resource arn
  account=$(aws sts get-caller-identity --query Account --output text)
  # the policy from the doc, with this cluster's region, account and name filled in
  policy=$(awk '/^```json$/ { f = 1; next } /^```$/ { f = 0 } f' "$repo/docs/permissions-aws.md" |
    sed "s/REGION/$region/g; s/ACCOUNT/$account/g; s/CLUSTER/$source_cluster/g")
  cluster_arn=$(aws eks describe-cluster --region "$region" --name "$source_cluster" --query cluster.arn --output text)
  for action in DescribeCluster ListPodIdentityAssociations ListAccessEntries ListAddons; do
    pairs+=("eks:$action $cluster_arn")
  done
  # each Pod Identity association, and the role it names
  for arn in $(aws eks list-pod-identity-associations --region "$region" --cluster-name "$source_cluster" --query 'associations[].associationArn' --output text); do
    pairs+=("eks:DescribePodIdentityAssociation $arn")
  done
  for arn in $(aws eks list-pod-identity-associations --region "$region" --cluster-name "$source_cluster" --query 'associations[].associationId' --output text); do
    pairs+=("iam:GetRole $(aws eks describe-pod-identity-association --region "$region" --cluster-name "$source_cluster" --association-id "$arn" --query association.roleArn --output text)")
  done
  # each access entry
  for p in $(aws eks list-access-entries --region "$region" --cluster-name "$source_cluster" --query 'accessEntries[]' --output text); do
    arn=$(aws eks describe-access-entry --region "$region" --cluster-name "$source_cluster" --principal-arn "$p" --query accessEntry.accessEntryArn --output text)
    pairs+=("eks:DescribeAccessEntry $arn" "eks:ListAssociatedAccessPolicies $arn")
  done
  # each add-on
  for p in $(aws eks list-addons --region "$region" --cluster-name "$source_cluster" --query 'addons[]' --output text); do
    pairs+=("eks:DescribeAddon $(aws eks describe-addon --region "$region" --cluster-name "$source_cluster" --addon-name "$p" --query addon.addonArn --output text)")
  done
  pairs+=("ec2:DescribeVolumes *" "ec2:DescribeSubnets *" "ec2:DescribeVpcs *")
  for p in "${pairs[@]}"; do
    action=${p%% *} resource=${p#* }
    decision=$(aws iam simulate-custom-policy --policy-input-list "$policy" --action-names "$action" --resource-arns "$resource" \
      --query 'EvaluationResults[0].EvalDecision' --output text)
    [[ $decision == allowed ]] || fail "docs/permissions-aws.md's policy does not allow $action on $resource ($decision)"
  done
  # and a role in another account stays out of reach, as the doc says
  decision=$(aws iam simulate-custom-policy --policy-input-list "$policy" --action-names iam:GetRole \
    --resource-arns "arn:aws:iam::000000000000:role/other" --query 'EvaluationResults[0].EvalDecision' --output text)
  [[ $decision != allowed ]] || fail "docs/permissions-aws.md's policy allows iam:GetRole on another account's role"
  echo "   least-privilege policy: ${#pairs[@]} calls allowed on the cluster's resources" >&2
}
say "least-privilege IAM policy ($(elapsed))"
check_least_privilege

cluster "$target_cluster" ensure >/dev/null
DST=$(cluster "$target_cluster" context)
export SRC DST DST_CLUSTER=$target_cluster REGION=$region

# The fixture's local chart, as populate.sh made it.
helm create "$work/chart" >/dev/null
sed -i.bak 's#repository: nginx#repository: public.ecr.aws/nginx/nginx#; s#tag: ""#tag: "1.27"#' "$work/chart/values.yaml"

# Errors that mean the target already has it.
tolerated='already exists|AlreadyExists|ResourceInUseException|EntityAlreadyExists'

# run_line runs one playbook command in this shell (variables like SNAP and
# NEW carry to the next line), tolerating "already exists".
run_line() {
  local line=$1 rc=0
  line=${line//<repo>\/chart/$work/chart}
  local placeholder='<[a-z-]+>'
  if [[ $line =~ $placeholder ]]; then
    fail "placeholder left in a command: $line"
    return
  fi
  printf '$ %s\n' "$line" >&2
  eval "$line" 2>"$work/err" </dev/null || rc=$?
  if ((rc != 0)); then
    if grep -qE "$tolerated" "$work/err"; then
      echo "   (already there)" >&2
    else
      cat "$work/err" >&2
      fail "command failed ($rc): $line"
    fi
  fi
}

# skip says why a block is not run, or nothing.
# fill_remote_secret runs one of the playbook's create-secret lines with each
# <value> filled in from the source's Secret: kubectl to jq to aws, never via disk.
fill_remote_secret() {
  local line=$1 key ns name tpl value
  if [[ $line == *'put-parameter'* ]]; then
    fail "Parameter Store remote secrets are not filled by this test: $line"
    return
  fi
  local name_re="--name ('[^']*'|[^ ]+)" tpl_re="--secret-string '(\{[^']*\})'" src_re='# replaces ([^ /]+)/([^ ]+)$'
  [[ $line =~ $name_re ]] || { fail "no --name in: $line"; return; }
  key=${BASH_REMATCH[1]}; key=${key#\'}; key=${key%\'}
  [[ $line =~ $tpl_re ]] || { fail "no --secret-string in: $line"; return; }
  tpl=${BASH_REMATCH[1]}
  [[ $line =~ $src_re ]] || { fail "no '# replaces ns/name' in: $line"; return; }
  ns=${BASH_REMATCH[1]} name=${BASH_REMATCH[2]}
  value=$(k_src -n "$ns" get secret "$name" -o json |
    jq -ce --argjson tpl "$tpl" '.data as $d | $tpl | with_entries(.value = ($d[.key] // error("no key \(.key)") | @base64d))') ||
    { fail "reading Secret $ns/$name for $key"; return; }
  echo "   remote secret $key (from $ns/$name)" >&2
  aws secretsmanager create-secret --region "$region" --name "$key" --secret-string "$value" >/dev/null 2>&1 ||
    aws secretsmanager put-secret-value --region "$region" --secret-id "$key" --secret-string "$value" >/dev/null ||
    fail "writing remote secret $key"
}
skip() {
  local block=$1
  case $block in
    SRC=*) echo "settings, set above" ;;
    *'# filled in by'*) ;; # Secrets created empty for their controller: run
    *--field-manager=setdrift-restore\ -f\ redacted/*) echo "option B: the redacted files need the real values" ;;
    *create-tags*) echo "reusing volumes: copies are made instead" ;;
    *update-assume-role-policy*) [[ ${E2E_RESTORE_IRSA:-} == 1 ]] || echo "IRSA: the fixture's role is in an unreadable account" ;;
  esac
}

# The playbook's sh blocks, in order, NUL-separated.
# The platform's own playbooks go where RESTORE.md's platform step says to
# follow them: EKS's settings, then AWS-wide ones (EBS copies, secrets).
playbook() {
  awk -v eks="$root/platform/aws/eks/RESTORE.md" -v aws="$root/platform/aws/RESTORE.md" '
    /^## [0-9]+\. (EKS|AWS) settings$/ {
      print
      while ((getline l < eks) > 0) print l
      while ((getline l < aws) > 0) print l
      next
    }
    { print }' "$root/RESTORE.md"
}
blocks() {
  playbook | awk '/^```sh$/ { inblock = 1; next }
       /^```$/ && inblock { inblock = 0; printf "%c", 0; next }
       inblock { print }'
}
# The set to apply: the platform set when the export has one for EKS, as RESTORE.md says for a target on EKS.
set=.
[[ -d $root/platform/aws/eks ]] && set=platform/aws/eks

say "follow RESTORE.md ($(elapsed))"
cd "$root"
while IFS= read -r -d '' block; do
  if why=$(skip "$block") && [[ -n $why ]]; then
    printf '\n-- skipped (%s):\n%s\n' "$why" "$(head -1 <<<"$block")" >&2
    continue
  fi
  echo >&2
  if [[ $block == *'secretsmanager create-secret'* || $block == *'ssm put-parameter'* ]]; then
    # Option C: the remote secrets, created by hand as the playbook says, the values from the source.
    while IFS= read -r line; do
      [[ $line == *'create-secret'* || $line == *'put-parameter'* ]] && fill_remote_secret "$line"
    done <<<"$block"
    continue
  fi
  if [[ $block == *'apply -k external-secrets/'* ]]; then
    # The store the ExternalSecrets read from, for AWS, then the ExternalSecrets.
    run_line 'kubectl --context "$DST" apply --server-side -f platform/aws/secret-store.yaml'
    run_line 'kubectl --context "$DST" -n external-secrets wait --for=jsonpath={.subsets[0].addresses[0].ip} endpoints/external-secrets-webhook --timeout=300s'
    run_line 'kubectl --context "$DST" apply -k external-secrets/'
    continue
  fi
  if [[ $block == *'apply --server-side -k '* ]]; then
    # The playbook: if a webhook is not up yet, wait and apply again.
    for attempt in 1 2 3 4 5; do
      if kubectl --context "$DST" apply --server-side -k "$set" >"$work/apply.log" 2>&1; then break; fi
      if ((attempt == 5)); then cat "$work/apply.log" >&2; fail "apply -k $set failed"; fi
      echo "   apply attempt $attempt failed; again in 30s" >&2
      sleep 30
    done
    continue
  fi
  while IFS= read -r line; do
    [[ -z $line || $line == \#* ]] && continue
    # Option A for Secrets: External Secrets restores them here.
    if [[ $line == *' get secret '*'kubectl --context "$DST" apply'* ]]; then
      printf -- '-- skipped (a Secret: External Secrets restores it): %s\n' "${line:0:90}" >&2
      continue
    fi
    run_line "$line"
  done <<<"$block"
done < <(blocks)

say "check the result ($(elapsed))"
k_dst() { kubectl --context "$DST" "$@"; }

# unavailable lists the Deployments in a namespace with fewer available replicas than wanted.
unavailable() {
  k_dst -n "$1" get deployments -o jsonpath='{range .items[*]}{.metadata.name} {.spec.replicas} {.status.availableReplicas}{"\n"}{end}' 2>/dev/null |
    awk '$2 != ($3 == "" ? 0 : $3) { print $1 }'
}
# not_ready lists the named Pods in sd-apps that are not Ready.
not_ready() {
  local p
  for p in db-0 scratch-user; do
    [[ $(k_dst -n sd-apps get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) == True ]] || echo "$p"
  done
}
# One deadline for everything, polled: kubectl wait times out per object and drops its watch on a slow API.
deadline=$(( $(date +%s) + ${E2E_SETTLE_SECONDS:-900} ))
while :; do
  pending=$(for ns in argocd flux-system external-secrets; do unavailable "$ns" | sed "s#^#$ns/#"; done; not_ready | sed 's#^#sd-apps/pod/#')
  [[ -z $pending ]] && break
  if (( $(date +%s) > deadline )); then
    fail "not ready in time: $(tr '\n' ' ' <<<"$pending")"
    # What a person would look at: the Pods, their recent events, the Deployments' conditions.
    for ns in argocd flux-system external-secrets sd-apps; do
      echo "-- $ns" >&2
      k_dst -n "$ns" get pods -o wide >&2 || true
      k_dst -n "$ns" get events --sort-by=.lastTimestamp 2>/dev/null | tail -15 >&2 || true
    done
    k_dst -n argocd get deployments -o jsonpath='{range .items[*]}{.metadata.name}: {range .status.conditions[*]}{.type}={.status} {.message}; {end}{"\n"}{end}' >&2 || true
    break
  fi
  sleep 15
done
[[ $(k_dst -n sd-apps exec db-0 -- cat /data/marker 2>/dev/null) == "$marker" ]] || fail "db-0's volume lost its data"
[[ $(k_dst -n sd-apps exec scratch-user -- cat /s/marker 2>/dev/null) == "$marker" ]] || fail "scratch's volume lost its data"
not_running=$(k_dst get pods -A --no-headers --field-selector=status.phase!=Running,status.phase!=Succeeded 2>/dev/null || true)
[[ -z $not_running ]] || fail "pods not running: $not_running"
# The Secrets External Secrets restored hold the source's values (compared by hash).
k_dst wait --for=condition=Ready externalsecret --all -A --timeout=300s >/dev/null || fail "ExternalSecrets not ready"
for f in external-secrets/namespaces/*/externalsecret.external-secrets.io/*.yaml; do
  ns=$(basename "$(dirname "$(dirname "$f")")") name=$(basename "$f" .yaml)
  a=$(k_src -n "$ns" get secret "$name" -o jsonpath='{.data}' | shasum -a 256)
  b=$(k_dst -n "$ns" get secret "$name" -o jsonpath='{.data}' | shasum -a 256)
  [[ $a == "$b" ]] || fail "Secret $ns/$name restored with other values than the source's"
done
setdrift verify --context "$DST" . | tee "$work/verify.log" || fail "setdrift verify"
grep -q "this is a different cluster" "$work/verify.log" || fail "verify did not note the different cluster"
[[ $set == . ]] || grep -q "the $set set" "$work/verify.log" || fail "verify did not check the $set set"

say "clean up ($(elapsed))"
# The source keeps its volumes as before; the snapshots go.
for pv in $(grep -ho "patch pv [^ ]*" RESTORE.md | awk '{print $3}'); do
  k_src patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null
done
# The remote secrets the test wrote.
for key in $(grep -ho "key: $target_cluster/[^ ]*" external-secrets/namespaces/*/*/*.yaml | awk '{print $2}' | sort -u); do
  aws secretsmanager delete-secret --region "$region" --secret-id "$key" --force-delete-without-recovery >/dev/null 2>&1 || true
done
for f in restore/volumes/*/*.snapshot; do
  [[ -s $f ]] && aws ec2 delete-snapshot --region "$region" --snapshot-id "$(cat "$f")" || true
done
if [[ ${E2E_KEEP:-} == 1 ]] || ((${#failures[@]} > 0)); then
  echo "Keeping $target_cluster to look at (context $DST); the reaper removes it after 2h idle, or: E2E_CLUSTER=$target_cluster make e2e-down" >&2
else
  cluster "$target_cluster" down >/dev/null # the reaper also deletes the copied volumes
fi
"$here/cluster.sh" touch >/dev/null

if ((${#failures[@]} > 0)); then
  say "restore FAILED after $(elapsed): ${#failures[@]} problem(s)"
  printf '  - %s\n' "${failures[@]}"
  exit 1
fi
say "restore passed in $(elapsed)"

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
