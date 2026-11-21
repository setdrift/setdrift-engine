// writes RESTORE.md's storage steps, the storage classes claims ask for and the volumes to bring over

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

// volumes writes the step that brings the volumes' data over, and restore/volumes/
// it tells if it wrote any volume, so the apply step can say it comes after them
func (d *restoreData) volumes(p *playbook, out *outDir) (bool, error) {
	vols := d.s.GetInventory().GetVolumes()
	// checking if the inventory has no volumes, if so theres no step
	if len(vols) == 0 {
		return false, nil
	}
	p.section("Volumes")
	p.printf("Applying the export gives each claim a new, empty volume.")
	// checking if any volume is a csi volume another cluster can reach, if so the step says how to keep the data
	if slices.ContainsFunc(vols, func(v *snapshotv1alpha1.Volume) bool { return v.VolumeHandle != "" && v.Driver != "" }) {
		p.printf(" To keep the data,\n" +
			"bring each volume over before applying: restore/volumes/ has a\n" +
			"PersistentVolume for each that another cluster can reach, bound in advance\n" +
			"to its claim, so the claim (or the StatefulSet that creates it) gets the\n" +
			"volume with the data.")
	}
	p.printf("\n\n")
	p.printf("| Claim | Size | Class | Volume | Zone | Reclaim |\n|---|---|---|---|---|---|\n")
	// writing a table row per volume, cell is defined in manifest.go
	for _, v := range vols {
		claim := v.Namespace + "/" + v.Claim
		// checking if a StatefulSet created the claim, if so the row names it
		if v.StatefulSet != "" {
			claim += " (StatefulSet " + v.StatefulSet + ")"
		}
		p.printf("| %s | %s | %s | `%s` | %s | %s |\n", cell(claim), cell(v.Capacity), cell(v.StorageClass), cell(cmp.Or(v.VolumeHandle, "-")), cell(cmp.Or(v.Zone, "-")), cell(v.ReclaimPolicy))
	}
	p.printf("\n")

	// invoking writeVolumes to write restore/volumes/ and sort the volumes by what they need, its defined below
	ebs, local, wrote, err := writeVolumes(out, vols)
	if err != nil {
		return false, err
	}
	// invoking retainSteps so the source keeps volumes its claims would delete, its defined below
	retainSteps(p, vols)
	// checking if some data stays on a node, if so localVolumesNote says so, its defined below
	if len(local) > 0 {
		localVolumesNote(p, local)
	}
	// checking if there are EBS volumes, if so ebsSteps writes their snapshot and copy commands, its defined below
	if len(ebs) > 0 {
		d.ebsSteps(p, ebs)
	}
	// checking if any volume file was written, if so createClaimSteps writes the commands that create them, its defined below
	if wrote > 0 {
		createClaimSteps(p, vols)
	}
	return wrote > 0, nil
}

// writeVolumes writes restore/volumes/, a volume and its claim for each volume another cluster can reach
// it gives back the EBS volumes, the claims whose data stays on a node, and how many it wrote
func writeVolumes(out *outDir, vols []*snapshotv1alpha1.Volume) (ebs []*snapshotv1alpha1.Volume, local []string, wrote int, err error) {
	// going through the volumes, local collects the claims whose volume isnt a csi volume (hostPath, local), so its data stays on a node
	for _, v := range vols {
		// checking if its not a csi volume, if so theres no file to write for it
		if v.VolumeHandle == "" || v.Driver == "" {
			// checking if the PersistentVolume was read (its reclaim policy comes only from it) and has no driver, if so its hostPath or local
			if v.PersistentVolume != "" && v.ReclaimPolicy != "" && v.Driver == "" {
				local = append(local, v.Namespace+"/"+v.Claim)
			}
			continue
		}
		// invoking restoreVolume for the volume and claim yaml, its defined below
		pv, err := restoreVolume(v)
		if err != nil {
			return nil, nil, 0, err
		}
		// invoking out.write to save it under restore/volumes/<namespace>/<claim>.yaml, its defined in kustomize.go, SafeName too
		if err := out.write(filepath.Join(restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".yaml"), pv); err != nil {
			return nil, nil, 0, err
		}
		wrote++
		// checking if the handle is an EBS volume id, if so it gets the EBS snapshot and copy steps
		if strings.HasPrefix(v.VolumeHandle, "vol-") {
			ebs = append(ebs, v)
		}
	}

	return ebs, local, wrote, nil
}

// retainSteps writes the source cluster commands that keep the volumes the reclaim policy Delete would delete with their claims
func retainSteps(p *playbook, vols []*snapshotv1alpha1.Volume) {
	var deleting []string
	// collecting the volumes with reclaim policy Delete
	for _, v := range vols {
		// checking if the volume goes with its claim and its name is known
		if v.ReclaimPolicy == "Delete" && v.PersistentVolume != "" {
			deleting = append(deleting, v.PersistentVolume)
		}
	}
	// checking if any would be deleted, if so they get a patch to Retain, text.ShellQuote is defined in internal/text/text.go
	if len(deleting) > 0 {
		p.printf("**First, on the source cluster, before deleting it or its claims.**\n" +
			"These volumes are deleted with their claims (reclaim policy Delete); keep\n" +
			"them:\n\n")
		var lines []string
		// building one patch command per volume
		for _, pv := range deleting {
			lines = append(lines, fmt.Sprintf(`kubectl --context "$SRC" patch pv %s -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'   # source cluster`, text.ShellQuote(pv)))
		}
		p.code(lines...)
	}
}

// localVolumesNote says which claims' data stays on a node's disk, where no other cluster can reach it
func localVolumesNote(p *playbook, local []string) {
	// text.Choose picks the singular or plural wording, its defined in internal/text/text.go
	p.printf("%s on a node's own disk (a local or hostPath volume, e.g. kind's\n"+
		"local-path storage), which no other cluster can reach: no volume is written\n"+
		"for %s, and its claim gets a new, empty volume. Copy the data at the\n"+
		"application level if you need it (a backup and restore, or kubectl cp).\n\n",
		"The data of "+strings.Join(local, ", ")+text.Choose(len(local) == 1, " is", " are"),
		text.Choose(len(local) == 1, "it", "them"))
}

// ebsSteps writes the EBS snapshot and copy commands, in the aws playbook when there is one
func (d *restoreData) ebsSteps(p *playbook, ebs []*snapshotv1alpha1.Volume) {
	// the EBS snapshots and copies are aws's, on another cloud the data is copied its own way
	q := d.awsp
	// checking if theres no aws playbook, if so the commands go in RESTORE.md itself, otherwise RESTORE.md points to the aws one
	if q == nil {
		q = p
	} else {
		p.printf("On AWS, %s/RESTORE.md copies the EBS volumes from snapshots and puts\n"+
			"the copies' IDs in restore/volumes/; do it before the next command.\n\n", filepath.ToSlash(filepath.Dir(d.platDir)))
	}
	// checking if the commands go in the aws playbook, if so they get a step of their own there
	if q != p {
		q.section("EBS volumes")
	}
	q.printf("**First, on the source cluster.** Snapshot the EBS volumes; each snapshot's\n" +
		"ID is saved next to its volume file for the copy below.\n\n")
	var snaps []string
	// building one create-snapshot command per volume, snapshotFile is defined below
	for _, v := range ebs {
		snaps = append(snaps, fmt.Sprintf(`aws ec2 create-snapshot --region "$REGION" --volume-id %s --description %s --query SnapshotId --output text > %s`,
			text.ShellQuote(v.VolumeHandle), text.ShellQuote("setdrift restore "+v.Namespace+"/"+v.Claim), text.ShellQuote(snapshotFile(v))))
	}
	q.code(snaps...)
	q.printf("**EBS volumes.** Either reuse each volume (same account and region, once\n" +
		"the source no longer uses it) or create a copy from its snapshot. Copying,\n" +
		"per volume:\n\n")
	// invoking ebsCopyCommands for the copy commands, its defined below
	q.code(ebsCopyCommands(ebs)...)
	var autoMode []string
	// collecting the volumes of EKS Auto Mode's driver
	for _, v := range ebs {
		// checking if the volume uses the Auto Mode driver
		if v.Driver == autoModeEBSDriver {
			autoMode = append(autoMode, v.VolumeHandle)
		}
	}
	// checking if any are Auto Mode volumes, if so a reused one needs the cluster name tag, text.ShellQuoteAll is defined in internal/text/text.go
	if len(autoMode) > 0 {
		q.printf("EKS Auto Mode only attaches volumes tagged with its cluster's name. The\n" +
			"copies above are tagged; when reusing a volume instead, tag it:\n\n")
		q.code(fmt.Sprintf(`aws ec2 create-tags --region "$REGION" --resources %s --tags Key=eks:eks-cluster-name,Value="$DST_CLUSTER"`, strings.Join(text.ShellQuoteAll(autoMode), " ")))
		q.printf("If the new cluster uses another EBS CSI driver than the source (Auto Mode's\n" +
			"`ebs.csi.eks.amazonaws.com` or the add-on's `ebs.csi.aws.com`), change\n" +
			"`driver:` in the files.\n\n")
	}
}

// ebsCopyCommands gives back, per volume, the commands that create a copy from its snapshot and put the copy's id in its file
func ebsCopyCommands(ebs []*snapshotv1alpha1.Volume) []string {
	var lines []string
	// building the commands for each volume
	for _, v := range ebs {
		file := filepath.ToSlash(filepath.Join(restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".yaml"))
		az := v.Zone
		// checking if aws recorded the availability zone, if so it wins over the kubernetes zone
		if a := v.GetAws().GetAvailabilityZone(); a != "" {
			az = a
		}
		// checking if no zone is known, if so the command gets a placeholder
		if az == "" {
			az = "<zone>"
		}
		create := fmt.Sprintf(`NEW=$(aws ec2 create-volume --region "$REGION" --snapshot-id "$SNAP" --availability-zone %s --volume-type %s`,
			text.ShellQuote(az), text.ShellQuote(cmp.Or(v.GetAws().GetVolumeType(), "gp3")))
		// checking if aws details were read, if so the copy keeps the source's performance and encryption
		if a := v.GetAws(); a != nil {
			// checking if the volume had set iops on a type that takes them, gp3 or io
			if a.Iops > 0 && (a.VolumeType == "gp3" || strings.HasPrefix(a.VolumeType, "io")) {
				create += fmt.Sprintf(" --iops %d", a.Iops)
			}
			// checking if a gp3 volume had set throughput
			if a.Throughput > 0 && a.VolumeType == "gp3" {
				create += fmt.Sprintf(" --throughput %d", a.Throughput)
			}
			// checking if the volume was encrypted, if so the copy is too
			if a.Encrypted {
				create += " --encrypted"
				// checking if it used its own kms key, if so the copy uses the same one
				if a.KmsKeyId != "" {
					create += " --kms-key-id " + text.ShellQuote(a.KmsKeyId)
				}
			}
		}
		// checking if its an Auto Mode volume, if so the copy gets the cluster name tag Auto Mode needs to attach it
		if v.Driver == autoModeEBSDriver {
			create += ` --tag-specifications 'ResourceType=volume,Tags=[{Key=eks:eks-cluster-name,Value='"$DST_CLUSTER"'}]'`
		}
		create += " --query VolumeId --output text)"
		// comment is defined in manifest.go
		lines = append(lines,
			"# "+comment(v.Namespace+"/"+v.Claim),
			fmt.Sprintf(`SNAP=$(cat %s)`, text.ShellQuote(snapshotFile(v))),
			`aws ec2 wait snapshot-completed --region "$REGION" --snapshot-ids "$SNAP"`,
			create,
			// the ids reach perl through its environment, so no shell expands them
			fmt.Sprintf(`OLD=%s NEW="$NEW" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/' %s`, text.ShellQuote(v.VolumeHandle), text.ShellQuote(file)))
	}
	return lines
}

// createClaimSteps writes the commands that create each volume and its claim, bound, before the export is applied
func createClaimSteps(p *playbook, vols []*snapshotv1alpha1.Volume) {
	p.printf("Then create each volume and its claim, bound to each other, before\n" +
		"applying the export: a claim created by the export or by a StatefulSet\n" +
		"would otherwise get a new, empty volume. Their own field manager keeps the\n" +
		"export's apply from changing them.\n\n")
	var lines []string
	// creating each namespace first if its missing, namespacesOf is defined below
	for _, ns := range namespacesOf(vols) {
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" create namespace %s --dry-run=client -o yaml | kubectl --context "$DST" apply -f -`, text.ShellQuote(ns)))
	}
	lines = append(lines, `kubectl --context "$DST" apply --server-side --field-manager=setdrift-restore -f restore/volumes/ --recursive`)
	p.code(lines...)
}

// snapshotFile gives back where the playbook saves the id of v's EBS snapshot, next to its volume file
func snapshotFile(v *snapshotv1alpha1.Volume) string {
	return filepath.ToSlash(filepath.Join(restoreDir, "volumes", SafeName(v.Namespace), SafeName(v.Claim)+".snapshot"))
}

// namespacesOf gives back the namespaces of the volumes that get written, sorted and each once
func namespacesOf(vols []*snapshotv1alpha1.Volume) []string {
	seen := map[string]bool{}
	var out []string
	// collecting each written volume's namespace once
	for _, v := range vols {
		// checking if the volume gets written and its namespace isnt listed yet
		if v.VolumeHandle != "" && !seen[v.Namespace] {
			seen[v.Namespace] = true
			out = append(out, v.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// storageClasses writes the step for the classes claims ask for, which the target must have
// it writes restore/storageclasses/ with the source's definition of each, including those the platform created (EKS's gp2)
func (d *restoreData) storageClasses(p *playbook, out *outDir) error {
	used := map[string]bool{}
	// collecting the classes the inventory's volumes use
	for _, v := range d.s.GetInventory().GetVolumes() {
		// checking if the volume has a class
		if v.StorageClass != "" {
			used[v.StorageClass] = true
		}
	}
	// collecting the classes the written claims and StatefulSet templates name
	for _, w := range d.written {
		// invoking claimClasses on each decoded manifest, its defined below, of is defined in manifest.go
		for _, c := range claimClasses(w.resource, d.manifests.of(w.resource)) {
			used[c] = true
		}
	}
	// checking if no class is used, if so theres no step
	if len(used) == 0 {
		return nil
	}
	defs := map[string]*snapshotv1alpha1.Resource{}
	// finding the snapshot's definition of each used class
	for _, r := range d.s.GetResources() {
		// checking if its a StorageClass one of the claims uses
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
	// writing each class's file and its create command, sortedMapKeys is defined in restore.go
	for _, name := range sortedMapKeys(used) {
		r := defs[name]
		// checking if the snapshot has no definition of it, if so its listed as missing
		if r == nil {
			missing = append(missing, name)
			continue
		}
		// decoding its own copy of the manifest, since the annotations get removed below and manifests.of copies must not change
		var m map[string]any
		if err := json.Unmarshal([]byte(r.ManifestJson), &m); err != nil {
			return err
		}
		// checking if it has metadata and annotations, if so the default-class annotations come out so the target keeps its own default
		if meta, _ := m["metadata"].(map[string]any); meta != nil {
			// checking if there are annotations to clean
			if ann, _ := meta["annotations"].(map[string]any); ann != nil {
				delete(ann, "storageclass.kubernetes.io/is-default-class")
				delete(ann, "storageclass.beta.kubernetes.io/is-default-class")
				// checking if no annotation is left, if so the empty map goes too
				if len(ann) == 0 {
					delete(meta, "annotations")
				}
			}
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			return err
		}
		file := filepath.ToSlash(filepath.Join(restoreDir, "storageclasses", SafeName(name)+".yaml"))
		// invoking out.write to save the class, its defined in kustomize.go
		if err := out.write(filepath.FromSlash(file), b); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf(`kubectl --context "$DST" get storageclass %s >/dev/null 2>&1 || kubectl --context "$DST" apply -f %s   # provisioner %s`,
			text.ShellQuote(name), text.ShellQuote(file), comment(fmt.Sprint(m["provisioner"]))))
	}
	// checking if any class file was written, if so the commands get a shell block
	if len(lines) > 0 {
		p.code(lines...)
	}
	// checking if some classes have no definition, if so the step names them
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

// claimClasses gives back the storage classes a resource's claims name explicitly, a PersistentVolumeClaim's or a StatefulSet's templates'
func claimClasses(r *snapshotv1alpha1.Resource, m map[string]any) []string {
	var out []string
	// checking which kind it is, only claims and StatefulSets name classes
	switch r.Kind {
	case "PersistentVolumeClaim":
		// checking if the claim names a class, str is defined in manifest.go
		if c := str(m, "spec", "storageClassName"); c != "" {
			out = append(out, c)
		}
	case "StatefulSet":
		templates, _, _ := unstructured.NestedSlice(m, "spec", "volumeClaimTemplates")
		// going through the claim templates for the classes they name
		for _, item := range templates {
			t, _ := item.(map[string]any)
			// checking if the template names a class
			if c := str(t, "spec", "storageClassName"); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// accessModes gives back a volume's access modes, ReadWriteOnce when the inventory has none
func accessModes(v *snapshotv1alpha1.Volume) []string {
	// checking if the inventory recorded no access modes, if so ReadWriteOnce stands in
	if len(v.AccessModes) == 0 {
		return []string{"ReadWriteOnce"}
	}
	return v.AccessModes
}

// restoreVolume gives back the yaml for a PersistentVolume for v, bound in advance to its claim, and the claim itself
func restoreVolume(v *snapshotv1alpha1.Volume) ([]byte, error) {
	driver := v.Driver
	// checking if its the old in-tree EBS driver, if so the csi driver takes its place
	if driver == "kubernetes.io/aws-ebs" {
		driver = "ebs.csi.aws.com"
	}
	csi := map[string]any{"driver": driver, "volumeHandle": v.VolumeHandle}
	// checking if the volume recorded a filesystem type, if so the new one keeps it
	if v.FsType != "" {
		csi["fsType"] = v.FsType
	}
	// the volume keeps its data when released (Retain) and is reserved for its claim through claimRef
	spec := map[string]any{
		"capacity":                      map[string]any{"storage": cmp.Or(v.Capacity, "1Gi")},
		"accessModes":                   accessModes(v),
		"persistentVolumeReclaimPolicy": "Retain",
		"storageClassName":              v.StorageClass,
		"csi":                           csi,
		"claimRef":                      map[string]any{"namespace": v.Namespace, "name": v.Claim},
	}
	// checking if the volume recorded a volume mode, like Block
	if v.VolumeMode != "" {
		spec["volumeMode"] = v.VolumeMode
	}
	// checking if the volume is in a zone, if so pods using it are kept to that zone
	if v.Zone != "" {
		spec["nodeAffinity"] = map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{
			"matchExpressions": []any{map[string]any{"key": "topology.kubernetes.io/zone", "operator": "In", "values": []any{v.Zone}}},
		}}}}
	}
	name := v.PersistentVolume
	// checking if the source volume's name isnt known, if so one is made from the claim
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
	// the claim is bound to the volume by name and carries the volume's class, so it binds whatever the target's default class is
	// a StatefulSet uses an existing claim of the name it would create
	claimSpec := map[string]any{
		"accessModes":      spec["accessModes"],
		"storageClassName": v.StorageClass,
		"volumeName":       name,
		"resources":        map[string]any{"requests": map[string]any{"storage": cmp.Or(v.Capacity, "1Gi")}},
	}
	// checking if theres a volume mode, if so the claim asks for the same one
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
	// the file header says what the file restores and how to use a copy instead
	header := fmt.Sprintf("# Restores %s/%s with the data of volume %s (see RESTORE.md).\n"+
		"# To use a copy, replace the volume ID with the copy's.\n", v.Namespace, v.Claim, v.VolumeHandle)
	return []byte(header + string(out) + "---\n" + string(claim)), nil
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
