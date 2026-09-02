package daemon

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// TestASuccessfulPollRevisesAnAccountsClassification is the repair path for
// every account added before its first reading.
//
// store.ApplyUsage was written, documented and unit-tested for exactly this,
// and then never called: the only mention of it outside its own file and its
// own test was a COMMENT in internal/cli/primary.go claiming the daemon did
// this. So an account misfiled at add time stayed misfiled for the life of the
// installation, no matter how many readings said otherwise — and the note
// `ccdad primary` prints, promising that every usage reading re-checks the
// classification, was false.
//
// A reading is also the only thing that can fill in Account.Credit, so nothing
// was writing the stored balance either.
func TestASuccessfulPollRevisesAnAccountsClassification(t *testing.T) {
	isolate(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	acct := store.Account{Provider: provider.Claude, UUID: "acct-1", Email: "seat@example.com", Kind: identity.KindSubscription}
	if err := s.Add(acct, nil); err != nil {
		t.Fatal(err)
	}

	e := NewEngine()
	thr := configuredThresholds(config.Config{Threshold: 80, CreditThreshold: 80})
	e.commit(acct, creditOnlySnapshot(60.2255), tickEpoch, []string{acct.UUID}, thr, false, nil)

	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got := after.Accounts()
	if len(got) != 1 {
		t.Fatalf("store holds %d accounts, want 1", len(got))
	}
	if got[0].Kind != identity.KindCredit {
		t.Errorf("Kind = %v, want %v — the reading carries no plan window and overage enabled", got[0].Kind, identity.KindCredit)
	}
	if got[0].Credit.UsedCredits == nil {
		t.Error("Credit.UsedCredits = nil — a successful reading is the only thing that fills the stored balance")
	}
}

// TestAFailedPollLeavesAnAccountClassifiedAsItWas is the guard on the repair
// above, and it is the direction that costs money to get wrong.
//
// A credit account that has run out reports overage off and no plan windows.
// Re-classifying on that reading would file it as a subscription at the exact
// moment it went broke — so an attempt with NO reading must change nothing.
func TestAFailedPollLeavesAnAccountClassifiedAsItWas(t *testing.T) {
	isolate(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	acct := store.Account{Provider: provider.Claude, UUID: "acct-1", Email: "seat@example.com", Kind: identity.KindCredit}
	if err := s.Add(acct, nil); err != nil {
		t.Fatal(err)
	}

	e := NewEngine()
	thr := configuredThresholds(config.Config{Threshold: 80, CreditThreshold: 80})
	e.commit(acct, nil, tickEpoch, []string{acct.UUID}, thr, false, nil)

	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Accounts()[0].Kind; got != identity.KindCredit {
		t.Errorf("Kind = %v, want %v — a poll that produced no reading is not evidence", got, identity.KindCredit)
	}
}
