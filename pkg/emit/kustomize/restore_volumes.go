// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// RESTORE.md's storage steps: the storage classes claims ask for, and the
// volumes to bring over.

package kustomize

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"setdrift.io/setdrift-engine/internal/text"
	snapshotv1alpha1 "setdrift.io/setdrift-engine/pkg/snapshot/v1alpha1"
)

// volumes: bring data back. It writes restore/volumes/ and reports whether
// it did.
// volumes writes the step that brings the volumes' data over, and
// restore/volumes/; it reports whether it wrote any volume.
func (d *restoreData) volumes(p *playbook, root string) (bool, error) {
	vols := d.s.GetInventory().GetVolumes()
	if len(vols) == 0 {
		return false, nil
	}
	p.section("Volumes")
	p.printf("Applying the export gives each claim a new, empty volume.")
	if slices.ContainsFunc(vols, func(v *snapshotv1alpha1.Volume) bool { return v.VolumeHandle != "" && v.Driver != "" }) {
		p.printf(" To keep the data,\n" +
			"bring each volume over before applying: restore/volumes/ has a\n" +
			"PersistentVolume for each that another cluster can reach, bound in advance\n" +
			"to its claim, so the claim (or the StatefulSet that creates it) gets the\n" +
			"volume with the data.")
	}
	p.printf("\n\n")
	p.printf("| Claim | Size | Class | Volume | Zone | Reclaim |\n|---|---|---|---|---|---|\n")
	for _, v := range vols {
		claim := v.Namespace + "/" + v.Claim
		if v.StatefulSet != "" {
			claim += " (StatefulSet " + v.StatefulSet + ")"
		}
		p.printf("| %s | %s | %s | `%s` | %s | %s |\n", claim, v.Capacity, v.StorageClass, cmp.Or(v.VolumeHandle, "-"), cmp.Or(v.Zone, "-"), v.ReclaimPolicy)
	}
	p.printf("\n")

	ebs, local, wrote, err := writeVolumes(root, vols)
	if err != nil {
		return false, err
	}
	retainSteps(p, vols)
	if len(local) > 0 {
		localVolumesNote(p, local)
	}
	if len(ebs) > 0 {
		d.ebsSteps(p, ebs)
	}
	if wrote > 0 {
		createClaimSteps(p, vols)
	}
	return wrote > 0, nil
}

// writeVolumes writes restore/volumes/: a volume and its claim for each
// volume another cluster can reach. It returns the EBS volumes, the claims
// whose data stays on a node, and how many it wrote.
func writeVolumes(root string, vols []*snapshotv1alpha1.Volume) (ebs []*snapshotv1alpha1.Volume, local []string, wrote int, err error) {
	// local: claims whose volume is not a CSI volume (hostPath, local), so its data stays on a node
	for _, v := range vols {
		if v.VolumeHandle == "" || v.Driver == "" {
			// The PersistentVolume was read (its reclaim policy comes only from it) and has no driver: hostPath or local.
			if v.PersistentVolume != "" && v.ReclaimPolicy != "" && v.Driver == "" {
				local = append(local, v.Namespace+"/"+v.Claim)
			}
			continue
		}
		pv, err := restoreVolume(v)
		if err != nil {
			return nil, nil, 0, err
		}
		if err := writeFile(filepath.Join(root, restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".yaml"), pv); err != nil {
			return nil, nil, 0, err
		}
		wrote++
		if strings.HasPrefix(v.VolumeHandle, "vol-") {
			ebs = append(ebs, v)
		}
	}

	return ebs, local, wrote, nil
}

// retainSteps writes the source-cluster commands that keep volumes the
// reclaim policy Delete would delete with their claims.
func retainSteps(p *playbook, vols []*snapshotv1alpha1.Volume) {
	var deleting []string
	for _, v := range vols {
		if v.ReclaimPolicy == "Delete" && v.PersistentVolume != "" {
			deleting = append(deleting, v.PersistentVolume)
		}
	}
	if len(deleting) > 0 {
		p.printf("**First, on the source cluster, before deleting it or its claims.**\n" +
			"These volumes are deleted with their claims (reclaim policy Delete); keep\n" +
			"them:\n\n")
		var lines []string
		for _, pv := range deleting {
			lines = append(lines, fmt.Sprintf(`kubectl --context "$SRC" patch pv %s -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'   # source cluster`, text.ShellQuote(pv)))
		}
		p.code(lines...)
	}
}

// localVolumesNote says which claims' data stays on a node's disk.
func localVolumesNote(p *playbook, local []string) {
	p.printf("%s on a node's own disk (a local or hostPath volume, e.g. kind's\n"+
		"local-path storage), which no other cluster can reach: no volume is written\n"+
		"for %s, and its claim gets a new, empty volume. Copy the data at the\n"+
		"application level if you need it (a backup and restore, or kubectl cp).\n\n",
		"The data of "+strings.Join(local, ", ")+text.Choose(len(local) == 1, " is", " are"),
		text.Choose(len(local) == 1, "it", "them"))
}

// ebsSteps writes the EBS snapshot and copy commands, in the AWS playbook
// when there is one.
func (d *restoreData) ebsSteps(p *playbook, ebs []*snapshotv1alpha1.Volume) {
	// The EBS snapshots and copies are AWS's; on another cloud the data is copied its own way.
	q := d.awsp
	if q == nil {
		q = p
	} else {
		p.printf("On AWS, %s/RESTORE.md copies the EBS volumes from snapshots and puts\n"+
			"the copies' IDs in restore/volumes/; do it before the next command.\n\n", filepath.ToSlash(filepath.Dir(d.platDir)))
	}
	if q != p {
		q.section("EBS volumes")
	}
	q.printf("**First, on the source cluster.** Snapshot the EBS volumes; each snapshot's\n" +
		"ID is saved next to its volume file for the copy below.\n\n")
	var snaps []string
	for _, v := range ebs {
		snaps = append(snaps, fmt.Sprintf(`aws ec2 create-snapshot --region "$REGION" --volume-id %s --description %s --query SnapshotId --output text > %s`,
			v.VolumeHandle, text.ShellQuote("setdrift restore "+v.Namespace+"/"+v.Claim), text.ShellQuote(snapshotFile(v))))
	}
	q.code(snaps...)
	q.printf("**EBS volumes.** Either reuse each volume (same account and region, once\n" +
		"the source no longer uses it) or create a copy from its snapshot. Copying,\n" +
		"per volume:\n\n")
	q.code(ebsCopyCommands(ebs)...)
	var autoMode []string
	for _, v := range ebs {
		if v.Driver == autoModeEBSDriver {
			autoMode = append(autoMode, v.VolumeHandle)
		}
	}
	if len(autoMode) > 0 {
		q.printf("EKS Auto Mode only attaches volumes tagged with its cluster's name. The\n" +
			"copies above are tagged; when reusing a volume instead, tag it:\n\n")
		q.code(fmt.Sprintf(`aws ec2 create-tags --region "$REGION" --resources %s --tags Key=eks:eks-cluster-name,Value="$DST_CLUSTER"`, strings.Join(autoMode, " ")))
		q.printf("If the new cluster uses another EBS CSI driver than the source (Auto Mode's\n" +
			"`ebs.csi.eks.amazonaws.com` or the add-on's `ebs.csi.aws.com`), change\n" +
			"`driver:` in the files.\n\n")
	}
}

// ebsCopyCommands returns, per volume, the commands that create a copy from
// its snapshot and put the copy's ID in its file.
func ebsCopyCommands(ebs []*snapshotv1alpha1.Volume) []string {
	var lines []string
	for _, v := range ebs {
		file := filepath.ToSlash(filepath.Join(restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".yaml"))
		az := v.Zone
		if a := v.GetAws().GetAvailabilityZone(); a != "" {
			az = a
		}
		if az == "" {
			az = "<zone>"
		}
		create := fmt.Sprintf(`NEW=$(aws ec2 create-volume --region "$REGION" --snapshot-id "$SNAP" --availability-zone %s --volume-type %s`, az, cmp.Or(v.GetAws().GetVolumeType(), "gp3"))
		if a := v.GetAws(); a != nil {
			if a.Iops > 0 && (a.VolumeType == "gp3" || strings.HasPrefix(a.VolumeType, "io")) {
				create += fmt.Sprintf(" --iops %d", a.Iops)
			}
			if a.Throughput > 0 && a.VolumeType == "gp3" {
				create += fmt.Sprintf(" --throughput %d", a.Throughput)
			}
			if a.Encrypted {
				create += " --encrypted"
				if a.KmsKeyId != "" {
					create += " --kms-key-id " + text.ShellQuote(a.KmsKeyId)
				}
			}
		}
		if v.Driver == autoModeEBSDriver {
			create += ` --tag-specifications 'ResourceType=volume,Tags=[{Key=eks:eks-cluster-name,Value='"$DST_CLUSTER"'}]'`
		}
		create += " --query VolumeId --output text)"
		lines = append(lines,
			"# "+v.Namespace+"/"+v.Claim,
			fmt.Sprintf(`SNAP=$(cat %s)`, text.ShellQuote(snapshotFile(v))),
			`aws ec2 wait snapshot-completed --region "$REGION" --snapshot-ids "$SNAP"`,
			create,
			// the IDs reach perl through its environment, so no shell expands them
			fmt.Sprintf(`OLD=%s NEW="$NEW" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/' %s`, text.ShellQuote(v.VolumeHandle), text.ShellQuote(file)))
	}
	return lines
}

// createClaimSteps writes the commands that create each volume and its
// claim, bound, before the export is applied.
func createClaimSteps(p *playbook, vols []*snapshotv1alpha1.Volume) {
	p.printf("Then create each volume and its claim, bound to each other, before\n" +
		"applying the export: a claim created by the export or by a StatefulSet\n" +
		"would otherwise get a new, empty volume. Their own field manager keeps the\n" +
		"export's apply from changing them.\n\n")
	var lines []string
	for _, ns := range namespacesOf(vols) {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" create namespace %s --dry-run=client -o yaml | kubectl --context "$DST" apply -f -`, text.ShellQuote(ns)))
	}
	lines = append(lines, `kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f restore/volumes/ --recursive`)
	p.code(lines...)
}

// snapshotFile is where the playbook saves the ID of v's EBS snapshot.
func snapshotFile(v *snapshotv1alpha1.Volume) string {
	return filepath.ToSlash(filepath.Join(restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".snapshot"))
}

// namespacesOf returns the namespaces of the volumes that are written, sorted.
func namespacesOf(vols []*snapshotv1alpha1.Volume) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vols {
		if v.VolumeHandle != "" && !seen[v.Namespace] {
			seen[v.Namespace] = true
			out = append(out, v.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// storageClasses: the classes claims ask for, which the target must have.
// It writes restore/storageclasses/ with the source's definition of each,
// including those the platform created (EKS's gp2).
func (d *restoreData) storageClasses(p *playbook, root string) error {
	used := map[string]bool{}
	for _, v := range d.s.GetInventory().GetVolumes() {
		if v.StorageClass != "" {
			used[v.StorageClass] = true
		}
	}
	for _, w := range d.written {
		for _, c := range claimClasses(w.resource, d.manifests.of(w.resource)) {
			used[c] = true
		}
	}
	if len(used) == 0 {
		return nil
	}
	defs := map[string]*snapshotv1alpha1.Resource{}
	for _, r := range d.s.GetResources() {
		if r.Kind == "StorageClass" && strings.HasPrefix(r.ApiVersion, "storage.k8s.io/") && used[r.Name] {
			defs[r.Name] = r
		}
	}
	p.section("Storage classes")
	p.printf("Claims ask for these classes by name; a claim whose class the target lacks\n" +
		"stays Pending. restore/storageclasses/ has the source's definition of each,\n" +
		"without the default-class annotation (the target keeps its own default).\n" +
		"Each line creates one only if the target lacks it:\n\n")
	var lines, missing []string
	for _, name := range sortedMapKeys(used) {
		r := defs[name]
		if r == nil {
			missing = append(missing, name)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(r.ManifestJson), &m); err != nil {
			return err
		}
		if meta, _ := m["metadata"].(map[string]any); meta != nil {
			if ann, _ := meta["annotations"].(map[string]any); ann != nil {
				delete(ann, "storageclass.kubernetes.io/is-default-class")
				delete(ann, "storageclass.beta.kubernetes.io/is-default-class")
				if len(ann) == 0 {
					delete(meta, "annotations")
				}
			}
		}
		out, err := yaml.Marshal(m)
		if err != nil {
			return err
		}
		file := filepath.ToSlash(filepath.Join(restoreDir, "storageclasses", SafeName(name)+".yaml"))
		if err := writeFile(filepath.Join(root, file), out); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" get storageclass %s >/dev/null 2>&1 || kubectl --context "$DST" apply -f %s   # provisioner %v`,
			text.ShellQuote(name), text.ShellQuote(file), m["provisioner"]))
	}
	if len(lines) > 0 {
		p.code(lines...)
	}
	if len(missing) > 0 {
		p.printf("Not in the snapshot (export without -n to include them): %s.\n\n", strings.Join(missing, ", "))
	}
	p.printf("On another platform, the class can keep its name with the target's\n" +
		"provisioner, so the claims need no change: edit `provisioner:` and\n" +
		"`parameters:` in the file (e.g. GKE `pd.csi.storage.gke.io` with `type:\n" +
		"pd-balanced`; AKS `disk.csi.azure.com` with `skuName: Premium_LRS`; EKS\n" +
		"`ebs.csi.aws.com` or Auto Mode `ebs.csi.eks.amazonaws.com` with `type: gp3`).\n" +
		"Volume data does not move between platforms this way; only the claims.\n")
	return nil
}

// claimClasses returns the storage classes a resource's claims name
// explicitly: a PersistentVolumeClaim's, or a StatefulSet's templates'.
func claimClasses(r *snapshotv1alpha1.Resource, m map[string]any) []string {
	var out []string
	switch r.Kind {
	case "PersistentVolumeClaim":
		if c := str(m, "spec", "storageClassName"); c != "" {
			out = append(out, c)
		}
	case "StatefulSet":
		templates, _, _ := unstructured.NestedSlice(m, "spec", "volumeClaimTemplates")
		for _, item := range templates {
			t, _ := item.(map[string]any)
			if c := str(t, "spec", "storageClassName"); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// accessModes is a volume's access modes, ReadWriteOnce when the
// inventory has none.
func accessModes(v *snapshotv1alpha1.Volume) []string {
	if len(v.AccessModes) == 0 {
		return []string{"ReadWriteOnce"}
	}
	return v.AccessModes
}

// restoreVolume returns a PersistentVolume for v, bound in advance to its
// claim.
func restoreVolume(v *snapshotv1alpha1.Volume) ([]byte, error) {
	driver := v.Driver
	if driver == "kubernetes.io/aws-ebs" {
		driver = "ebs.csi.aws.com"
	}
	csi := map[string]any{"driver": driver, "volumeHandle": v.VolumeHandle}
	if v.FsType != "" {
		csi["fsType"] = v.FsType
	}
	spec := map[string]any{
		"capacity":                      map[string]any{"storage": cmp.Or(v.Capacity, "1Gi")},
		"accessModes":                   accessModes(v),
		"persistentVolumeReclaimPolicy": "Retain",
		"storageClassName":              v.StorageClass,
		"csi":                           csi,
		"claimRef":                      map[string]any{"namespace": v.Namespace, "name": v.Claim},
	}
	if v.VolumeMode != "" {
		spec["volumeMode"] = v.VolumeMode
	}
	if v.Zone != "" {
		spec["nodeAffinity"] = map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{
			"matchExpressions": []any{map[string]any{"key": "topology.kubernetes.io/zone", "operator": "In", "values": []any{v.Zone}}},
		}}}}
	}
	name := v.PersistentVolume
	if name == "" {
		name = "restore-" + v.Namespace + "-" + v.Claim
	}
	out, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	})
	if err != nil {
		return nil, err
	}
	// The claim, bound to the volume by name and with the volume's class, so
	// it binds whatever the target's default class is. A StatefulSet uses an
	// existing claim of the name it would create.
	claimSpec := map[string]any{
		"accessModes":      spec["accessModes"],
		"storageClassName": v.StorageClass,
		"volumeName":       name,
		"resources":        map[string]any{"requests": map[string]any{"storage": cmp.Or(v.Capacity, "1Gi")}},
	}
	if v.VolumeMode != "" {
		claimSpec["volumeMode"] = v.VolumeMode
	}
	claim, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": v.Claim, "namespace": v.Namespace},
		"spec":       claimSpec,
	})
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Restores %s/%s with the data of volume %s (see RESTORE.md).\n"+
		"# To use a copy, replace the volume ID with the copy's.\n", v.Namespace, v.Claim, v.VolumeHandle)
	return []byte(header + string(out) + "---\n" + string(claim)), nil
}
