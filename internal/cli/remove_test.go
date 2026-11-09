package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A destructive command with no terminal to confirm at must be told explicitly,
// or a CI script silently deletes credentials nobody meant to lose.
func TestRemoveWithoutTTYRequiresYes(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")

	code, _, _, top := runRoot(t, "remove", "1")
	if code != ExitUsage {
		t.Fatalf("remove without --yes = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "--yes") {
		t.Fatalf("error %q does not tell the caller how to proceed", top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("u-1"); !ok {
		t.Fatal("remove deleted the account despite refusing")
	}
}

func TestRemoveWithYesDeletesAccountAndCredentials(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")
	credFile := filepath.Join(os.Getenv("CCDAD_HOME"), "credentials", "u-1.json")
	if _, err := os.Stat(credFile); err != nil {
		t.Fatal(err)
	}

	if code, _, _, top := runRoot(t, "remove", "1", "--yes"); code != ExitOK {
		t.Fatalf("remove --yes = %d (%s), want 0", code, top)
	}
	if _, err := os.Stat(credFile); !os.IsNotExist(err) {
		t.Fatal("remove --yes left the stored credentials behind")
	}
	s, _ := store.Open()
	if _, ok := s.Get("u-1"); ok {
		t.Fatal("remove --yes left the account in the store")
	}
}

// Answering anything but yes leaves the account alone, at exit 3.
func TestRemoveAnsweredNoKeepsTheAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	seedAccount(t, "u-1", "a@example.com")

	cmd := newRemoveCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	err, _, errOut := runCmd(t, cmd, "1")
	if CodeFor(err) != ExitNothingToDo {
		t.Fatalf("CodeFor = %d, want %d", CodeFor(err), ExitNothingToDo)
	}
	if !strings.Contains(errOut, "Left alone") {
		t.Fatalf("stderr = %q, want the no-op notice", errOut)
	}
	s, _ := store.Open()
	if _, ok := s.Get("u-1"); !ok {
		t.Fatal("answering 'n' still removed the account")
	}
}

func TestRemoveAnsweredYesRemovesTheAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	seedAccount(t, "u-1", "a@example.com")

	cmd := newRemoveCmd()
	cmd.SetIn(strings.NewReader("y\n"))
	if err, _, _ := runCmd(t, cmd, "1"); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	s, _ := store.Open()
	if _, ok := s.Get("u-1"); ok {
		t.Fatal("answering 'y' did not remove the account")
	}
}

// Removing the account that is currently live leaves the user logged in as an
// account ccdad can no longer switch back to. That state must not be silent.
func TestRemoveWarnsWhenTheAccountWasLive(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	code, _, errOut, top := runRoot(t, "remove", "1", "--yes")
	if code != ExitOK {
		t.Fatalf("remove = %d (%s), want 0", code, top)
	}
	if !strings.Contains(errOut, "still the live") {
		t.Fatalf("stderr = %q, want a warning that the login is still installed", errOut)
	}
}

// `ccdad run --full-profile` writes an API-key account's primaryApiKey into
// <store>/profiles/<uuid>/.claude.json, so an orphaned profile is an orphaned
// CREDENTIAL and not stale configuration. Nothing else cleans one: uninstall
// removes the whole store, and doctor scans sessions only — so `remove`, whose
// own help promises to "delete its stored credentials", has to.
func TestRemoveDeletesTheAccountsFullProfileToo(t *testing.T) {
	isolate(t)
	seedTokenAccount(t, "u-1", "a@example.com", cclink.APIKeyKind, "sk-ant-api-XYZ")
	profile := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-1")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ccpath.GlobalConfigFile),
		[]byte(`{"primaryApiKey":"sk-ant-api-XYZ"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, top := runRoot(t, "remove", "1", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, stderr, top)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("%s survived the removal (stat err: %v); the account's API key is still on disk", profile, err)
	}
	if !strings.Contains(stderr, profile) {
		t.Errorf("the removal never says the profile went: %q", stderr)
	}
}

// The non-interactive refusal has to name what it is about to delete, or a
// script author reading it does not know a profile is in scope.
func TestRemoveNamesTheProfileInTheConfirmationItDemands(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	profile := filepath.Join(mustPath(ccpath.StoreHome()), ProfilesDirName, "u-1")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, top := runRoot(t, "remove", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d (%s / %s), want %d", code, stderr, top, ExitUsage)
	}
	if got := top + stderr; !strings.Contains(got, profile) {
		t.Errorf("the confirmation does not name the profile it would delete: %q", got)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("a refused removal deleted the profile anyway: %v", err)
	}
}

// An account that has never been run with --full-profile has no profile, and
// the removal must not invent a sentence about one.
func TestRemoveSaysNothingAboutAProfileThatDoesNotExist(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, _, stderr, top := runRoot(t, "remove", "1", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, stderr, top)
	}
	if strings.Contains(stderr, "profile") {
		t.Errorf("the removal mentions a profile that was never created: %q", stderr)
	}
}

// One email can be a Claude login and a Codex login at once, and the display
// index restarts per provider, so neither the email nor the bare number tells a
// reader which of the two a removal is about to delete. Every sentence the
// command prints about its target -- the refusal, the prompt, the result -- has
// to carry the provider and the reference that names exactly one.
func TestRemoveNamesTheProviderOfAnEmailBothProvidersShare(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "claude-0001", "a@example.com")
	seedCodexAccount(t, "codex-0001", "a@example.com")

	code, _, stderr, top := runRoot(t, "remove", "x1")
	if code != ExitUsage {
		t.Fatalf("remove x1 without --yes = %d (%s / %s), want %d", code, stderr, top, ExitUsage)
	}
	if want := "a@example.com (codex, index x1)"; !strings.Contains(top, want) {
		t.Errorf("the refusal %q does not name %q", top, want)
	}

	stubEnvironment(t, true, false)
	cmd := newRemoveCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	if _, _, errOut := runCmd(t, cmd, "x1"); !strings.Contains(errOut, "Remove a@example.com (codex, index x1) ") {
		t.Errorf("the prompt %q does not say which provider's account it would delete", errOut)
	}

	code, _, stderr, top = runRoot(t, "remove", "x1", "--yes")
	if code != ExitOK {
		t.Fatalf("remove x1 --yes = %d (%s / %s), want 0", code, stderr, top)
	}
	if want := "Removed a@example.com (codex, index x1)."; !strings.Contains(stderr, want) {
		t.Errorf("the result %q does not say %q", stderr, want)
	}
	s, _ := store.Open()
	if _, ok := s.Get("codex-0001"); ok {
		t.Fatal("remove x1 --yes left the codex account in the store")
	}
	if _, ok := s.Get("claude-0001"); !ok {
		t.Fatal("remove x1 --yes deleted the claude account that shares its email")
	}
}

// What the engine remembers about an account is keyed by its uuid, and so is the
// account a later `ccdad add` brings back under the same login. Removing one
// therefore drops its quarantine and its departure mark, or the account returns
// quarantined, or held out until a window of its previous life rolls over. The
// other account's entries stay: this is a prune of the removed, not a reset.
func TestRemoveForgetsWhatTheEngineRememberedAboutTheAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	at := time.Now()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		for _, uuid := range []string{"u-1", "u-2"} {
			st.Quarantine(uuid, at, time.Hour, "dead refresh token")
			st.RecordSwitch("", at, strategy.Departure{From: uuid, Closing: usage.WindowFiveHour})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if code, _, _, top := runRoot(t, "remove", "1", "--yes"); code != ExitOK {
		t.Fatalf("remove = %d (%s), want 0", code, top)
	}

	marks := departuresOnDisk(t)
	quarantined := map[string]bool{}
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		for _, uuid := range []string{"u-1", "u-2"} {
			_, quarantined[uuid] = st.Quarantined(uuid, at)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, held := marks["u-1"]; held || quarantined["u-1"] {
		t.Errorf("the removed account is still remembered: mark %v, quarantined %v", marks["u-1"], quarantined["u-1"])
	}
	if _, held := marks["u-2"]; !held || !quarantined["u-2"] {
		t.Errorf("the account left in the store lost its state: mark %v, quarantined %v", held, quarantined["u-2"])
	}
}

// The removal forgets the account it removed and nothing else -- including an
// account this process's copy of the store never saw. That is the one another
// `ccdad add` writes between this command reading the store and taking the
// engine-state lock: it is quarantined or marked in the state, and absent from
// every set this command could have built. A prune of whatever is not in the
// store would delete its fresh state.
func TestRemoveForgetsOnlyTheAccountItRemoved(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")
	at := time.Now()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		for _, uuid := range []string{"u-1", "u-new"} {
			st.Quarantine(uuid, at, time.Hour, "dead refresh token")
			st.RecordSwitch("", at, strategy.Departure{From: uuid, Closing: usage.WindowFiveHour})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if code, _, _, top := runRoot(t, "remove", "1", "--yes"); code != ExitOK {
		t.Fatalf("remove = %d (%s), want 0", code, top)
	}

	var quarantined bool
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		_, quarantined = st.Quarantined("u-new", at)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, held := departuresOnDisk(t)["u-new"]; !held || !quarantined {
		t.Errorf("an account the removal never saw lost its state: mark %v, quarantined %v", held, quarantined)
	}
}
