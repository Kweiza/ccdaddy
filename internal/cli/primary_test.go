package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// The seat this command exists for: metered only in credits, unreachable by the
// engine while max_auto_spend bounds it, and one command away from being ranked
// with the accounts whose quota is already paid for.
func TestPrimaryOnRanksTheSeatAndSaysWhatItCosts(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "u-1", "seat@example.com")

	code, _, stderr, _ := runRoot(t, "primary", "seat@example.com", "on")
	if code != ExitOK {
		t.Fatalf("primary on = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "max_auto_spend") {
		t.Errorf("stderr = %q, want it to name the ceiling this removes", stderr)
	}
	if !strings.Contains(stderr, "is now primary") {
		t.Errorf("stderr = %q, want it to say the flag is on", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); !got.Primary {
		t.Error("the account is not primary after `ccdad primary seat@example.com on`")
	}
}

// The notice has to be printed BEFORE the write, and the only way to observe
// that order from outside is to make the write fail: a store lock held by
// another process is exactly that. A notice printed afterwards never reaches
// the one user who most needs it -- the one who typed this by mistake.
func TestPrimarySaysWhatItCostsBeforeItWrites(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "u-1", "seat@example.com")

	saved := store.LockTimeout
	store.LockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { store.LockTimeout = saved })

	// Another process's hold. gofrs is used directly rather than through the
	// store so this is a second open file description, which is what makes
	// flock(2) exclude it.
	held := flock.New(mustPath(store.LockPath()))
	locked, err := held.TryLock()
	if err != nil || !locked {
		t.Fatalf("TryLock() = %v, %v; want the lock", locked, err)
	}
	t.Cleanup(func() { _ = held.Unlock() })

	code, _, stderr, _ := runRoot(t, "primary", "seat@example.com", "on")

	if code != ExitFailure {
		t.Fatalf("primary behind a held store lock = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, "max_auto_spend") {
		t.Errorf("stderr = %q; the notice comes after the write, so a write that fails says nothing at all", stderr)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Primary {
		t.Error("the flag was set despite the lock being held elsewhere")
	}
}

func TestPrimaryTwiceIsNothingToDo(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "u-1", "seat@example.com")

	if code, _, stderr, _ := runRoot(t, "primary", "seat@example.com", "on"); code != ExitOK {
		t.Fatalf("first primary on = %d (%s), want 0", code, stderr)
	}
	code, _, stderr, top := runRoot(t, "primary", "seat@example.com", "on")
	if code != ExitNothingToDo {
		t.Fatalf("second primary on = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already primary") {
		t.Errorf("stderr = %q, want it to say the account is already primary", stderr)
	}
	// And no money notice on the second run: the ceiling was already off for
	// this account, so nothing is being armed and a line saying otherwise
	// describes an action that did not happen.
	if strings.Contains(stderr, "max_auto_spend") {
		t.Errorf("stderr = %q, want no money notice for an account that was already primary", stderr)
	}
	if top != "" {
		t.Errorf("ExecuteWith printed %q on top of the command's own notice", top)
	}
}

// off is the way back behind the credit gate, and it must not print the money
// notice: nothing is being armed.
func TestPrimaryOffPutsTheSeatBackBehindTheCreditGate(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "u-1", "seat@example.com")
	if code, _, stderr, _ := runRoot(t, "primary", "seat@example.com", "on"); code != ExitOK {
		t.Fatalf("primary on = %d (%s), want 0", code, stderr)
	}

	code, _, stderr, _ := runRoot(t, "primary", "seat@example.com", "off")
	if code != ExitOK {
		t.Fatalf("primary off = %d (%s), want 0", code, stderr)
	}
	if strings.Contains(stderr, "max_auto_spend") {
		t.Errorf("stderr = %q, want no money notice on the way off", stderr)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Primary {
		t.Error("the account is still primary after `ccdad primary seat@example.com off`")
	}
	code, _, stderr, _ = runRoot(t, "primary", "seat@example.com", "off")
	if code != ExitNothingToDo {
		t.Errorf("primary off on an account that is not primary = %d, want %d", code, ExitNothingToDo)
	}
	// The account is not primary here, which is the state that separates "only
	// on the way ON" from "only when the flag is not already set". Turning
	// something off never removes a ceiling, so this run must be silent about
	// money too.
	if strings.Contains(stderr, "max_auto_spend") {
		t.Errorf("stderr = %q, want no money notice on the way off", stderr)
	}
}

// A verb this command cannot read is exit 2 rather than a guess: the two
// guesses are "spend nothing" and "spend automatically".
func TestPrimaryRefusesWhatItCannotRead(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "u-1", "seat@example.com")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"a verb that is neither", []string{"primary", "seat@example.com", "yes"}},
		{"an empty verb", []string{"primary", "seat@example.com", ""}},
		{"no verb at all", []string{"primary", "seat@example.com"}},
		{"an unknown account", []string{"primary", "nobody", "on"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _, _ := runRoot(t, tc.args...); code != ExitUsage {
				t.Errorf("`ccdad %s` = %d, want %d", strings.Join(tc.args, " "), code, ExitUsage)
			}
		})
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Primary {
		t.Error("a refusal set the flag anyway")
	}
}

// An account that is not metered in credits has nothing for this flag to
// select: the engine reads it only for a credit account. Refusing would be
// wrong -- store.ApplyUsage revises the classification from every successful
// poll, so the flag can be set before the first reading arrives -- but
// accepting it in silence leaves the user with a setting that does nothing and
// no way to know.
func TestPrimaryNamesAKindTheFlagDoesNotReach(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "plan@example.com") // classifies as a subscription

	code, _, stderr, _ := runRoot(t, "primary", "plan@example.com", "on")
	if code != ExitOK {
		t.Fatalf("primary on = %d (%s), want 0", code, stderr)
	}
	if !strings.Contains(stderr, "subscription") {
		t.Errorf("stderr = %q, want it to name the classification the flag is not read for", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); !got.Primary {
		t.Error("the flag was refused rather than stored for an account that is not credit-metered yet")
	}
}
