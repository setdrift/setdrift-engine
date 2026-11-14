// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0

// Package version holds build information: set with -ldflags by the
// Makefile and release builds, or else read from what Go records in every
// binary, so a `go install …@v0.1.0` build reports its version too.
package version

import "runtime/debug"

// Variables, not constants, because the linker's -X flag can only set
// variables. Empty means not set at build time; init fills them in.
var (
	// Version is the release, e.g. v0.1.0, shown by `setdrift version`,
	// stamped on every snapshot and sent as the API user agent.
	Version = ""
	// Commit is the short git commit the binary was built from.
	Commit = ""
)

func init() {
	info, _ := debug.ReadBuildInfo()
	Version, Commit = resolve(info, Version, Commit)
}

// resolve keeps what -ldflags set, and fills in the rest from Go's build
// information: the module version (go install …@v0.1.0, or Go's own
// pseudo-version for a build in a git checkout) and the VCS commit, with
// -dirty for uncommitted changes. "dev" and "none" when neither knows.
func resolve(info *debug.BuildInfo, version, commit string) (string, string) {
	if version == "" && info != nil {
		// "(devel)" is what Go records when it knows no version
		if v := info.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}
	if commit == "" && info != nil {
		revision, modified := "", false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if revision != "" && modified {
			revision += "-dirty"
		}
		commit = revision
	}
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "none"
	}
	return version, commit
}
