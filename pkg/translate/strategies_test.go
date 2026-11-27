package translate

import (
	"testing"

	"setdrift.io/setdrift-engine/pkg/capability"
)

// every strategy the capability map takes has an implementation here, so a platform or cloud naming one never gets nothing
// a new strategy name in capability.Strategies fails this test until translate implements it
func TestStrategiesImplemented(t *testing.T) {
	implemented := map[string]func(string) bool{
		"identity.federation": func(s string) bool {
			// projected-token is injectFederation's, credential-file has the user write the file, from RESTORE.md's trust step
			return s == capability.ProjectedToken || s == capability.CredentialFile
		},
		"identity.trust":          func(s string) bool { return trusts[s] != nil },
		"secretManager.format":    func(s string) bool { return secretFormats[s] != nil },
		"buckets":                 func(s string) bool { return bucketWriters[s] != nil },
		"translate.images":        func(s string) bool { return imageCopies[s] != nil },
		"translate.createCommand": func(s string) bool { return createCommands[s] != nil },
	}
	for kind, names := range capability.Strategies {
		has, ok := implemented[kind]
		if !ok {
			t.Errorf("strategy kind %s has no check here: add it, with the table translate implements it in", kind)
			continue
		}
		for _, n := range names {
			if n != capability.None && !has(n) {
				t.Errorf("%s %q: the capability map takes it, and translate doesn't implement it", kind, n)
			}
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
