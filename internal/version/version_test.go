package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func useBuildInfo(t *testing.T, bi *debug.BuildInfo) {
	t.Helper()
	orig := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
	t.Cleanup(func() { readBuildInfo = orig })
}

func useRelease(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := Version, Commit, Date
	Version, Commit, Date = v, c, d
	t.Cleanup(func() { Version, Commit, Date = ov, oc, od })
}

func vcs(rev, at, modified string) []debug.BuildSetting {
	return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.time", Value: at}, {Key: "vcs.modified", Value: modified}}
}

func TestGet(t *testing.T) {
	// A release build: ldflags win, and its tree was clean by definition.
	useRelease(t, "v0.1.0", "abc123", "2026-12-01T10:00:00Z")
	useBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("other", "2020-01-01T00:00:00Z", "true")})
	if i := Get(); i.Version != "v0.1.0" || i.Commit != "abc123" || i.Date != "2026-12-01T10:00:00Z" || i.Modified || i.Short() != "v0.1.0" {
		t.Errorf("release: %+v", i)
	}

	// go install ...@v0.1.0: the module's version.
	useRelease(t, "", "", "")
	useBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}})
	if i := Get(); i.Version != "v0.1.0" || i.Commit != "" {
		t.Errorf("go install: %+v", i)
	}

	// go build in a clone with changes: dev, its commit, dirty.
	useBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("def456", "2026-11-15T20:00:00Z", "true")})
	i := Get()
	if i.Version != "dev" || i.Commit != "def456" || !i.Modified || i.Short() != "dev-dirty" {
		t.Errorf("clone: %+v", i)
	}
	if s := i.String(); !strings.HasPrefix(s, "tow dev\n") || !strings.Contains(s, "def456 (with uncommitted changes)") || !strings.Contains(s, "platform: ") {
		t.Errorf("String:\n%s", s)
	}

	// Go 1.24+ stamps a clone's build itself: its "+dirty" isn't doubled.
	useBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261115212537-c30f7fa6c7df+dirty"}, Settings: vcs("c30f7fa", "2026-11-15T21:25:37Z", "true")})
	if i := Get(); i.Short() != "v0.0.0-20261115212537-c30f7fa6c7df+dirty" {
		t.Errorf("stamped: %q", i.Short())
	}

	// Nothing recorded at all.
	useBuildInfo(t, nil)
	if i := Get(); i.Version != "dev" || !strings.Contains(i.String(), "commit:   unknown") {
		t.Errorf("bare: %+v", i)
	}
}
