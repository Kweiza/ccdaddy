package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func servingNow(t *testing.T) string {
	t.Helper()
	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}
	uuid, _ := codexswitch.ReadServing(root)
	return uuid
}

// The headline: naming a Codex account moves the pointer and writes nothing
// else. Claude Code's credentials file must be exactly as it was.
func TestSwitchToACodexAccountMovesThePointerAndNotTheLogin(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1", got)
	}
	assertNoLiveCredentials(t)
	if !strings.Contains(stderr, "from the next new thread") {
		t.Fatalf("stderr does not say when the repoint takes effect:\n%s", stderr)
	}
}

// The never-cross line for this command, checked directly rather than by
// reading the source: a codex switch must never call store.SetActive,
// switcher.Execute or switcher.RecordSwitch. Each of those three leaves an
// observable mark -- the credentials file, the store's ActiveUUID hint, and
// the CLAUDE side of the anti-flap cooldown -- and this asserts every one of
// them is exactly as a fresh store leaves it, while the codex-side cooldown
// codexswitch.Execute is supposed to stamp DID move, which is what tells a
// no-op implementation (one that skipped Execute too) from this one.
func TestSwitchToACodexAccountTouchesNoneOfTheClaudeSwitchMachinery(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	assertNoLiveCredentials(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveUUID(); got != "" {
		t.Fatalf("store.ActiveUUID() = %q after a codex switch, want \"\" -- store.SetActive was called", got)
	}

	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if at, to := st.LastSwitch(); !at.IsZero() || to != "" {
		t.Fatalf("LastSwitch() = (%v, %q), want the zero value -- switcher.RecordSwitch was called", at, to)
	}
	if at, to := st.CodexLastSwitch(); at.IsZero() || to != "cx-1" {
		t.Fatalf("CodexLastSwitch() = (%v, %q), want a real stamp naming cx-1 -- codexswitch.Execute's own "+
			"cooldown must still fire even though the Claude one must not", at, to)
	}
}

// A repoint takes effect on the NEXT NEW THREAD, and the daemon is what serves
// it. With no daemon running the sentence has to say so on the same line, or a
// user starts codex and finds nothing changed.
func TestSwitchToACodexAccountSaysWhenThereIsNoDaemon(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubSingleton(t, false, nil)

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if !strings.Contains(stderr, "once the daemon runs") {
		t.Fatalf("stderr does not name the missing daemon:\n%s", stderr)
	}
}

func TestSwitchToACodexAccountIsRunningDaemonSaysNothingExtra(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubSingleton(t, true, nil)

	_, _, stderr, _ := runRoot(t, "switch", "codex@example.com")
	if strings.Contains(stderr, "once the daemon runs") {
		t.Fatalf("stderr names a missing daemon while one is running:\n%s", stderr)
	}
}

// Exit 3 is "the world is already as you asked", and a cron job that reads 0
// here would believe it had changed something.
func TestSwitchToTheCodexAccountAlreadyServingIsNothingToDo(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	if code, _, _, top := runRoot(t, "switch", "codex@example.com"); code != ExitOK {
		t.Fatalf("the first switch exited %d (%s)", code, top)
	}

	code, _, stderr, _ := runRoot(t, "switch", "codex@example.com")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitNothingToDo, stderr)
	}
}

// --provider is an ASSERTION about the account the caller named. It exists so a
// script that means to move codex cannot silently rewrite Claude Code's login
// because an alias moved between providers.
func TestSwitchRefusesAProviderAssertionThatDoesNotHold(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "claude@example.com", "--provider", "codex")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
	assertNoLiveCredentials(t)
}

func TestSwitchAcceptsAProviderAssertionThatHolds(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com", "--provider", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1", got)
	}
}

// codexswitch.Execute writes the pointer BEFORE it stamps the cooldown, so a
// failure in the stamp step alone leaves the pointer already moved. Reporting
// a plain failure in that case would be a lie -- codex now serves the account
// the user named -- so runCodexSwitch has its own honest sentence for it, and
// this is what proves that branch is reached rather than merely read.
//
// The stamp write is made to fail by putting a DIRECTORY where strategy.json
// belongs, so the state save's rename lands on an existing path and fails.
// A read-only store root (the fixture codexswitch's own tests use for this)
// does not survive reaching this command: store.Open() tightens the root back
// to 0700 on every call, and switch.go opens the store again on its way to
// the codex branch, undoing the fixture before Execute ever runs.
func TestSwitchToACodexAccountSaysSoWhenThePointerMovedButTheCooldownDidNot(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "strategy.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, top := runRoot(t, "switch", "codex@example.com")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d (ExitFailure)\n%s%s", code, ExitFailure, stderr, top)
	}
	if !strings.Contains(stderr, "Serving codex from codex@example.com from the next new thread") {
		t.Fatalf("stderr does not say the pointer moved:\n%s", stderr)
	}
	if !strings.Contains(stderr, "cooldown was not recorded") {
		t.Fatalf("stderr does not say the cooldown was not recorded:\n%s", stderr)
	}
	if got := servingNow(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1 -- the pointer must have moved even though Execute returned an error", got)
	}
}

func TestSwitchRefusesAProviderNameItCannotRead(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "claude@example.com", "--provider", "openai")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
}

// The OTHER arm of that same split, and the one nothing here reached. When the
// POINTER write itself fails, nothing moved and the user must not be told
// otherwise. Without this, `if errors.Is(...)` could be `if true`, or the
// plain `return err` could be `return nil`, and the whole suite stays green.
//
// The pointer write is made to fail by making codex/ itself unwritable, which
// is the mirror of the fixture above: store.Open() tightens the store ROOT
// back to 0700 on every call, but it never touches codex/, so this one
// survives the trip through switch.go.
func TestSwitchToACodexAccountSaysNothingMovedWhenThePointerWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a mode-based denial does not hold for root or on windows")
	}
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	root, err := codexRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	code, _, stderr, _ := runRoot(t, "switch", "codex@example.com")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want 1 on a pointer write that failed", code)
	}
	if stderr != "" {
		t.Fatalf("the command claimed something about serving when nothing moved:\n%s", stderr)
	}
	if _, ok := codexswitch.ReadServing(root); ok {
		t.Fatal("a pointer exists after the write that was supposed to fail")
	}
}

// The two flag refusals this task added are messages, and a message nobody
// asserts on is prose. Deleting both blocks left every switch test green,
// because a later refusal fires anyway -- with a sentence that does not say
// which flag the user should drop.
func TestSwitchRefusesTheFlagsThatMeanNothingForCodex(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	for _, tc := range []struct{ name, flag, value, want string }{
		{"strategy", "--strategy", "conservative", "takes no strategy"},
		{"model", "--model", "opus", "names a Claude model family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _, top := runRoot(t, "switch", "--provider", "codex", tc.flag, tc.value)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d (%s)", code, ExitUsage, top)
			}
			if !strings.Contains(top, tc.want) {
				t.Fatalf("the refusal does not say why %s means nothing here:\n%s", tc.flag, top)
			}
		})
	}
}

// The targetless codex grammar. `--provider codex` names the pool; the ranking
// picks inside it, on the same evaluation the daemon's lane runs.
func TestATargetlessSwitchWithProviderCodexPicksTheBestCodexAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")
	seedCodexAccount(t, "cx-1", "one@example.com")
	seedCodexAccount(t, "cx-2", "two@example.com")
	seedCodexUsage(t, "cx-1", 10)
	seedCodexUsage(t, "cx-2", 70)

	code, _, stderr, top := runRoot(t, "switch", "--provider", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)\n%s", code, top, stderr)
	}
	if got := servingNow(t); got != "cx-2" {
		t.Fatalf("serving = %q, want cx-2 -- the account with the most left", got)
	}
	assertNoLiveCredentials(t)
}

// With nothing polled there is no evidence to choose on, and a reshuffle is not
// a choice. Exit 4 is the code a supervisor alerts on.
func TestATargetlessCodexSwitchWithNoReadingsIsBlocked(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "one@example.com")

	code, _, stderr, _ := runRoot(t, "switch", "--provider", "codex")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d (blocked)\n%s", code, ExitBlocked, stderr)
	}
	if !strings.Contains(stderr, "no codex usage readings") {
		t.Fatalf("stderr does not say why nothing could be chosen:\n%s", stderr)
	}
}

// --strategy and --model are the Claude ranking's knobs and the codex pass
// ignores both, so they are refused rather than dropped: a flag silently
// dropped is a user believing they narrowed something.
func TestATargetlessCodexSwitchRefusesTheClaudeRankingFlags(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "one@example.com")

	for _, argv := range [][]string{
		{"switch", "--provider", "codex", "--strategy", "headroom"},
		{"switch", "--provider", "codex", "--model", "opus"},
	} {
		code, _, stderr, _ := runRoot(t, argv...)
		if code != ExitUsage {
			t.Fatalf("%v exited %d, want %d (usage)\n%s", argv, code, ExitUsage, stderr)
		}
	}
}

// A bare targetless switch is unchanged: still the Claude grammar, still
// refused without --strategy.
func TestABareTargetlessSwitchIsStillRefused(t *testing.T) {
	isolate(t)
	seedAccount(t, "cl-1", "claude@example.com")

	code, _, stderr, _ := runRoot(t, "switch")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)\n%s", code, ExitUsage, stderr)
	}
}

// seedCodexUsage caches a codex reading with `headroom` percent of its primary
// window left.
func seedCodexUsage(t *testing.T, uuid string, headroom float64) {
	t.Helper()
	pct := 100 - headroom
	resets := time.Now().Add(time.Hour)
	snap := &usage.Snapshot{CodexPrimary: usage.NewWindowWithLength(&pct, &resets, 5*time.Hour)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
