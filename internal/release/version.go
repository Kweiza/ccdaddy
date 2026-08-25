// Package release holds everything about WHERE ccdad releases live: how a tag
// is spelled and ordered, what this machine's asset is called, how the
// published sha256sums.txt is read, and the HTTP client that reads them.
//
// It exists because both installers hard-code those rules — install.sh in
// shell, install.ps1 in PowerShell — and a third, divergent copy in Go would be
// a verification hole rather than a duplication. `ccdad update` and the daemon's
// version check share this one.
package release

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
)

// Version is a release version. Only the three numbers are ordered; Pre is
// carried so a prerelease tag round-trips and sorts below the release of the
// same numbers.
type Version struct {
	Major, Minor, Patch int
	// Pre keeps its leading '-', so String can append it unconditionally.
	Pre string
}

// tagRe is the whole grammar, anchored at both ends.
//
// Each digit run is bounded at nine characters. That is not a limit on
// versions — no release will ever carry ten digits in one field — it is a
// bound on remote-controlled input reaching strconv.Atoi: this string arrives
// in a Location header an origin chooses, and an unbounded run of digits
// overflows a 32-bit int.
//
// Build metadata (`+…`) is deliberately not accepted. No ccdad release has ever
// carried one, and a field nothing produces is a field an origin can smuggle
// something through.
var tagRe = regexp.MustCompile(`^v?([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})(-[0-9A-Za-z.-]+)?$`)

// ParseTag reads a release tag. It accepts "1.2.3", "v1.2.3" and surrounding
// whitespace, so that `--version 1.2.3` and `--version v1.2.3` are the same
// request. Without that, a user's missing v fails later as "this signature is
// for a different release" — the tamper remedy, for a typo.
func ParseTag(s string) (Version, bool) {
	m := tagRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	// Bounded above, so none of these can fail.
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch, Pre: m[4]}, true
}

// Compare orders two versions: -1, 0 or 1.
func (v Version) Compare(o Version) int {
	if c := cmp.Compare(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Patch, o.Patch); c != 0 {
		return c
	}
	// A prerelease sorts BELOW the release of the same numbers: v1.2.3-rc.1 is
	// not v1.2.3, and a machine on the release must not be offered the rc as an
	// upgrade.
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	// Two prereleases of ONE version are compared as plain strings rather than
	// by semver's dot-separated identifier rules. That gets rc.1 < rc.2 right
	// and rc.10 wrong, and it is written down rather than discovered: ccdad has
	// never published two prereleases of one version, and the `latest` endpoint
	// excludes prereleases entirely, so the only way to reach this line is a
	// user typing both tags by hand.
	return cmp.Compare(v.Pre, o.Pre)
}

// String renders the version the way buildinfo.Version is spelled: "0.7.0",
// never a leading v.
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch) + v.Pre
}

// Tag renders the release tag: "v0.7.0". This is the form the trusted comment
// binds and the form the download path carries.
func (v Version) Tag() string { return "v" + v.String() }
