package release

import (
	"os"
	"runtime"
	"strings"
)

// defaultBase is the origin install.sh already defaults to. It is spelled here
// rather than assembled from a slug constant so that a reader of this file sees
// the whole URL a release is fetched from.
const defaultBase = "https://github.com/Kweiza/ccdaddy/releases"

// BaseURL is where releases are fetched from, honouring the variable
// install.sh and install.ps1 already read. It is one seam and not two: the
// command and the daemon's version check both go through here, so a daemon
// cannot ignore a test origin the CLI was pointed at.
//
// A trailing slash is trimmed rather than tolerated, because every caller joins
// a path onto this and a doubled separator is a 404 nobody can explain.
func BaseURL() string {
	if v := os.Getenv("CCDAD_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultBase
}

// DownloadBase is where one release's assets live.
func DownloadBase(base, tag string) string {
	return strings.TrimRight(base, "/") + "/download/" + tag
}

// Asset names the release asset for the build this binary IS.
//
// runtime.GOOS and runtime.GOARCH, with one consequence worth stating: an amd64
// binary running under Rosetta or Windows-on-ARM updates to amd64 forever,
// while install.ps1's own detection would pick arm64. So `update` and the
// installer disagree exactly on the machines emulation exists for, and the
// README names the installer as the way to change architecture.
func Asset() string { return assetName(runtime.GOOS, runtime.GOARCH) }

// assetName is Asset with the platform as a parameter, so all six targets are
// testable from the one machine a test runs on.
func assetName(goos, goarch string) string {
	name := "ccdad-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}
