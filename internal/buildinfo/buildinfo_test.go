package buildinfo

import (
	"strings"
	"testing"
)

// String feeds `ccdad --version`, which is a documented surface, and it has
// three branches none of which were asserted: the vcs fallback, the twelve-
// character truncation, and the bare-version case.
func TestString(t *testing.T) {
	saveV, saveC := Version, Commit
	t.Cleanup(func() { Version, Commit = saveV, saveC })

	Version, Commit = "1.2.3", "0123456789abcdef0123"
	if got, want := String(), "1.2.3 (0123456789ab)"; got != want {
		t.Errorf("String() = %q, want %q — the commit is truncated to twelve characters", got, want)
	}

	Version, Commit = "1.2.3", "short"
	if got, want := String(), "1.2.3 (short)"; got != want {
		t.Errorf("String() = %q, want %q — a commit already under twelve characters is left alone", got, want)
	}
}

// With no commit stamped in, String falls back to whatever Go recorded about
// the build. Under `go test` that is a real VCS revision, so the assertion is
// on the shape rather than on a value: either a bare version, or the version
// plus a parenthesised revision of at most twelve characters.
func TestStringWithoutAStampedCommit(t *testing.T) {
	saveV, saveC := Version, Commit
	t.Cleanup(func() { Version, Commit = saveV, saveC })

	Version, Commit = "dev", ""
	got := String()
	if got == "dev" {
		return // no VCS data available in this build; the bare-version branch
	}
	if !strings.HasPrefix(got, "dev (") || !strings.HasSuffix(got, ")") {
		t.Fatalf("String() = %q, want either %q or %q with a revision", got, "dev", "dev (<rev>)")
	}
	rev := strings.TrimSuffix(strings.TrimPrefix(got, "dev ("), ")")
	if rev == "" || len(rev) > 12 {
		t.Fatalf("revision %q, want between one and twelve characters", rev)
	}
}
