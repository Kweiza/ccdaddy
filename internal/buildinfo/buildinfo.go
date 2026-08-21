// Package buildinfo carries the version stamped in at link time.
package buildinfo

import "runtime/debug"

// Version and Commit are overridden at build time with
// -ldflags "-X github.com/Kweiza/ccdaddy/internal/buildinfo.Version=1.2.3".
var (
	Version = "dev"
	Commit  = ""
)

// String renders the version for `ccdad --version`. When the binary was built
// without ldflags (go install, go run), Go stamps VCS data into BuildInfo, so
// fall back to that rather than reporting nothing useful.
func String() string {
	v := Version
	c := Commit
	if c == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					c = s.Value
					break
				}
			}
		}
	}
	if c == "" {
		return v
	}
	if len(c) > 12 {
		c = c[:12]
	}
	return v + " (" + c + ")"
}
