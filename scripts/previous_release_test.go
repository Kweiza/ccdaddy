package scripts

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Which release the upgrade leg of .github/workflows/install-smoke.yml installs
// first. Getting it wrong is not a red workflow: it is an upgrade leg that
// quietly upgrades from the wrong thing, or one that turns itself off. `gh` is
// replaced on PATH the same way scripts/require_green_ci_test.go replaces it,
// so the real jq still runs the real filter.

// aRelease renders one element of the releases list. Only the three fields the
// filter reads are present; a draft carries a null published_at, as the API
// returns.
func aRelease(tag, publishedAt string, draft bool) string {
	published := fmt.Sprintf("%q", publishedAt)
	if draft {
		published = "null"
	}
	return fmt.Sprintf(`{"tag_name":%q,"published_at":%s,"draft":%t}`, tag, published, draft)
}

func releaseList(items ...string) string {
	return "[" + strings.Join(items, ",") + "]"
}

// previousRelease runs the script with `gh` stubbed. stdout is kept apart from
// stderr because the KEY=VALUE line is the product and everything else is
// commentary — the caller redirects only the first into $GITHUB_OUTPUT.
func previousRelease(t *testing.T, answers []ghAnswer, env []string, args ...string) (stdout, stderr string, code int, calls []string) {
	t.Helper()
	binDir, stateDir := fakeGh(t, answers)

	script, err := filepath.Abs("previous-release.sh")
	if err != nil {
		t.Fatalf("resolving previous-release.sh: %v", err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GH_REPO=Kweiza/ccdaddy",
	}, env...)

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running previous-release.sh: %v\n%s", err, errOut.String())
		}
		code = exit.ExitCode()
	}

	if raw, readErr := os.ReadFile(filepath.Join(stateDir, "args")); readErr == nil {
		calls = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
	return out.String(), errOut.String(), code, calls
}

func TestPreviousRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		tag  string
		want string
	}{
		{
			name: "the release published before this one",
			body: releaseList(
				aRelease("v1.2.3", "2026-08-22T01:52:31Z", false),
				aRelease("v1.2.2", "2026-08-20T09:00:00Z", false),
				aRelease("v1.2.1", "2026-08-01T09:00:00Z", false),
			),
			tag:  "v1.2.3",
			want: "previous=v1.2.2\n",
		},
		{
			// A draft has no public download URL. Pinning CCDAD_VERSION at one
			// makes the installer abort on a missing sums file, which reads as
			// a broken installer rather than as an unpublished tag.
			name: "a draft is not something anyone can install",
			body: releaseList(
				aRelease("v1.2.3", "2026-08-22T01:52:31Z", false),
				aRelease("v1.3.0", "", true),
				aRelease("v1.2.2", "2026-08-20T09:00:00Z", false),
			),
			tag:  "v1.2.3",
			want: "previous=v1.2.2\n",
		},
		{
			// The order is imposed by the filter, so a list that arrives in
			// some other order still answers the question that was asked.
			name: "publication order decides, not list order",
			body: releaseList(
				aRelease("v1.2.1", "2026-08-01T09:00:00Z", false),
				aRelease("v1.2.3", "2026-08-22T01:52:31Z", false),
				aRelease("v1.2.2", "2026-08-20T09:00:00Z", false),
			),
			tag:  "v1.2.3",
			want: "previous=v1.2.2\n",
		},
		{
			// Today's shape: one prerelease and nothing else. Upgrading from an
			// rc to the stable that follows it is a real upgrade, so a
			// prerelease is a candidate like any other.
			name: "a prerelease is a candidate",
			body: releaseList(
				aRelease("v0.1.0", "2026-08-23T01:00:00Z", false),
				aRelease("v0.1.0-rc1", "2026-08-22T01:52:31Z", false),
			),
			tag:  "v0.1.0",
			want: "previous=v0.1.0-rc1\n",
		},
		{
			// The first release. A skip, not a failure: the upgrade leg has
			// nothing to install first, and failing here would deny a smoke
			// test to the release that most needs one.
			name: "nothing came before it",
			body: releaseList(aRelease("v0.1.0-rc1", "2026-08-22T01:52:31Z", false)),
			tag:  "v0.1.0-rc1",
			want: "previous=\n",
		},
		{
			name: "no releases at all",
			body: releaseList(),
			tag:  "v0.1.0",
			want: "previous=\n",
		},
		{
			// The workflow can run before the release it is testing appears in
			// the list. Skipping the tag by name rather than by position covers
			// that without the caller saying which case it is in.
			name: "this release is not in the list yet",
			body: releaseList(
				aRelease("v1.2.2", "2026-08-20T09:00:00Z", false),
				aRelease("v1.2.1", "2026-08-01T09:00:00Z", false),
			),
			tag:  "v1.2.3",
			want: "previous=v1.2.2\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, _ := previousRelease(t, []ghAnswer{{body: tc.body}}, nil, tc.tag)
			if code != 0 {
				t.Fatalf("previous-release.sh exited %d:\n%s", code, stderr)
			}
			if stdout != tc.want {
				t.Errorf("previous-release.sh wrote %q to stdout, want %q", stdout, tc.want)
			}
		})
	}
}

// One page of the newest hundred, and deliberately no --paginate: gh applies
// --jq per page, so a filter that sorts would sort within a page and the
// aggregate would be page order again.
func TestPreviousReleaseAsksForOneUnpaginatedPage(t *testing.T) {
	body := releaseList(
		aRelease("v1.2.3", "2026-08-22T01:52:31Z", false),
		aRelease("v1.2.2", "2026-08-20T09:00:00Z", false),
	)
	_, _, _, calls := previousRelease(t, []ghAnswer{{body: body}}, nil, "v1.2.3")
	if len(calls) != 1 {
		t.Fatalf("gh was called %d times, want once: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "repos/Kweiza/ccdaddy/releases?per_page=100") {
		t.Errorf("gh was called with %q, want the releases list at per_page=100", calls[0])
	}
	if strings.Contains(calls[0], "--paginate") {
		t.Errorf("gh was called with %q; --jq runs per page, so --paginate unsorts the sort", calls[0])
	}
}

// "There is no earlier release" and "nobody could ask" are the same empty
// string to the caller, and the second one silently turns the upgrade leg off.
func TestPreviousReleaseFailsWhenTheAPIDoes(t *testing.T) {
	stdout, stderr, code, _ := previousRelease(t,
		[]ghAnswer{{body: "gh: HTTP 403 rate limit exceeded", fail: true}}, nil, "v1.2.3")
	if code == 0 {
		t.Fatalf("previous-release.sh exited 0 on an API failure:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("previous-release.sh wrote %q to stdout; an unanswered question has no output", stdout)
	}
	if !strings.Contains(stderr, "cannot list the releases") {
		t.Errorf("previous-release.sh said:\n%s\nwant it to name what it could not do", stderr)
	}
}

func TestPreviousReleaseRefusesWithoutATagOrARepository(t *testing.T) {
	t.Run("no tag", func(t *testing.T) {
		_, stderr, code, _ := previousRelease(t, []ghAnswer{{body: releaseList()}}, nil)
		if code == 0 {
			t.Fatalf("previous-release.sh exited 0 with no tag:\n%s", stderr)
		}
		if !strings.Contains(stderr, "usage:") {
			t.Errorf("previous-release.sh said:\n%s\nwant a usage line", stderr)
		}
	})

	t.Run("no repository", func(t *testing.T) {
		_, stderr, code, _ := previousRelease(t, []ghAnswer{{body: releaseList()}},
			[]string{"GH_REPO=", "GITHUB_REPOSITORY="}, "v1.2.3")
		if code == 0 {
			t.Fatalf("previous-release.sh exited 0 with no repository:\n%s", stderr)
		}
		if !strings.Contains(stderr, "no repository") {
			t.Errorf("previous-release.sh said:\n%s\nwant it to say which variable is missing", stderr)
		}
	})
}
