// Package version says which tow this is. A release build sets Version, Commit and Date
// with -ldflags (GoReleaser); any other build reads what Go recorded in the binary: the
// module's version for 'go install ...@v0.1.0', the commit for a build from a clone.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by the release build: -X github.com/towproj/tow/internal/version.Version=v0.1.0 ...
var (
	Version = ""
	Commit  = ""
	Date    = "" // the commit's time, RFC 3339: the same for every build of a release
)

// Info is this build's version, as one value.
type Info struct {
	Version, Commit, Date string
	Modified              bool // built from a tree with uncommitted changes
	Go, Platform          string
}

// Get is this build's version: the release build's, else what Go recorded, else "dev".
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date, Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := readBuildInfo(); ok {
		if i.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			i.Version = bi.Main.Version // go install github.com/towproj/tow@v0.1.0
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.Date == "" {
					i.Date = s.Value
				}
			case "vcs.modified":
				i.Modified = s.Value == "true" && Version == ""
			}
		}
	}
	if i.Version == "" {
		i.Version = "dev"
	}
	return i
}

// readBuildInfo is debug.ReadBuildInfo; swappable for tests.
var readBuildInfo = debug.ReadBuildInfo

// Short is the version alone, with "-dirty" for a build from uncommitted changes: v0.1.0.
// Go 1.24+ already stamps a clone's build v0.0.0-<time>-<commit>+dirty.
func (i Info) Short() string {
	if i.Modified && !strings.Contains(i.Version, "dirty") {
		return i.Version + "-dirty"
	}
	return i.Version
}

// String is every detail, one per line, as 'tow version' prints them.
func (i Info) String() string {
	commit := i.Commit
	if commit == "" {
		commit = "unknown"
	}
	if i.Modified {
		commit += " (with uncommitted changes)"
	}
	date := i.Date
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("tow %s\n  commit:   %s\n  built:    %s\n  go:       %s\n  platform: %s\n", i.Version, commit, date, i.Go, i.Platform)
}
