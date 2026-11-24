#!/usr/bin/env bash
# Sets up the cost guardrails for SetDrift's GCP test project: a monthly budget
# scoped to the project that emails the billing account's admins as spending
# grows and sends each update to a Pub/Sub topic, a kill switch
# (functions/main.py, kill_switch) that at KILL_PERCENT of the budget deletes
# the test GKE clusters and disables billing on the project, and an hourly
# reaper (reap) that deletes test clusters past their time to live. See
# ../README.md.
#
#   setup.sh setup    create or update all of it, the kill switch only logging (safe to run again)
#   setup.sh test     send the kill switch a budget update at the kill point, and show what it would do
#   setup.sh arm      let the kill switch act, after test showed it works
#   setup.sh status   the kill switch's mode and the test clusters
#
# Settings: GCP_PROJECT (required, its id must contain "setdrift", since the
# kill switch stops everything in it), BILLING_ACCOUNT (required, the id
# like 0X0X0X-0X0X0X-0X0X0X), BUDGET (100, in the account's currency),
# KILL_PERCENT (75), REGION (us-central1).
#
# Disabling billing stops every service in the project and may delete its
# resources for good; that is the point. Re-enable billing by hand to use the
# project again. A free trial account is never charged unless you upgrade it.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
project=${GCP_PROJECT:?set GCP_PROJECT to the test project id}
account=${BILLING_ACCOUNT:-}
budget=${BUDGET:-100}
kill=${KILL_PERCENT:-75}
region=${REGION:-us-central1}
sa=setdrift-guard@$project.iam.gserviceaccount.com
build_sa=setdrift-build@$project.iam.gserviceaccount.com

say() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The project, checked before anything changes: the kill switch stops
# everything in it, so it must be the test project.
[[ $project == *setdrift* ]] || die "project \"$project\" is not named for SetDrift; the kill switch stops everything in the project it guards, so use a test project whose id contains setdrift"
gcloud config set project "$project" >/dev/null 2>&1

fraction=$(awk "BEGIN { print $kill / 100 }")

# A service account just created takes up to a minute to appear to IAM, so a
# role granted to it at once can fail as if it didn't exist; each grant is
# retried for two minutes.
grant() { # what to grant on (projects, or billing accounts), id, member, role, flag
  # $1 is left unquoted, "billing accounts" is two words to gcloud
  local i
  for i in $(seq 24); do
    if gcloud $1 add-iam-policy-binding "$2" --member="$3" --role="$4" ${5:+"$5"} >/dev/null 2>&1; then
      return
    fi
    sleep 5
  done
  gcloud $1 add-iam-policy-binding "$2" --member="$3" --role="$4" ${5:+"$5"} >/dev/null
}

deploy() { # name entry topic simulate
  gcloud functions deploy "$1" --gen2 --runtime=python313 --region="$region" --source="$here/functions" \
    --entry-point="$2" --trigger-topic="$3" --service-account="$sa" --trigger-service-account="$sa" \
    --build-service-account="projects/$project/serviceAccounts/$build_sa" \
    --set-env-vars="GOOGLE_CLOUD_PROJECT=$project,KILL_FRACTION=$fraction,SIMULATE=$4" --quiet >/dev/null
}

setup() {
  [[ -n $account ]] || die "set BILLING_ACCOUNT to the billing account id (gcloud billing accounts list)"
  say "project $project, billing account $account"

  say "APIs"
  gcloud services enable billingbudgets.googleapis.com cloudbilling.googleapis.com cloudbuild.googleapis.com \
    cloudfunctions.googleapis.com eventarc.googleapis.com run.googleapis.com pubsub.googleapis.com \
    artifactregistry.googleapis.com cloudscheduler.googleapis.com container.googleapis.com >/dev/null

  # Every account is named, since a new project's default compute account,
  # which functions would otherwise build and trigger with, has no roles.
  say "service accounts setdrift-guard (runs the functions) and setdrift-build (builds them)"
  gcloud iam service-accounts describe "$sa" >/dev/null 2>&1 ||
    gcloud iam service-accounts create setdrift-guard --display-name="SetDrift cost guardrails" >/dev/null
  gcloud iam service-accounts describe "$build_sa" >/dev/null 2>&1 ||
    gcloud iam service-accounts create setdrift-build --display-name="SetDrift guardrails build" >/dev/null
  grant projects "$project" "serviceAccount:$build_sa" roles/cloudbuild.builds.builder --condition=None
  # Disabling billing needs Project Billing Manager and Browser on the project
  # and Billing Account Viewer on the account; deleting clusters needs
  # Kubernetes Engine Cluster Admin; the Pub/Sub triggers need Eventarc Event
  # Receiver and Cloud Run Invoker. Nothing broader.
  for role in roles/billing.projectManager roles/browser roles/container.clusterAdmin roles/eventarc.eventReceiver roles/run.invoker; do
    grant projects "$project" "serviceAccount:$sa" "$role" --condition=None
  done
  grant "billing accounts" "$account" "serviceAccount:$sa" roles/billing.viewer

  say "Pub/Sub topics for the budget and the reaper's clock"
  for t in setdrift-budget setdrift-reaper-tick; do
    gcloud pubsub topics describe "$t" >/dev/null 2>&1 || gcloud pubsub topics create "$t" >/dev/null
  done

  say "kill switch, logging only until setup.sh arm"
  deploy setdrift-kill-switch kill_switch setdrift-budget true
  say "reaper"
  deploy setdrift-reaper reap setdrift-reaper-tick false
  gcloud scheduler jobs describe setdrift-reaper --location="$region" >/dev/null 2>&1 ||
    gcloud scheduler jobs create pubsub setdrift-reaper --location="$region" --schedule="0 * * * *" \
      --topic=setdrift-reaper-tick --message-body=tick >/dev/null

  say "budget setdrift-monthly: $budget a month for this project, emails at 25% and 75%, kill switch at $kill%"
  existing=$(gcloud billing budgets list --billing-account="$account" --filter="displayName=setdrift-monthly" --format="value(name)")
  rules=(--threshold-rule=percent=0.25 --threshold-rule=percent=0.75 --threshold-rule=percent=1.0)
  # a budget keeps no history worth saving, so an existing one is replaced rather than edited
  if [[ -n $existing ]]; then
    gcloud billing budgets delete "$existing" --billing-account="$account" --quiet >/dev/null
  fi
  gcloud billing budgets create --billing-account="$account" --display-name=setdrift-monthly \
    --budget-amount="$budget" --filter-projects="projects/$project" "${rules[@]}" \
    --notifications-rule-pubsub-topic="projects/$project/topics/setdrift-budget" >/dev/null
  say "done; run setup.sh test, then setup.sh arm"
}

test_kill() {
  cost=$(awk "BEGIN { print $budget * $kill / 100 }")
  say "sending the kill switch a budget update of $cost of $budget"
  gcloud pubsub topics publish setdrift-budget --message="{\"budgetDisplayName\": \"setdrift-monthly (test)\", \"costAmount\": $cost, \"budgetAmount\": $budget, \"currencyCode\": \"USD\"}" >/dev/null
  say "waiting a minute for it to run"
  sleep 60
  gcloud functions logs read setdrift-kill-switch --region="$region" --limit=20
  say "the lines above should end with 'SIMULATE: would disable billing on projects/$project'"
}

arm() {
  say "arming the kill switch: from now on it disables billing at $kill% of the budget"
  deploy setdrift-kill-switch kill_switch setdrift-budget false
  say "armed"
}

status() {
  gcloud functions describe setdrift-kill-switch --region="$region" --format='value(serviceConfig.environmentVariables.SIMULATE)' |
    sed 's/^true$/kill switch: logging only (setup.sh arm to arm it)/; s/^false$/kill switch: armed/'
  gcloud container clusters list --filter='resourceLabels.setdrift-e2e=true' --format='table(name,location,createTime,resourceLabels.ttl-hours)'
}

case ${1:-} in
setup) setup ;;
test) test_kill ;;
arm) arm ;;
status) status ;;
*) sed -n '2,18p' "$0" >&2; exit 2 ;;
esac

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
