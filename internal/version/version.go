// Package version holds the build information, set with -ldflags by the Makefile and release builds, or else read from what Go records in every binary
// that way a `go install …@v0.1.0` build reports its version too
package version

import "runtime/debug"

// Version and Commit are variables, not constants, because the linker's -X flag can only set variables
// empty means not set at build time, init fills them in
var (
	// Version is the release, e.g. v0.1.0, shown by `setdrift version`, stamped on every snapshot and sent as the API user agent
	Version = ""
	// Commit is the short git commit the binary was built from
	Commit = ""
)

// init fills in Version and Commit from Go's build information when -ldflags left them empty
func init() {
	info, _ := debug.ReadBuildInfo()
	// invoking resolve to settle both values, its defined below
	Version, Commit = resolve(info, Version, Commit)
}

// resolve keeps what -ldflags set and fills in the rest from Go's build information, the module version and the vcs commit
// the module version is the go install …@v0.1.0 one, or Go's own pseudo-version for a build in a git checkout
// the commit gets -dirty for uncommitted changes, and its "dev" and "none" when neither knows
func resolve(info *debug.BuildInfo, setVersion, setCommit string) (version, commit string) {
	version, commit = setVersion, setCommit
	// checking if -ldflags left the version empty and Go recorded build information to fill it from
	if version == "" && info != nil {
		// checking if Go knows a real version, "(devel)" is what it records when it knows none
		if v := info.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}
	// checking if -ldflags left the commit empty and Go recorded build information to fill it from
	if commit == "" && info != nil {
		revision, modified := "", false
		// going through the build settings for the vcs revision and whether the checkout had changes
		for _, s := range info.Settings {
			// checking which setting this is, only the two vcs ones matter
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		// checking if the revision is longer than 7 characters, if so it gets cut to the short form
		if len(revision) > 7 {
			revision = revision[:7]
		}
		// checking if theres a revision and the checkout had uncommitted changes, if so it gets -dirty
		if revision != "" && modified {
			revision += "-dirty"
		}
		commit = revision
	}
	// checking if nothing knew the version, if so its "dev"
	if version == "" {
		version = "dev"
	}
	// checking if nothing knew the commit, if so its "none"
	if commit == "" {
		commit = "none"
	}
	return version, commit
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
