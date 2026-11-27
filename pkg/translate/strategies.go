// the strategies the capability map names, each a mechanism that differs by more than its values, implemented here
// a platform or cloud picks one by name in its YAML, TestStrategiesImplemented fails when a name the map takes has none here

package translate

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"setdrift.io/setdrift-engine/pkg/capability"
	"sigs.k8s.io/yaml"
)

// trusts write RESTORE.md's steps that make an identity's cloud trust a new cluster's OIDC issuer for one ServiceAccount, subject is its OIDC subject
var trusts = map[string]func(b *strings.Builder, id Identity, subject string){
	capability.TrustAWSIAMRole: func(b *strings.Builder, id Identity, subject string) {
		account := ""
		if parts := strings.Split(id.ID, ":"); len(parts) >= 5 {
			account = parts[4]
		}
		role := id.ID[strings.LastIndex(id.ID, "/")+1:]
		fmt.Fprintf(b, " keeps AWS IAM role `%s`. Register the issuer with AWS IAM once, then add a statement to the role's trust policy (`aws iam get-role --role-name %s` shows it; keep its other statements).\n\n", id.ID, shq(role))
		fmt.Fprintf(b, "```sh\naws iam create-open-id-connect-provider --url \"$ISSUER\" --client-id-list sts.amazonaws.com\n```\n\n")
		fmt.Fprintf(b, "```json\n{\n  \"Effect\": \"Allow\",\n  \"Principal\": {\"Federated\": \"arn:aws:iam::%s:oidc-provider/<ISSUER without https://>\"},\n  \"Action\": \"sts:AssumeRoleWithWebIdentity\",\n  \"Condition\": {\"StringEquals\": {\n    \"<ISSUER without https://>:sub\": \"%s\",\n    \"<ISSUER without https://>:aud\": \"sts.amazonaws.com\"\n  }}\n}\n```\n\n", account, subject)
	},
	capability.TrustAzureFederatedCredential: func(b *strings.Builder, id Identity, subject string) {
		find := "`az identity list` finds the identity with this client ID, and its name and resource group"
		// checking if the client ID is a GUID, as Azure's are, if so the lookup names it, anything else from the cluster stays out of a command
		if azureGUID.MatchString(id.ID) {
			find = fmt.Sprintf("`az identity list --query \"[?clientId=='%s']\"` finds the identity's name and resource group", id.ID)
		}
		fmt.Fprintf(b, " keeps Azure managed identity `%s`. Add a federated credential for the new cluster (%s).\n\n", id.ID, find)
		fmt.Fprintf(b, "```sh\naz identity federated-credential create --name %s-%s --identity-name <identity name> --resource-group <resource group> \\\n  --issuer \"$ISSUER\" --subject %s --audiences api://AzureADTokenExchange\n```\n\n", id.Namespace, id.ServiceAccount, subject)
	},
	capability.TrustGCPWorkloadIdentityPool: func(b *strings.Builder, id Identity, subject string) {
		fmt.Fprintf(b, " keeps Google service account `%s`. Google Cloud federates another cluster through a workload identity pool and a credential configuration file, which isn't added to the Pods for you. Create the pool's provider for the issuer and let the ServiceAccount act as the Google service account, then follow Google's guide to mount the configuration (create-cred-config) and a projected token, with GOOGLE_APPLICATION_CREDENTIALS pointing at it.\n\n", id.ID)
		fmt.Fprintf(b, "```sh\ngcloud iam workload-identity-pools providers create-oidc <provider> --location global --workload-identity-pool <pool> \\\n  --issuer-uri \"$ISSUER\" --attribute-mapping google.subject=assertion.sub\ngcloud iam service-accounts add-iam-policy-binding %s --role roles/iam.workloadIdentityUser \\\n  --member \"principal://iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<pool>/subject/%s\"\n```\n\n", shq(id.ID), subject)
	},
}

// imageCopies write RESTORE.md's commands that copy images to a platform's registry, after its setup lines
var imageCopies = map[string]func(m *capability.Map, imgs []Image, ic capability.ImageCopy) []string{
	// az acr import pulls each from the source with a password, one command per image
	capability.ImagesACRImport: func(m *capability.Map, imgs []Image, _ capability.ImageCopy) []string {
		var cmds []string
		for _, img := range imgs {
			user, pw := sourceLogin(m, img)
			cmds = append(cmds, fmt.Sprintf("az acr import --name \"$ACR\" --source %s --image %s --username %s --password \"$(%s)\"", shq(img.Ref), shq(img.Repo+tagOrLatest(img.Tag)), user, pw))
		}
		return cmds
	},
	// crane signs in to each source registry, creates each repository where the registry needs it, then copies each image
	capability.ImagesCrane: func(m *capability.Map, imgs []Image, ic capability.ImageCopy) []string {
		cmds := loginLines(m, imgs)
		if ic.CreateRepository != "" {
			var repos []string
			for _, img := range imgs {
				if !slices.Contains(repos, img.Repo) {
					repos = append(repos, img.Repo)
					cmds = append(cmds, strings.ReplaceAll(ic.CreateRepository, "{repository}", shq(img.Repo)))
				}
			}
		}
		for _, img := range imgs {
			cmds = append(cmds, fmt.Sprintf("crane copy %s \"$REGISTRY\"/%s", shq(img.Ref), shq(img.Repo+tagOrLatest(img.Tag))))
		}
		return cmds
	},
}

// bucketWriters write a cloud's object storage bucket for equivalent, the functions are in buckets.go
var bucketWriters = map[string]func(o *unstructured.Unstructured, b bucket, obj newObject) ([]*unstructured.Unstructured, []string){
	capability.BucketsACKS3:                ackS3Bucket,
	capability.BucketsConfigConnector:      configConnectorBucket,
	capability.BucketsAzureServiceOperator: azureBlobContainer,
}

// secretFormats name the secrets in a Secrets Store CSI class's parameters as a cloud's provider reads them
var secretFormats = map[string]func(names []string, params map[string]any){
	capability.SecretsAWSObjects: func(names []string, params map[string]any) {
		params["objects"] = listYAML(names, func(n string) map[string]any { return map[string]any{"objectName": n, "objectType": "secretsmanager"} })
	},
	// Azure's objects are an array of YAML strings, one per secret
	capability.SecretsAzureObjects: func(names []string, params map[string]any) {
		var arr []string
		for _, n := range names {
			// the error is dropped: marshalling plain maps, strings and slices can't fail
			b, _ := yaml.Marshal(map[string]any{"objectName": n, "objectType": "secret"})
			arr = append(arr, string(b))
		}
		// the error is dropped: marshalling plain maps, strings and slices can't fail
		b, _ := yaml.Marshal(map[string]any{"array": arr})
		params["objects"] = string(b)
	},
	capability.SecretsGCPSecrets: func(names []string, params map[string]any) {
		params["secrets"] = listYAML(names, func(n string) map[string]any {
			return map[string]any{"resourceName": "projects/<project>/secrets/" + n + "/versions/latest", "path": n}
		})
	},
}

// createCommands write RESTORE.md's step that creates the target cluster from the source's shape
var createCommands = map[string]func(b *strings.Builder, r *Result, ri RestoreInfo){
	capability.CreateAzAKS: azAKSCreate,
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
