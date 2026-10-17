// Package buildinfo holds the version stamped in at release time:
//
//	go build -ldflags "-X github.com/notepid/ham-bbs/internal/buildinfo.Version=v1.2.3"
package buildinfo

import "runtime/debug"

// Version is the release version, or "dev" for local builds.
var Version = "dev"

// String returns the version, with the VCS revision for dev builds.
func String() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return Version
}
