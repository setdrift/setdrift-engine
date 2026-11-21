package version

import (
	"runtime/debug"
	"testing"
)

// -ldflags values win; otherwise Go's build information fills in; "dev"
// and "none" when nothing knows.
func TestResolve(t *testing.T) {
	vcs := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	for _, c := range []struct {
		name                    string
		info                    *debug.BuildInfo
		version, commit         string
		wantVersion, wantCommit string
	}{
		{"ldflags set both", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: vcs("0123456789abcdef", "false")},
			"v0.1.0", "abc1234", "v0.1.0", "abc1234"},
		{"go install @v0.1.0", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			"", "", "v0.1.0", "none"},
		{"build in a checkout", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261111120000-0123456789ab+dirty"}, Settings: vcs("0123456789abcdef", "true")},
			"", "", "v0.0.0-20261111120000-0123456789ab+dirty", "0123456-dirty"},
		{"ldflags commit, no tag", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("0123456789abcdef", "false")},
			"", "abc1234", "dev", "abc1234"},
		{"nothing known", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "", "", "dev", "none"},
		{"no build info", nil, "", "", "dev", "none"},
	} {
		v, cm := resolve(c.info, c.version, c.commit)
		if v != c.wantVersion || cm != c.wantCommit {
			t.Errorf("%s: %q %q, want %q %q", c.name, v, cm, c.wantVersion, c.wantCommit)
		}
	}
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
