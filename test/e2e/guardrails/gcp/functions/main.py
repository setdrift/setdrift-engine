# The GCP guardrails, two Cloud Run functions setup.sh deploys.
#
# kill_switch runs on each budget notification. When the month's cost reaches
# KILL_FRACTION of the budget it deletes the test GKE clusters, then disables
# billing on the project, which stops everything in it (Google's documented
# way to cap costs). With SIMULATE=true it only logs what it would do, which
# is how setup.sh deploys it until setup.sh arm.
#
# reap runs every hour, from Cloud Scheduler, and deletes each GKE cluster
# labeled setdrift-e2e=true that is older than its ttl-hours label (default 2).
import base64
import datetime
import json
import os

import functions_framework
from google.cloud import billing_v1, container_v1

PROJECT = os.environ["GOOGLE_CLOUD_PROJECT"]
KILL_FRACTION = float(os.environ.get("KILL_FRACTION", "0.75"))
SIMULATE = os.environ.get("SIMULATE", "true").lower() != "false"


def test_clusters():
    client = container_v1.ClusterManagerClient()
    clusters = client.list_clusters(parent=f"projects/{PROJECT}/locations/-").clusters
    return client, [c for c in clusters if c.resource_labels.get("setdrift-e2e") == "true"]


def delete(client, cluster, why):
    name = f"projects/{PROJECT}/locations/{cluster.location}/clusters/{cluster.name}"
    if SIMULATE:
        print(f"SIMULATE: would delete {name} ({why})")
        return
    print(f"deleting {name} ({why})")
    client.delete_cluster(name=name)


@functions_framework.cloud_event
def kill_switch(event):
    data = json.loads(base64.b64decode(event.data["message"]["data"]).decode())
    cost, budget = float(data["costAmount"]), float(data["budgetAmount"])
    print(f"budget {data.get('budgetDisplayName')}: {cost} of {budget} {data.get('currencyCode')}")
    if budget <= 0 or cost < KILL_FRACTION * budget:
        return
    client, clusters = test_clusters()
    for c in clusters:
        delete(client, c, f"cost {cost} reached {KILL_FRACTION:.0%} of {budget}")
    billing = billing_v1.CloudBillingClient()
    name = f"projects/{PROJECT}"
    if not billing.get_project_billing_info(name=name).billing_enabled:
        print("billing is already disabled")
        return
    if SIMULATE:
        print(f"SIMULATE: would disable billing on {name}")
        return
    billing.update_project_billing_info(name=name, project_billing_info=billing_v1.ProjectBillingInfo(billing_account_name=""))
    print(f"billing disabled on {name}")


@functions_framework.cloud_event
def reap(event):
    now = datetime.datetime.now(datetime.timezone.utc)
    client, clusters = test_clusters()
    for c in clusters:
        created = datetime.datetime.fromisoformat(c.create_time.replace("Z", "+00:00"))
        ttl = float(c.resource_labels.get("ttl-hours", "2"))
        age = (now - created).total_seconds() / 3600
        if age >= ttl:
            delete(client, c, f"{age:.1f} hours old, ttl {ttl}")


# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
