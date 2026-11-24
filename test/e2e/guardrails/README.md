# Cost guardrails for the Azure and GCP test accounts

SetDrift's end-to-end tests create real AKS and GKE clusters. These guardrails keep a forgotten cluster or a runaway test from costing more than about $100 a month on either cloud, and stop all spending in the test account when it gets there.

Neither cloud offers a true spending cap for a normal account. Azure pay-as-you-go subscriptions can't have a spending limit at all, and a GCP budget only sends alerts. What both offer is a budget that can start automation, so the guardrails are built in layers, each catching what the one before it misses.

| Layer | Azure | GCP |
|---|---|---|
| Isolation | A subscription of its own, so nothing here can touch anything else | A project of its own |
| The guard that acts in hours | A reaper, every hour, deletes test resource groups past their time to live (2 hours by default) | A reaper, every hour, deletes test GKE clusters past their time to live |
| Alerts | Emails at 25% of the budget and when the month is forecast to pass it | Emails to the billing account's admins at 25%, 75% and 100% |
| The kill switch | At 50% of the budget, denies every new resource and deletes every resource group except the guard's own, Azure's `azure-signup-rg` (it holds the account's free services) and any tagged `setdrift-keep=true` | At 75% of the budget, deletes the test clusters and disables billing on the project, which stops everything in it |

The kill switch fires below the cap because both clouds report cost late. Azure evaluates budgets once a day, on cost data 8 to 24 hours old, so spending can run a day or two past the point that fires it, hence 50%. GCP sends budget updates several times a day, with the first one possibly hours late, hence 75%. Neither is a guarantee, which is why the reaper is the guard that matters day to day.

A GCP free trial account is never charged unless you upgrade it to a paid account. When the $300 credit or the 90 days run out, the trial ends and its resources stop. Keep the trial account as it is and the kill switch is a second line, not the only one.

## Azure

You need the Azure CLI (`brew install azure-cli`) and to be the Owner of the test subscription, which you are if you create it.

1. **Create a subscription only for SetDrift tests.** In the Azure portal, open Subscriptions, then Add, and pick Pay-As-You-Go. Give it a name containing `setdrift`, like `setdrift-e2e`. The setup script refuses any subscription not named that way, since the kill switch deletes everything in the subscription it guards. Note its subscription ID.
2. **Sign in.** Run `az login` in your terminal and sign in in the browser it opens. Never paste a password, key or token into a chat or a file in the repository.
3. **Run the setup.** Use your own email for the alerts. It's passed in the command and never written to the repository.

   ```sh
   AZURE_SUBSCRIPTION=<subscription id> ALERT_EMAIL=<your email> test/e2e/guardrails/azure/setup.sh setup
   ```

   It creates a resource group `setdrift-guard` holding an Automation account with the two runbooks, the hourly reaper schedule, the kill switch's webhook and an action group, then the budget. It's safe to run again, for example to change `BUDGET` or `KILL_PERCENT`.
4. **Test the kill switch once.** It fires the kill switch for real on a throwaway resource group, checks that the group is deleted and the deny-all policy is in place, then lifts the policy. It asks you to type the subscription name first.

   ```sh
   AZURE_SUBSCRIPTION=<subscription id> test/e2e/guardrails/azure/setup.sh test
   ```

5. **Check on it any time.** `status` shows the month's spend against the budget, whether the kill switch has fired, and the test resource groups with their age.

   ```sh
   AZURE_SUBSCRIPTION=<subscription id> test/e2e/guardrails/azure/setup.sh status
   ```

6. **If the kill switch fires**, everything but `setdrift-guard` is deleted and new resources are denied. Once you've looked at why, lift the policy with `setup.sh remove-kill-switch`.

Test resource groups must carry the tags the reaper reads, `setdrift-e2e=true`, `created-at` with the UTC time, and optionally `ttl-hours`.

```sh
az group create --name setdrift-e2e-aks --location eastus \
  --tags setdrift-e2e=true created-at=$(date -u +%Y-%m-%dT%H:%M:%SZ) ttl-hours=2
```

## GCP

You need the Google Cloud CLI (`brew install --cask google-cloud-sdk`).

1. **Sign in.** Run `gcloud auth login` and sign in in the browser it opens.
2. **Find your billing account.** Run `gcloud billing accounts list` and note the ID of the free trial account, like `0X0X0X-0X0X0X-0X0X0X`.
3. **Create a project only for SetDrift tests, and link it to that account.** Its ID must contain `setdrift`, and project IDs are unique across all of Google Cloud, so add something of your own.

   ```sh
   gcloud projects create setdrift-e2e-<something unique>
   gcloud billing projects link setdrift-e2e-<something unique> --billing-account=<billing account id>
   ```

4. **Run the setup.** It enables the APIs it needs, creates a service account with only the roles it needs, the two Pub/Sub topics, the two functions, the reaper's hourly schedule and the budget. The kill switch starts in a mode where it only logs what it would do.

   ```sh
   GCP_PROJECT=setdrift-e2e-<something unique> BILLING_ACCOUNT=<billing account id> test/e2e/guardrails/gcp/setup.sh setup
   ```

5. **Test the kill switch.** It sends the kill switch a budget update at the kill point and shows its log, which should end with `SIMULATE: would disable billing`.

   ```sh
   GCP_PROJECT=setdrift-e2e-<something unique> test/e2e/guardrails/gcp/setup.sh test
   ```

6. **Arm it.** From then on it really disables billing at 75% of the budget.

   ```sh
   GCP_PROJECT=setdrift-e2e-<something unique> test/e2e/guardrails/gcp/setup.sh arm
   ```

7. **If the kill switch fires**, billing on the project is off and everything in it stops, and some resources may be deleted for good. To use the project again, re-enable billing on it by hand in the console.

Test GKE clusters must carry the labels the reaper reads, `setdrift-e2e=true` and optionally `ttl-hours`.

```sh
gcloud container clusters create-auto setdrift-e2e-gke --region us-central1 --labels setdrift-e2e=true,ttl-hours=2
```

## Settings

| Setting | Azure | GCP |
|---|---|---|
| The test account | `AZURE_SUBSCRIPTION` (its name must contain `setdrift`) | `GCP_PROJECT` (its ID must contain `setdrift`) |
| Where alerts go | `ALERT_EMAIL` | The billing account's admins |
| The monthly budget | `BUDGET`, default `100` | `BUDGET`, default `100` |
| When the kill switch fires | `KILL_PERCENT`, default `50` | `KILL_PERCENT`, default `75` |
| Region | `LOCATION`, default `eastus` | `REGION`, default `us-central1` |
