#!/usr/bin/env bash
# Manages the e2e test cluster (eks-cluster.yaml). The reaper (reaper.yaml)
# deletes it after E2E_IDLE_MINUTES (default 120) without use; any command
# here except status counts as use.
#
#   cluster.sh setup     deploy the reaper (once per account and region)
#   cluster.sh up        start creating the cluster in the background; returns at once
#   cluster.sh ensure    create it if needed, wait until ready, point kubectl at it
#   cluster.sh touch     record use, postponing teardown
#   cluster.sh status    stack state and time until teardown
#   cluster.sh down      tear it down now (through the reaper, which also
#                        cleans up load balancers, volumes and test secrets)
#
# Settings: E2E_VERSION (1.37), E2E_CLUSTER (setdrift-e2e-<version>),
# E2E_REGION (AWS_REGION, else us-east-2), E2E_ADMIN_ARN (the caller),
# AWS_PROFILE. The kubeconfig context is <cluster>-admin.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
version=${E2E_VERSION:-1.37}
cluster=${E2E_CLUSTER:-setdrift-e2e-${version//./}}
region=${E2E_REGION:-${AWS_REGION:-us-east-2}}
idle=${E2E_IDLE_MINUTES:-120}
reaper_stack=setdrift-reaper # must not start with setdrift-e2e-, or it would reap itself
context=$cluster-admin
state=${XDG_CACHE_HOME:-$HOME/.cache}/setdrift-e2e
log=$state/$cluster.log
pidfile=$state/$cluster.pid
param=/setdrift-e2e/$cluster/last-used

aws() { command aws --region "$region" "$@"; }
say() { printf '%s\n' "$*" >&2; }

stack_status() {
  aws cloudformation describe-stacks --stack-name "$1" --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE
}

reaper_output() {
  aws cloudformation describe-stacks --stack-name "$reaper_stack" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text 2>/dev/null
}

touch_cluster() {
  aws ssm put-parameter --name "$param" --type String --overwrite \
    --value "$(date -u +%Y-%m-%dT%H:%M:%S+00:00)" >/dev/null
}

# The IAM principal kubectl runs as: an assumed-role session maps to its role.
admin_arn() {
  if [[ -n ${E2E_ADMIN_ARN:-} ]]; then echo "$E2E_ADMIN_ARN"; return; fi
  local arn
  arn=$(aws sts get-caller-identity --query Arn --output text)
  if [[ $arn =~ ^arn:([^:]+):sts::([0-9]+):assumed-role/([^/]+)/ ]]; then
    arn="arn:${BASH_REMATCH[1]}:iam::${BASH_REMATCH[2]}:role/${BASH_REMATCH[3]}"
  fi
  echo "$arn"
}

setup() {
  aws cloudformation deploy --stack-name "$reaper_stack" \
    --template-file "$here/reaper.yaml" --capabilities CAPABILITY_IAM \
    --parameter-overrides IdleMinutes="$idle" --no-fail-on-empty-changeset
}

up() {
  local role status
  role=$(reaper_output ClusterStackRoleArn)
  if [[ -z $role || $role == None ]]; then
    say "The reaper is not deployed in $region; run: $0 setup"
    exit 1
  fi
  status=$(stack_status "$cluster")
  case $status in
    CREATE_COMPLETE | UPDATE_COMPLETE | UPDATE_ROLLBACK_COMPLETE)
      touch_cluster; say "$cluster is up ($status)"; return ;;
    CREATE_IN_PROGRESS | UPDATE_IN_PROGRESS | *CLEANUP_IN_PROGRESS)
      touch_cluster; say "$cluster is being created ($status); log: $log"; return ;;
    NONE | DELETE_COMPLETE | DELETE_IN_PROGRESS | ROLLBACK_COMPLETE | DELETE_FAILED) ;;
    *) say "$cluster is $status; fix or delete it first"; exit 1 ;;
  esac
  mkdir -p "$state"
  touch_cluster
  local admin
  admin=$(admin_arn)
  say "Creating $cluster (Kubernetes $version) in the background; log: $log"
  nohup bash -c '
    set -e
    while true; do
      s=$(aws --region "$1" cloudformation describe-stacks --stack-name "$2" --query "Stacks[0].StackStatus" --output text 2>/dev/null || echo NONE)
      case $s in
        NONE|DELETE_COMPLETE) break ;;
        ROLLBACK_COMPLETE|DELETE_FAILED) aws --region "$1" cloudformation delete-stack --stack-name "$2" --role-arn "$4" ;;
      esac
      echo "waiting for the previous $2 to be deleted ($s)"; sleep 30
    done
    aws --region "$1" cloudformation deploy --stack-name "$2" --template-file "$3" \
      --capabilities CAPABILITY_IAM --role-arn "$4" \
      --parameter-overrides ClusterName="$2" KubernetesVersion="$5" AdminPrincipalArn="$6"
  ' _ "$region" "$cluster" "$here/eks-cluster.yaml" "$role" "$version" "$admin" >"$log" 2>&1 &
  echo $! >"$pidfile"
  disown
}

wait_ready() {
  local status start=$SECONDS
  while true; do
    status=$(stack_status "$cluster")
    case $status in
      CREATE_COMPLETE | UPDATE_COMPLETE | UPDATE_ROLLBACK_COMPLETE) break ;;
      *_IN_PROGRESS) ;;
      *)  # NONE, DELETE_COMPLETE, ROLLBACK_COMPLETE, *_FAILED: fine only while `up` is still working
        if ! kill -0 "$(cat "$pidfile" 2>/dev/null)" 2>/dev/null; then
          say "$cluster is $status"; [[ -f $log ]] && tail -20 "$log" >&2; exit 1
        fi ;;
    esac
    say "$(date +%H:%M:%S) $cluster: $status ($(((SECONDS - start) / 60)) min)"
    sleep 30
  done
  local profile=()
  [[ -n ${AWS_PROFILE:-} ]] && profile=(--profile "$AWS_PROFILE")
  aws eks update-kubeconfig --name "$cluster" --alias "$context" ${profile[@]+"${profile[@]}"} >/dev/null
  until kubectl --context "$context" get --raw /readyz >/dev/null 2>&1; do sleep 5; done
  touch_cluster
  say "$cluster is ready; kubectl context: $context"
}

status() {
  local s last
  s=$(stack_status "$cluster")
  echo "stack $cluster: $s"
  last=$(aws ssm get-parameter --name "$param" --query Parameter.Value --output text 2>/dev/null || true)
  if [[ -n $last && $s != NONE ]]; then
    local age=$(( ($(date -u +%s) - $(date -u -j -f %Y-%m-%dT%H:%M:%S "${last%+00:00}" +%s 2>/dev/null || date -u -d "$last" +%s)) / 60 ))
    echo "last used $last ($age min ago); torn down after $idle min idle (checked every 15 min)"
  fi
  echo "reaper: $(stack_status "$reaper_stack")"
}

down() {
  local fn
  fn=$(reaper_output ReaperFunction)
  [[ -n $fn && $fn != None ]] || { say "The reaper is not deployed; run: $0 setup"; exit 1; }
  aws lambda invoke --function-name "$fn" --cli-binary-format raw-in-base64-out \
    --payload "{\"force\":\"$cluster\"}" /dev/stdout >/dev/null
  say "Deleting $cluster; \`$0 status\` shows progress"
}

case ${1:-} in
  setup) setup ;;
  up) up ;;
  ensure) up; wait_ready ;;
  touch) touch_cluster ;;
  status) status ;;
  down) down ;;
  context) echo "$context" ;;
  *) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
