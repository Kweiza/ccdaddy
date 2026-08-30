package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// probeNow is the clock every test in this file runs against.
var probeNow = mustTime("2026-08-24T12:00:00Z")

// unusedWindow is what the endpoint reports for a window nothing has ever been
// spent against: a utilization it can name and no reset time at all. It is the
// exact condition a probe exists to remove, and `window` cannot express it,
// because that helper takes a reset.
func unusedWindow(pct float64) usage.Window {
	return usage.NewWindow(&pct, nil)
}

// seedUnprobed stores an account holding a reading whose five-hour window has
// never been used. The reading is older than the cache's serve TTL so that the
// entry is one a poller would act on rather than one it would serve as it
// stands.
func seedUnprobed(t *testing.T, uuid, email string) {
	t.Helper()
	seedAccount(t, uuid, email)
	seedUsageEntry(t, uuid, usage.Entry{
		FetchedAt: probeNow.Add(-10 * time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: unusedWindow(0)},
	})
}

// The whole command in one assertion: one turn of Claude Code, run as the named
// account, out of a credential home ccdad made for this probe alone.
//
// The argument list is pinned because every token in it is the user's quota.
// `--max-turns 1` is what stops a model that decides to use a tool from turning
// a probe into a session, and the prompt is one word for the same reason.
func TestProbeRunsOneTurnAsTheNamedAccountInAScopedCredentialHome(t *testing.T) {
	claude := isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	seedUnprobed(t, "u-2", "b@example.com")

	var creds string
	stub := stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		raw, err := os.ReadFile(filepath.Join(home, ccpath.CredentialsFile))
		if err != nil {
			t.Errorf("reading the probe session's credentials: %v", err)
			return
		}
		creds = string(raw)
	})

	code, _, errOut, top := runRoot(t, "probe", "b@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if want := []string{"-p", "hi", "--max-turns", "1"}; !slices.Equal(stub.spec.Args, want) {
		t.Errorf("claude args = %q, want %q", stub.spec.Args, want)
	}
	scoped, ok := envOf(stub.spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if !ok {
		t.Fatal("the probe was not given a credential home of its own, so it would spend the live login's quota")
	}
	if scoped == claude {
		t.Fatalf("the probe was pointed at the live credential home %q", scoped)
	}
	sessions := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName)
	if !strings.HasPrefix(scoped, sessions+string(filepath.Separator)) {
		t.Errorf("probe credential home = %q, want it under %q", scoped, sessions)
	}
	if !strings.Contains(creds, "RT-u-2") || strings.Contains(creds, "RT-u-1") {
		t.Errorf("the probe ran as the wrong account: %s", creds)
	}
	if !stub.spec.Silent {
		t.Error("the probe's claude was given ccdad's own stdio; a model's greeting would land on a caller's stream")
	}
	if stub.spec.Timeout <= 0 {
		t.Error("the probe's claude has no deadline, so a claude waiting for a prompt holds a live refresh token forever")
	}
}

// The promise that makes a probe safe to run unattended, asserted on the bytes:
// whatever Claude Code was logged in as before is what it is logged in as after.
func TestProbeNeverTouchesTheLiveCredentialsFile(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	seedUnprobed(t, "u-2", "b@example.com")
	live := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(live, []byte(`{"claudeAiOauth":{"refreshToken":"RT-live"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	stubClaude(t, ExitOK)

	if code, _, errOut, top := runRoot(t, "probe", "b@example.com"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	after, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("the live credentials file is gone: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the live login changed across a probe:\n before %s\n after  %s", before, after)
	}
}

// A probe session holds a live refresh token at 0600, so a probe that leaked one
// would put a credential nobody knows about under the store — and the only thing
// that would ever mention it again is doctor's sessions check. This asserts both
// halves: the directory is gone, and the check that reports leftovers is clean.
func TestProbeRemovesItsSessionAndLeavesDoctorClean(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	var home string
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ = envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		// What Claude Code does on its first refresh, reproduced: the legacy
		// OAuth lock is a directory BESIDE the credential home, so removing the
		// home alone leaves it behind.
		if err := os.MkdirAll(home+".lock", 0o700); err != nil {
			t.Error(err)
		}
	})

	if code, _, errOut, top := runRoot(t, "probe", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	for _, path := range []string{home, home + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the probe (err = %v)", path, err)
		}
	}
	_, report, _ := runDoctor(t)
	if got := report.level(t, "sessions"); got != string(levelOK) {
		t.Fatalf("doctor's sessions check is %q after a probe (%s); the probe leaked a credential home",
			got, report.detail(t, "sessions"))
	}
}

// The refusals, and the exit code each one has to carry. 2 and 3 are not
// interchangeable here: a cron wrapper has to tell "you asked for something that
// cannot be done" from "there was nothing to do", and both answers are ordinary
// for this command.
func TestProbeRefusesWhatItCannotOrNeedNotProbe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		args  []string
		want  ExitCode
		says  string
	}{{
		name: "an api-key account has no window a probe could wake",
		setup: func(t *testing.T) {
			seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
		},
		args: []string{"probe", "1"},
		want: ExitUsage,
		says: "an API key",
	}, {
		name: "a setup-token account has no refresh grant to read one with",
		setup: func(t *testing.T) {
			seedTokenAccount(t, "u-1", "a@example.com", "setup-token", "sk-ant-oat-XYZ")
		},
		args: []string{"probe", "1"},
		want: ExitUsage,
		says: "a setup token",
	}, {
		name: "the window already reports a reset time",
		setup: func(t *testing.T) {
			seedAccount(t, "u-1", "a@example.com")
			seedUsageEntry(t, "u-1", usage.Entry{
				FetchedAt: probeNow.Add(-10 * time.Minute),
				Snapshot:  &usage.Snapshot{FiveHour: window(20, probeNow.Add(time.Hour))},
			})
		},
		args: []string{"probe", "1"},
		want: ExitNothingToDo,
		says: "clock is already running",
	}, {
		name:  "an account reference that does not resolve",
		setup: func(t *testing.T) { seedUnprobed(t, "u-1", "a@example.com") },
		args:  []string{"probe", "nobody@example.com"},
		want:  ExitUsage,
		says:  "no such account",
	}, {
		name:  "an exact uuid nothing holds",
		setup: func(t *testing.T) { seedUnprobed(t, "u-1", "a@example.com") },
		args:  []string{"probe", "--uuid", "u-9"},
		want:  ExitUsage,
		says:  "no account has the uuid",
	}, {
		name:  "an account and --all at once",
		setup: func(t *testing.T) { seedUnprobed(t, "u-1", "a@example.com") },
		args:  []string{"probe", "1", "--all"},
		want:  ExitUsage,
		says:  "not more than one of them",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			freezeClock(t, probeNow)
			tc.setup(t)
			stub := stubClaude(t, ExitOK)

			code, _, errOut, top := runRoot(t, tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, tc.want)
			}
			if got := errOut + top; !strings.Contains(got, tc.says) {
				t.Errorf("the refusal does not say %q:\n%s", tc.says, got)
			}
			if stub.started {
				t.Error("spent the user's quota for a probe that was refused")
			}
		})
	}
}

// A machine with no Claude Code cannot probe however often it is asked, so the
// caller has to change something rather than retry — which is exactly what 2
// means in this binary and 1 does not.
func TestProbeWithoutClaudeOnPATHIsAUsageError(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	saved := lookClaude
	t.Cleanup(func() { lookClaude = saved })
	lookClaude = func(string) (string, error) {
		return "", errors.New(`exec: "claude": executable file not found in $PATH`)
	}

	code, _, errOut, top := runRoot(t, "probe", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if got := errOut + top; !strings.Contains(got, "claude") {
		t.Errorf("the refusal does not name what is missing:\n%s", got)
	}
}

// The default mode's scoping does nothing on a Claude Code that predates
// CLAUDE_SECURESTORAGE_CONFIG_DIR, and for a probe that is worse than for a
// session: the child would run as the machine's LIVE login and spend the wrong
// account's quota while ccdad recorded a probe of this one.
func TestProbeRefusesOnAClaudeCodeThatCannotScopeRatherThanSpendingTheWrongQuota(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, _, errOut, top := runRoot(t, "probe", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Fatal("spent quota through a session that would have run as the machine's live login")
	}
	if got := errOut + top; !strings.Contains(got, "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		t.Errorf("the refusal does not say why this build cannot be probed on:\n%s", got)
	}
}

// A shell that already exports ANTHROPIC_AUTH_TOKEN outranks the scoped
// credentials file a probe seeds, the same way it outranks `ccdad run`'s. Left
// unchecked, the turn is spent against whatever account that token names while
// ccdad stamps the NAMED account as probed and starts it on a six-hour
// cooldown for a reading it never took — and --force must not be a way past
// it, because forcing does not change which account the turn is spent on.
func TestProbeRefusesWhenTheShellsAuthTokenOutranksTheNamedAccount(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-someone-elses")

	code, _, errOut, top := runRoot(t, "probe", "1", "--force")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d — the turn would have been spent as that token", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Error("claude was started — a probe of the wrong account is not a probe")
	}
	message := errOut + top
	if !strings.Contains(message, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("the refusal does not name the source that wins:\n%s", message)
	}
	if entry, ok := cachedEntry(t, "u-1"); ok && !entry.Probe.LastAttemptAt.IsZero() {
		t.Error("the named account was stamped as probed for a turn spent on someone else's quota")
	}
}

// A probe spends the account's own quota, so a probe that fails must not be
// retried at whatever cadence its caller runs at. The gate counts ATTEMPTS
// rather than failures, because a probe that "worked" and left the window
// without a reset is the case that would otherwise spin — and the exit code
// cannot tell the two apart, which is why nothing schedules on it.
//
// What holds this one is the ladder's FLOOR rather than a verdict: `ccdad probe`
// takes no reading, so nothing has judged the attempt yet, and the floor is
// exactly the span in which nothing can have.
func TestAFailedProbeIsHeldByTheLaddersFloor(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitFailure)

	// The first attempt runs, and fails.
	if code, _, _, _ := runRoot(t, "probe", "1"); code != ExitFailure {
		t.Fatalf("exit = %d, want %d for a claude that exited non-zero", code, ExitFailure)
	}
	if !stub.started {
		t.Fatal("the first probe never ran, so the gate below proves nothing")
	}
	entry, ok := cachedEntry(t, "u-1")
	if !ok || entry.Probe.LastAttemptAt.IsZero() {
		t.Fatalf("the failed attempt was not recorded (%+v); it would be retried at once", entry.Probe)
	}
	if entry.Probe.LastError == "" {
		t.Error("the failure left no record of itself, and a detached probe reports to nobody else")
	}
	if want := probeNow.Add(usage.ProbePollDelay); !entry.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %s, want %s — a poll now would spend the usage budget on top of the "+
			"inference budget the probe just spent", entry.NextPollAt, want)
	}

	// Inside the ladder's floor — the span in which no reading can yet have said
	// whether the turn worked — it is still held.
	freezeClock(t, probeNow.Add(usage.ProbeSettleGap-time.Minute))
	second := stubClaude(t, ExitOK)
	code, _, errOut, top := runRoot(t, "probe", "1")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitNothingToDo)
	}
	if second.started {
		t.Fatal("a second probe was spent inside the ladder's floor")
	}
	if !strings.Contains(errOut+top, "--force") {
		t.Errorf("the refusal does not name the way through it:\n%s%s", errOut, top)
	}

	// --force is the human saying they have fixed whatever was wrong. It stamps
	// a fresh attempt, so the gate below is measured from HERE.
	forced := stubClaude(t, ExitOK)
	if code, _, errOut, top := runRoot(t, "probe", "1", "--force"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !forced.started {
		t.Fatal("--force did not spend the quota it exists to spend")
	}

	// Six hours after that forced attempt: open again on its own.
	freezeClock(t, probeNow.Add(7*time.Hour))
	later := stubClaude(t, ExitOK)
	if code, _, errOut, top := runRoot(t, "probe", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !later.started {
		t.Fatal("the gate never lifted; a probe that failed once would never be tried again")
	}
}

// --force spends quota through the two gates that mean "ccdad thinks this is
// unnecessary". It must NOT open the one that means "this cannot work", because
// there is no reading behind an account with no OAuth login for anything to take.
func TestForceDoesNotMakeAnUnpollableAccountProbeable(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "probe", "1", "--force")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
	}
	if stub.started {
		t.Error("--force spent quota for a reading nothing could ever take")
	}
}

// cachedEntry reads one account's row back out of the usage cache.
func cachedEntry(t *testing.T, uuid string) (usage.Entry, bool) {
	t.Helper()
	c, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	return c.Get(uuid)
}

// --model is how a per-model weekly window is woken, and the flag has to do two
// separate things for that to work: reach claude, and change which window ccdad
// checks for a reset before it decides the probe is unnecessary. This account's
// five-hour window already has a reset, so a probe that looked only there would
// refuse.
func TestProbeModelSelectsTheWindowItWakesAndReachesClaude(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: probeNow.Add(-10 * time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour:     window(20, probeNow.Add(time.Hour)),
			SevenDayOpus: unusedWindow(0),
		},
	})
	stub := stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "probe", "1", "--model", "opus")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — the weekly Opus window has no reset to wake", code, errOut, top)
	}
	want := []string{"-p", "hi", "--max-turns", "1", "--model", "opus"}
	if !slices.Equal(stub.spec.Args, want) {
		t.Errorf("claude args = %q, want %q", stub.spec.Args, want)
	}

	// And the control: without the flag the five-hour window is the one asked
	// about, and it already has a reset.
	plain := stubClaude(t, ExitOK)
	if code, _, _, _ := runRoot(t, "probe", "1"); code != ExitNothingToDo {
		t.Fatalf("exit = %d without --model, want %d", code, ExitNothingToDo)
	}
	if plain.started {
		t.Error("probed a window that already reports a reset time")
	}
}

// The fact belongs to the command, not to each account: --all over two accounts
// is one command that spends quota, and one line saying so. Said lazily, so a
// pass that skips everything says nothing about a charge that never happened.
func TestProbeSaysItSpendsTheUsersQuotaOnceAndOnlyWhenItDoes(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedUnprobed(t, "u-1", "a@example.com")
	seedUnprobed(t, "u-2", "b@example.com")
	stubClaude(t, ExitOK)

	code, _, errOut, top := runRoot(t, "probe", "--all")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if n := strings.Count(errOut, "spends this account's own quota"); n != 1 {
		t.Fatalf("the quota notice appeared %d times over two accounts, want exactly 1:\n%s", n, errOut)
	}

	// A second pass has nothing to do: both accounts are inside the six-hour
	// gate, so nothing is spent and nothing claims otherwise.
	code, _, errOut, top = runRoot(t, "probe", "--all")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitNothingToDo)
	}
	if strings.Contains(errOut, "spends this account's own quota") {
		t.Errorf("a pass that spent nothing told the user it had charged them:\n%s", errOut)
	}
}

// --all skips an account held out of rotation: the engine will not switch to it,
// so a reading for it buys nothing and the quota would be spent for nobody. A
// disabled account named explicitly is still probed, because that is a human
// asking.
func TestProbeAllSkipsDisabledAccountsAndAnExplicitOneStillRuns(t *testing.T) {
	isolate(t)
	freezeClock(t, probeNow)
	seedDisabledAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: probeNow.Add(-10 * time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: unusedWindow(0)},
	})
	stub := stubClaude(t, ExitOK)

	if code, _, _, _ := runRoot(t, "probe", "--all"); code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d — --all spent quota on a disabled account", code, ExitNothingToDo)
	}
	if stub.started {
		t.Fatal("--all probed an account the engine will never switch to")
	}
	named := stubClaude(t, ExitOK)
	if code, _, errOut, top := runRoot(t, "probe", "1"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if !named.started {
		t.Error("a disabled account a human named explicitly was not probed")
	}
}

// The two fields a probe adds to launchSpec, against a real process rather than
// the stub every other test in this file uses. Nothing else in the tree sets
// either one, so without this they are asserted only where they are read back.
//
// The deadline half is the one that matters most. A child killed by the context
// exits with a signal status, and exitStatus renders that as 137 — a number that
// says the machine killed it. Reporting the deadline instead is what makes an
// unattended probe's failure readable, and it only works because ctx.Err() is
// asked AFTER the wait rather than before it.
func TestRunChildSilencesAChildAndReportsItsOwnDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh is the stand-in binary here")
	}

	t.Run("silent sends the child's output to the null device", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		savedOut, savedErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = w, w
		code, cerr := runChild(launchSpec{
			Path:   "/bin/sh",
			Args:   []string{"-c", "echo noise; echo alsonoise >&2"},
			Env:    os.Environ(),
			Silent: true,
		})
		os.Stdout, os.Stderr = savedOut, savedErr
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		leaked, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if cerr != nil || code != ExitOK {
			t.Fatalf("runChild = %d, %v; want 0, nil", code, cerr)
		}
		if len(leaked) != 0 {
			t.Errorf("the child wrote %q onto ccdad's own descriptors", leaked)
		}
	})

	t.Run("a child that outlives the deadline is ccdad's answer and not a signal", func(t *testing.T) {
		code, err := runChild(launchSpec{
			Path:    "/bin/sh",
			Args:    []string{"-c", "sleep 30"},
			Env:     os.Environ(),
			Timeout: 100 * time.Millisecond,
			Silent:  true,
		})
		if err == nil {
			t.Fatalf("runChild = %d, nil; a child that never finished was reported as an exit status", code)
		}
		if code != ExitFailure {
			t.Errorf("exit = %d, want %d", code, ExitFailure)
		}
		if !strings.Contains(err.Error(), "did not finish within") {
			t.Errorf("the failure does not say the deadline is what ended it: %v", err)
		}
	})

	t.Run("no timeout starts no watchdog and changes nothing", func(t *testing.T) {
		code, err := runChild(launchSpec{
			Path:   "/bin/sh",
			Args:   []string{"-c", "exit 7"},
			Env:    os.Environ(),
			Silent: true,
		})
		if err != nil || code != 7 {
			t.Fatalf("runChild = %d, %v; want 7, nil", code, err)
		}
	})
}
