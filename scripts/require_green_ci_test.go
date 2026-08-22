package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The gate that decides whether a tag may publish. It cannot be exercised by
// pushing a tag — that is the one thing it exists to make safe — and it cannot
// be exercised against the real API either, because the answer it reads is a
// property of a commit that has not been pushed yet.
//
// So `gh` is replaced on PATH. The stand-in applies the real jq to a canned
// response rather than returning pre-filtered lines: `gh api --jq` is where
// this script's only piece of parsing lives, and a stand-in that skipped it
// would leave the filter untested while the tests all passed.

// A single canned `gh api` answer. fail sends body to stderr and exits 1,
// which is what gh does for an HTTP error.
type ghAnswer struct {
	body string
	fail bool
}

// fakeGh writes a `gh` onto a fresh directory and returns it plus that
// directory. Answers are consumed in order; once they run out the last one
// repeats, so a test that polls does not have to predict how many times.
func fakeGh(t *testing.T, answers []ghAnswer) (binDir, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the gate runs on the ubuntu-latest release runner")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is needed to stand in for gh's built-in jq; the ubuntu runner has it")
	}

	stateDir = t.TempDir()
	binDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "count"), []byte("0"), 0o644); err != nil {
		t.Fatalf("seeding the call counter: %v", err)
	}
	for i, a := range answers {
		name := filepath.Join(stateDir, "answer-"+strconv.Itoa(i+1))
		if err := os.WriteFile(name, []byte(a.body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if a.fail {
			if err := os.WriteFile(name+".fail", nil, 0o644); err != nil {
				t.Fatalf("writing %s.fail: %v", name, err)
			}
		}
	}

	gh := "#!/usr/bin/env bash\nset -eu\ndir=" + strconv.Quote(stateDir) + `
n=$(cat "$dir/count")
n=$((n + 1))
printf '%s' "$n" >"$dir/count"
printf '%s\n' "$*" >>"$dir/args"

answer=$dir/answer-$n
while [ ! -f "$answer" ] && [ "$n" -gt 1 ]; do
	n=$((n - 1))
	answer=$dir/answer-$n
done

if [ -f "$answer.fail" ]; then
	cat "$answer" >&2
	exit 1
fi

filter=
prev=
for a in "$@"; do
	if [ "$prev" = --jq ]; then
		filter=$a
	fi
	prev=$a
done
if [ -n "$filter" ]; then
	jq -r "$filter" <"$answer"
else
	cat "$answer"
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatalf("writing the gh stand-in: %v", err)
	}
	return binDir, stateDir
}

const testSHA = "4787de9210ec071ec23672269e9323587bfc2de5"

// requireGreenCI runs the script with `gh` stubbed and returns its stderr, its
// exit code, and every argument list gh was called with.
func requireGreenCI(t *testing.T, answers []ghAnswer, env []string, args ...string) (string, int, []string) {
	t.Helper()
	binDir, stateDir := fakeGh(t, answers)

	script, err := filepath.Abs("require-green-ci.sh")
	if err != nil {
		t.Fatalf("resolving require-green-ci.sh: %v", err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GH_REPO=Kweiza/ccdaddy",
		// No test may sleep for the production defaults: thirty minutes of
		// waiting is the behaviour under test, not a thing to sit through.
		"CCDAD_CI_POLL=0",
		"CCDAD_CI_GRACE=0",
		"CCDAD_CI_TIMEOUT=0",
	}, env...)

	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running require-green-ci.sh: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}

	var calls []string
	raw, readErr := os.ReadFile(filepath.Join(stateDir, "args"))
	if readErr == nil {
		calls = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
	return string(out), code, calls
}

// The shape the Actions API returns. `conclusion` is null while a run is in
// flight, and that null reaches the script as the four letters "null" — which
// is exactly the value the comparison against "success" has to survive.
func workflowRuns(runs ...string) string {
	return `{"total_count":` + strconv.Itoa(len(runs)) + `,"workflow_runs":[` + strings.Join(runs, ",") + "]}"
}

func workflowRun(status, conclusion, url string) string {
	quoted := "null"
	if conclusion != "" {
		quoted = strconv.Quote(conclusion)
	}
	return `{"status":` + strconv.Quote(status) + `,"conclusion":` + quoted +
		`,"html_url":` + strconv.Quote(url) + `}`
}

func TestRequireGreenCIAcceptsASuccessfulRun(t *testing.T) {
	green := workflowRuns(workflowRun("completed", "success", "https://github.com/Kweiza/ccdaddy/actions/runs/1"))
	out, code, calls := requireGreenCI(t, []ghAnswer{{body: green}}, nil, testSHA)

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "actions/runs/1") {
		t.Errorf("stderr does not name the run that made the decision:\n%s", out)
	}
	if len(calls) != 1 {
		t.Fatalf("gh called %d times, want 1: %v", len(calls), calls)
	}
	// The endpoint is the decision. `commits/<sha>/check-runs` would include
	// the release workflow's own jobs and wait for itself.
	want := "repos/Kweiza/ccdaddy/actions/workflows/ci.yml/runs?head_sha=" + testSHA
	if !strings.Contains(calls[0], want) {
		t.Errorf("gh called with %q, want it to contain %q", calls[0], want)
	}
}

func TestRequireGreenCITakesTheWorkflowFileAsAnArgument(t *testing.T) {
	green := workflowRuns(workflowRun("completed", "success", "https://example.invalid/1"))
	_, code, calls := requireGreenCI(t, []ghAnswer{{body: green}}, nil, testSHA, "other.yml")

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(calls[0], "workflows/other.yml/runs") {
		t.Errorf("gh called with %q, want the workflow file from argv", calls[0])
	}
}

func TestRequireGreenCIWaitsForARunStillInFlight(t *testing.T) {
	out, code, calls := requireGreenCI(t, []ghAnswer{
		{body: workflowRuns(workflowRun("in_progress", "", "https://example.invalid/1"))},
		{body: workflowRuns(workflowRun("completed", "success", "https://example.invalid/1"))},
	}, []string{"CCDAD_CI_TIMEOUT=60", "CCDAD_CI_POLL=1"}, testSHA)

	if code != 0 {
		t.Fatalf("exit %d, want 0 — a run that is still going is not a run that failed\n%s", code, out)
	}
	if len(calls) < 2 {
		t.Errorf("gh called %d times, want at least 2: %v", len(calls), calls)
	}
}

func TestRequireGreenCIRefusesARunThatFinishedRed(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled", "timed_out", "action_required"} {
		t.Run(conclusion, func(t *testing.T) {
			red := workflowRuns(workflowRun("completed", conclusion, "https://example.invalid/1"))
			// `cancelled` is the one ci.yml's concurrency block is written
			// around: a run superseded by a later push ends cancelled, and a
			// cancelled run is not a commit anyone proved green.
			//
			// A timeout far larger than this test is willing to wait, on
			// purpose: a finished run cannot become green by waiting, so the
			// script has to refuse without spending it.
			out, code, calls := requireGreenCI(t, []ghAnswer{{body: red}},
				[]string{"CCDAD_CI_TIMEOUT=30", "CCDAD_CI_GRACE=30", "CCDAD_CI_POLL=5"}, testSHA)

			if code != 1 {
				t.Fatalf("exit %d, want 1\n%s", code, out)
			}
			if !strings.Contains(out, conclusion) {
				t.Errorf("stderr does not say which conclusion refused the tag:\n%s", out)
			}
			if len(calls) != 1 {
				t.Errorf("gh called %d times, want 1 — a completed run needs no polling", len(calls))
			}
		})
	}
}

// A commit whose first run went red and was then re-run green is green. The
// scan therefore may not stop at the first non-success it sees, which is the
// easy way to write this loop and the wrong one.
func TestRequireGreenCIAcceptsARerunAfterAFailure(t *testing.T) {
	mixed := workflowRuns(
		workflowRun("completed", "success", "https://example.invalid/2"),
		workflowRun("completed", "failure", "https://example.invalid/1"),
	)
	out, code, _ := requireGreenCI(t, []ghAnswer{{body: mixed}}, nil, testSHA)
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a red first attempt that was re-run green is green\n%s", code, out)
	}
}

func TestRequireGreenCIRefusesACommitCIHasNeverSeen(t *testing.T) {
	out, code, _ := requireGreenCI(t, []ghAnswer{{body: workflowRuns()}}, nil, testSHA)

	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	// Distinguishing "never ran" from "still running" is the entire reason
	// there is a grace period as well as a timeout: the first can never
	// resolve, and reporting it as a timeout sends the reader to wait.
	if !strings.Contains(out, "has never run") {
		t.Errorf("stderr does not say the run is absent rather than slow:\n%s", out)
	}
	if !strings.Contains(out, "workflow_dispatch") {
		t.Errorf("stderr does not name the way out:\n%s", out)
	}
}

func TestRequireGreenCIWaitsOutTheGraceForARunToAppear(t *testing.T) {
	out, code, calls := requireGreenCI(t, []ghAnswer{
		{body: workflowRuns()},
		{body: workflowRuns(workflowRun("completed", "success", "https://example.invalid/1"))},
	}, []string{"CCDAD_CI_GRACE=60", "CCDAD_CI_POLL=1"}, testSHA)

	if code != 0 {
		t.Fatalf("exit %d, want 0 — a tag pushed seconds after its branch beats the run into existence\n%s", code, out)
	}
	if len(calls) < 2 {
		t.Errorf("gh called %d times, want at least 2", len(calls))
	}
}

func TestRequireGreenCIGivesUpOnARunThatNeverFinishes(t *testing.T) {
	stuck := workflowRuns(workflowRun("in_progress", "", "https://example.invalid/1"))
	out, code, _ := requireGreenCI(t, []ghAnswer{{body: stuck}},
		[]string{"CCDAD_CI_TIMEOUT=1", "CCDAD_CI_POLL=1"}, testSHA)

	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("stderr does not say what it gave up waiting for:\n%s", out)
	}
}

func TestRequireGreenCIRefusesAWorkflowThatDoesNotExist(t *testing.T) {
	out, code, calls := requireGreenCI(t, []ghAnswer{
		{body: "gh: Not Found (HTTP 404)", fail: true},
	}, []string{"CCDAD_CI_TIMEOUT=30", "CCDAD_CI_POLL=5"}, testSHA, "typo.yml")

	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "no workflow typo.yml") {
		t.Errorf("stderr does not name the workflow it could not find:\n%s", out)
	}
	if len(calls) != 1 {
		t.Errorf("gh called %d times, want 1 — a 404 is not transient", len(calls))
	}
}

func TestRequireGreenCIRetriesAnUnreachableAPI(t *testing.T) {
	out, code, calls := requireGreenCI(t, []ghAnswer{
		{body: "gh: Bad Gateway (HTTP 502)", fail: true},
		{body: workflowRuns(workflowRun("completed", "success", "https://example.invalid/1"))},
	}, []string{"CCDAD_CI_TIMEOUT=60", "CCDAD_CI_POLL=1"}, testSHA)

	if code != 0 {
		t.Fatalf("exit %d, want 0 — a release must not be lost to one 502\n%s", code, out)
	}
	if len(calls) < 2 {
		t.Errorf("gh called %d times, want at least 2", len(calls))
	}
}

func TestRequireGreenCIRefusesWithoutACommitOrARepository(t *testing.T) {
	green := []ghAnswer{{body: workflowRuns(workflowRun("completed", "success", "https://example.invalid/1"))}}

	out, code, calls := requireGreenCI(t, green, nil)
	if code != 1 {
		t.Errorf("exit %d with no commit, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "no commit given") {
		t.Errorf("stderr does not tell a caller with no argument what is missing:\n%s", out)
	}
	if len(calls) != 0 {
		t.Errorf("gh called %d times with no commit, want 0", len(calls))
	}

	out, code, _ = requireGreenCI(t, green, []string{"GH_REPO=", "GITHUB_REPOSITORY="}, testSHA)
	if code != 1 {
		t.Errorf("exit %d with no repository, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "GH_REPO") {
		t.Errorf("stderr does not name the variable to set:\n%s", out)
	}
}
