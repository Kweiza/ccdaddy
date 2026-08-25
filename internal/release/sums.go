package release

import "regexp"

// MinAssetBytes is the floor a downloaded asset has to clear.
//
// Every released ccdad is over three megabytes on all six targets, and nothing
// under a megabyte is one — it is a proxy's error page, a captive-portal login,
// or a truncated transfer. install.sh applies the same figure for the same
// reason, and the two must not drift: a floor here that is lower than the
// installer's would let `update` accept what a fresh install refuses.
const MinAssetBytes = 1_000_000

// sumsShapeRe is one sha256sum row: 64 lowercase hex digits and TWO spaces.
// Two is exactly what sha256sum(1) and macOS `shasum -a 256` emit; one matches
// nothing.
var sumsShapeRe = regexp.MustCompile(`(?m)^[0-9a-f]{64}  `)

// SumsLookLikeSums reports whether b contains at least one checksum row.
//
// It is the check that catches a proxy's HTML error page BEFORE the bytes are
// treated as data. Per line rather than over the whole file, because the
// release's sums file also covers LICENSE, NOTICE and THIRD-PARTY-LICENSES.txt
// — a release is a binary distribution and those notices are part of it — so
// nothing here may depend on the line count or the ordering.
func SumsLookLikeSums(b []byte) bool { return sumsShapeRe.Match(b) }

// ExpectedHash returns the lowercase hex digest sums records for asset.
//
// Anchored at BOTH ends, and the asset name quoted. A missing trailing anchor
// lets ccdad-linux-amd64 match the ccdad-linux-amd64.exe row — a different
// platform's binary, with a digest that would then verify. Quoting matters
// because the asset name is data here and not a pattern.
//
// Comparison downstream is case-sensitive against this value, never EqualFold:
// the class above is lowercase, and a fold would accept a row an attacker
// re-cased.
//
// A CRLF file does not match, and that is the answer rather than a gap. Neither
// sha256sum nor shasum emits one, and accepting it would mean accepting a row
// whose asset name ends in a carriage return.
//
// ok=false is "not listed" and is a DIFFERENT refusal from a file that fails
// SumsLookLikeSums. The caller must ask both questions, in that order.
func ExpectedHash(sums []byte, asset string) (string, bool) {
	re, err := regexp.Compile(`(?m)^([0-9a-f]{64})  ` + regexp.QuoteMeta(asset) + `$`)
	if err != nil {
		// Unreachable while QuoteMeta is used, and returning "not listed" is
		// the fail-closed answer if it ever is not.
		return "", false
	}
	m := re.FindSubmatch(sums)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}
