package switcher

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

func codexRoot(t *testing.T) string {
	t.Helper()
	return mustPath(ccpath.StoreHome())
}

func servingIs(t *testing.T, uuid string) {
	t.Helper()
	if err := codexswitch.Execute(codexRoot(t), uuid); err != nil {
		t.Fatalf("pointing codex at %s: %v", uuid, err)
	}
}

func evalCodex(t *testing.T, book *codexproxy.LimitBook) Evaluation {
	t.Helper()
	ev, err := EvaluateCodex(openStore(t), codexRoot(t),
		EvalOptions{Now: time.Now(), Provider: provider.Codex}, book)
	if err != nil {
		t.Fatalf("EvaluateCodex: %v", err)
	}
	return ev
}

// The baseline is the POINTER and never Claude Code's live login. If the Claude
// uuid leaked in as the baseline it would never be found among the Codex
// candidates, cooldownGate's no-baseline arm would fire on every pass, and the
// lane would repoint on every tick -- the cooldown would be a no-op.
func TestTheCodexBaselineIsThePointerAndNotTheClaudeLogin(t *testing.T) {
	isolate(t)
	seed(t, "cl-1", "claude@example.com")
	liveAs(t, "cl-1")
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 50)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)

	ev := evalCodex(t, nil)
	if !ev.LiveKnown || ev.Live.UUID != "cx-1" {
		t.Fatalf("baseline = (%q, %v), want cx-1", ev.Live.UUID, ev.LiveKnown)
	}
	for _, r := range ev.Plan.Result.Order {
		if r.UUID == "cl-1" {
			t.Fatalf("the Claude account is in the codex ranking:\n%v", ev.Plan.Result.Order)
		}
	}
	if ev.Plan.Action != strategy.ActionSwitch || ev.Target.UUID != "cx-2" {
		t.Fatalf("plan = %v/%v target %q, want a switch to cx-2",
			ev.Plan.Action, ev.Plan.Reason, ev.Target.UUID)
	}
}

// An account another machine drives is not this one's to repoint at: the
// reading it would be judged on is spent from a budget somebody else is driving.
func TestAnElsewhereCodexAccountIsNotACandidate(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	other := seedCodex(t, "cx-2", "two@example.com")
	setElsewhere(t, other.UUID)
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)

	ev := evalCodex(t, nil)
	for _, r := range ev.Plan.Result.Order {
		if r.UUID == "cx-2" {
			t.Fatalf("an account another machine drives is in the ranking:\n%v", ev.Plan.Result.Order)
		}
	}
}

// A dead grant cannot serve a request, so pointing at it would turn every new
// thread into a branded 401 until somebody re-logged in.
func TestAnAccountNeedingALoginIsNotACandidate(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	markNeedsRelogin(t, "cx-2")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)

	ev := evalCodex(t, nil)
	for _, r := range ev.Plan.Result.Order {
		if r.UUID == "cx-2" {
			t.Fatalf("an account whose grant is dead is in the ranking:\n%v", ev.Plan.Result.Order)
		}
	}
}

// The pointer is what the cooldown protects, so a hand switch has to be able to
// hold the lane off for the same five minutes an automatic one does.
func TestTheCodexCooldownHoldsAHandSwitch(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1") // Execute stamps the codex cooldown

	ev := evalCodex(t, nil)
	if ev.Plan.Action != strategy.ActionStay || ev.Plan.Reason != strategy.ReasonCooldown {
		t.Fatalf("plan = %v/%v, want a stay on the cooldown", ev.Plan.Action, ev.Plan.Reason)
	}
}

// The Claude cooldown must not hold the Codex lane. Both stamps live in one
// file, so this is the failure a single shared pair would produce.
func TestTheClaudeCooldownDoesNotHoldTheCodexLane(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)
	if err := RecordSwitch("cl-1"); err != nil {
		t.Fatalf("RecordSwitch: %v", err)
	}

	ev := evalCodex(t, nil)
	if ev.Plan.Action != strategy.ActionSwitch || ev.Target.UUID != "cx-2" {
		t.Fatalf("plan = %v/%v target %q, want a switch to cx-2",
			ev.Plan.Action, ev.Plan.Reason, ev.Target.UUID)
	}
}

// A repoint onto an account the proxy already knows is throttled produces a 429
// on the first new thread. The book is how the two halves agree about that.
func TestAThrottledAccountIsNotARepointTarget(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)

	var book codexproxy.LimitBook
	book.MarkLimited("cx-2", time.Now().Add(time.Hour))

	ev := evalCodex(t, &book)
	for _, r := range ev.Plan.Result.Order {
		if r.UUID == "cx-2" {
			t.Fatalf("a throttled account is a repoint target:\n%v", ev.Plan.Result.Order)
		}
	}
}

// And the exemption: the account already serving stays in the pass even while
// throttled, or the pointer loses its baseline and the cooldown stops applying
// at exactly the moment a 429 makes churn most expensive.
func TestAThrottledServingAccountKeepsItsBaseline(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")
	seedCodex(t, "cx-2", "two@example.com")
	seedReading(t, "cx-1", 20)
	seedReading(t, "cx-2", 90)
	servingIs(t, "cx-1")
	clearCodexCooldown(t)

	var book codexproxy.LimitBook
	book.MarkLimited("cx-1", time.Now().Add(time.Hour))

	ev := evalCodex(t, &book)
	if _, ok := findRanked(ev.Plan.Result, "cx-1"); !ok {
		t.Fatalf("the serving account lost its place in the pass:\n%v", ev.Plan.Result.Order)
	}
}

// A machine with no readings at all: the answer is "nothing to choose on"
// rather than a reshuffle, which is the rule the Claude pass already keeps.
func TestEvaluateCodexReportsNoReadings(t *testing.T) {
	isolate(t)
	seedCodex(t, "cx-1", "one@example.com")

	ev := evalCodex(t, nil)
	if !ev.NoReadings {
		t.Fatalf("NoReadings = false on a machine that has never polled codex")
	}
}

// A store with no Codex accounts is not an error and not a decision.
func TestEvaluateCodexOnAMachineWithNoCodexAccounts(t *testing.T) {
	isolate(t)
	seed(t, "cl-1", "claude@example.com")

	ev := evalCodex(t, nil)
	if ev.Decided {
		t.Fatalf("Decided = true with no codex accounts to decide about")
	}
	// The asymmetry with the no-readings return is load-bearing and it is the
	// only signal there is: having no codex accounts and having some that
	// nobody has polled yet are different situations, they earn different
	// advice, and NoReadings staying false here is what tells them apart.
	if ev.NoReadings {
		t.Fatalf("NoReadings = true with no codex accounts; that is the flag that means 'accounts exist but nothing was polled'")
	}
}

func findRanked(res strategy.Result, uuid string) (strategy.Ranked, bool) {
	for _, r := range res.Order {
		if r.UUID == uuid {
			return r, true
		}
	}
	return strategy.Ranked{}, false
}

// clearCodexCooldown removes the stamp codexswitch.Execute leaves, for the
// tests that are about the ranking rather than about the hold.
func clearCodexCooldown(t *testing.T) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordCodexSwitch("", time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func setElsewhere(t *testing.T, uuid string) {
	t.Helper()
	if err := store.WithStore(func(s *store.Store) error {
		_, err := s.SetOwned(ownedExcept(s, uuid))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// ownedExcept is every account except one, which is what SetOwned takes: it is
// given the accounts THIS machine drives, and marks the rest Elsewhere.
func ownedExcept(s *store.Store, uuid string) []string {
	var out []string
	for _, a := range s.Accounts() {
		if a.UUID != uuid {
			out = append(out, a.UUID)
		}
	}
	return out
}

func markNeedsRelogin(t *testing.T, uuid string) {
	t.Helper()
	if err := store.WithStore(func(s *store.Store) error {
		creds, err := s.Credentials(uuid)
		if err != nil {
			return err
		}
		c, _, err := codexauth.FromBlob(creds)
		if err != nil {
			return err
		}
		h := codexauth.RefreshTokenHash(c.RefreshToken)
		return s.SetCodexReloginFor(uuid, h, h)
	}); err != nil {
		t.Fatal(err)
	}
}
