package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

var observed = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func pf(v float64) *float64 { return &v }

func subscriptionSnapshot() *usage.Snapshot {
	at := observed.Add(3 * time.Hour)
	return &usage.Snapshot{FiveHour: usage.NewWindow(pf(20), &at)}
}

// creditSnapshot is a pure credit account: no plan windows at all, with overage
// switched on.
func creditSnapshot(state usage.ExtraUsageState, reason string, limit, used *float64) *usage.Snapshot {
	return &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{State: state, DisabledReason: reason, Currency: "USD", MonthlyLimit: limit, UsedCredits: used})}
}

func seed(t *testing.T, kind identity.Kind) *Store {
	t.Helper()
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Add(Account{UUID: "acct-1", Email: "a@example.com", Kind: kind}, sampleCreds("t")); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	return s
}

func reopen(t *testing.T) *Store {
	t.Helper()
	s, err := Open()
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	return s
}

// Every production Classify call passes an empty UsageShape, so the window
// evidence Classify makes primary has never once been consulted. A pure credit
// account added by `ccdad add` is filed as a subscription and walks straight
// past the credit gate — which is precisely the failure that gate exists to
// prevent.
func TestApplyUsageReclassifiesACreditAccountOnRealEvidence(t *testing.T) {
	s := seed(t, identity.KindSubscription)

	if err := s.ApplyUsage("acct-1", creditSnapshot(usage.ExtraUsageEnabled, "", pf(15000), pf(1000)), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindCredit {
		t.Errorf("Kind = %v, want credit — the account reported no plan windows and overage on", a.Kind)
	}
}

// An account that is both — a subscription with overage enabled — classifies
// as Subscription and keeps its credit balance as a secondary axis. Windows
// win outright, because credits are not spent while a window still has room.
func TestApplyUsageKeepsWindowsPrimaryOverCredits(t *testing.T) {
	s := seed(t, identity.KindCredit)
	snap := subscriptionSnapshot()
	snap.ExtraUsage = usage.ExtraUsageFor(usage.ExtraUsageInput{State: usage.ExtraUsageEnabled, Currency: "USD", MonthlyLimit: pf(15000), UsedCredits: pf(1000)})

	if err := s.ApplyUsage("acct-1", snap, observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindSubscription {
		t.Errorf("Kind = %v, want subscription", a.Kind)
	}
	if a.Credit.State != usage.ExtraUsageEnabled.String() {
		t.Errorf("Credit.State = %q, want %q — the credit axis is kept alongside", a.Credit.State, usage.ExtraUsageEnabled)
	}
	// The store keeps MAJOR units, so 15000 cents on the wire is $150 here.
	if a.Credit.MonthlyLimit == nil || *a.Credit.MonthlyLimit != 150 {
		t.Errorf("Credit.MonthlyLimit = %v, want 150 dollars from 15000 cents", a.Credit.MonthlyLimit)
	}
	if a.Credit.UsedCredits == nil || *a.Credit.UsedCredits != 10 {
		t.Errorf("Credit.UsedCredits = %v, want 10 dollars from 1000 cents", a.Credit.UsedCredits)
	}
	if !a.Credit.ObservedAt.Equal(observed) {
		t.Errorf("Credit.ObservedAt = %s, want %s", a.Credit.ObservedAt, observed)
	}
}

// A credit account that has run out of credits reports is_enabled false with a
// reason. That is not evidence of a subscription, and Classify's no-evidence
// default — guess subscription — would walk it past the gate the moment it went
// broke, which is the worst possible moment.
func TestApplyUsageDoesNotFlipABrokeCreditAccountToSubscription(t *testing.T) {
	s := seed(t, identity.KindCredit)

	if err := s.ApplyUsage("acct-1", creditSnapshot(usage.ExtraUsageBlocked, "out_of_credits", pf(15000), pf(15000)), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindCredit {
		t.Errorf("Kind = %v, want credit — running out of credits is not evidence of a subscription", a.Kind)
	}
	if a.Credit.DisabledReason != "out_of_credits" {
		t.Errorf("Credit.DisabledReason = %q, want out_of_credits", a.Credit.DisabledReason)
	}
}

// Every subscription organization reports extra_usage with the switch OFF.
// Reading the mere presence of the object as a credit axis would file every Max
// account on the money-spending side of the gate.
func TestApplyUsageDoesNotReadOverageBeingAvailableAsACreditAxis(t *testing.T) {
	s := seed(t, identity.KindSubscription)

	if err := s.ApplyUsage("acct-1", creditSnapshot(usage.ExtraUsageDisabled, "", pf(15000), pf(0)), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindSubscription {
		t.Errorf("Kind = %v, want subscription — overage being switched off is not evidence of a credit account", a.Kind)
	}
}

// "This account reported no credit axis" and "this account reported one worth
// nothing" are different facts, and only one of them is safe to spend against.
func TestApplyUsageRecordsNoCreditStateForAnAccountThatReportedNone(t *testing.T) {
	s := seed(t, identity.KindSubscription)

	if err := s.ApplyUsage("acct-1", subscriptionSnapshot(), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Credit.State != "" {
		t.Errorf("Credit.State = %q, want empty — the reading carried no extra_usage at all", a.Credit.State)
	}
	if !a.Credit.ObservedAt.Equal(observed) {
		t.Errorf("Credit.ObservedAt = %s; the reading still happened, it just said nothing about credits", a.Credit.ObservedAt)
	}
}

// A reading that says nothing either way leaves the stored Kind alone. Classify's
// default is a FIRST-classification default: guessing subscription costs a wasted
// rotation, while guessing credit spends money. Re-applying it to an account that
// was already classified throws away better evidence than the one at hand.
func TestApplyUsageLeavesTheKindAloneWithoutEvidence(t *testing.T) {
	for _, kind := range []identity.Kind{identity.KindCredit, identity.KindSubscription} {
		t.Run(kind.String(), func(t *testing.T) {
			s := seed(t, kind)

			if err := s.ApplyUsage("acct-1", &usage.Snapshot{}, observed); err != nil {
				t.Fatalf("ApplyUsage() error = %v", err)
			}
			a, _ := reopen(t).Get("acct-1")
			if a.Kind != kind {
				t.Errorf("Kind = %v, want %v left untouched", a.Kind, kind)
			}
		})
	}
}

// A failed poll must not reach this at all. Passing no reading is a caller bug,
// and it fails loudly instead of quietly handing Classify a zero UsageShape.
func TestApplyUsageRefusesToRunWithoutAReading(t *testing.T) {
	s := seed(t, identity.KindCredit)

	err := s.ApplyUsage("acct-1", nil, observed)
	if err == nil {
		t.Fatal("ApplyUsage() accepted a nil reading; a failed poll must not be able to reclassify")
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindCredit {
		t.Errorf("Kind = %v; the refused call changed the stored kind anyway", a.Kind)
	}
	if !a.Credit.ObservedAt.IsZero() {
		t.Error("the refused call recorded a credit observation anyway")
	}
}

// An api-key account has no quota concept, so a usage reading says nothing about
// it and must not be able to reclassify it.
func TestApplyUsageLeavesAnAPIKeyAccountAlone(t *testing.T) {
	s := seed(t, identity.KindAPIKey)

	if err := s.ApplyUsage("acct-1", subscriptionSnapshot(), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}
	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindAPIKey {
		t.Errorf("Kind = %v, want api-key", a.Kind)
	}
}

func TestApplyUsageOnAnUnknownAccountIsNotFound(t *testing.T) {
	s := seed(t, identity.KindSubscription)

	if err := s.ApplyUsage("nobody", subscriptionSnapshot(), observed); !errors.Is(err, ErrNotFound) {
		t.Errorf("ApplyUsage() error = %v, want ErrNotFound", err)
	}
}

// save() rewrites KindName from Kind on every write, and Accounts() hands back a
// defensive copy — so a change made to the wrong one is a change that never
// reaches the disk.
func TestApplyUsagePersistsThroughTheStoresOwnRecord(t *testing.T) {
	seed(t, identity.KindSubscription)

	s := reopen(t)
	if err := s.ApplyUsage("acct-1", creditSnapshot(usage.ExtraUsageEnabled, "", pf(10), pf(1)), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(os.Getenv("CCDAD_HOME"), "accounts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// save() rewrites KindName from Kind, so the file is the only place that
	// proves the change went through the store's own record and not through a
	// copy Accounts() handed out.
	if !strings.Contains(stripQuotes(string(raw)), "kind = credit") {
		t.Errorf("accounts.toml did not record the new kind:\n%s", raw)
	}
}

// stripQuotes lets an assertion name a TOML value without depending on which
// quote character the encoder picked.
func stripQuotes(s string) string {
	return strings.NewReplacer("'", "", `"`, "").Replace(s)
}

// An accounts.toml written before the credit fields existed must still read, and
// must not come back claiming a credit state it never recorded.
func TestALegacyAccountsFileWithNoCreditFieldsStillReads(t *testing.T) {
	dir := withStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := "version = 1\n\n[[accounts]]\nuuid = \"acct-1\"\nemail = \"a@example.com\"\nidx = 1\nkind = \"subscription\"\nadded_at = 2026-08-21T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(dir, "accounts.toml"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	a, ok := reopen(t).Get("acct-1")
	if !ok {
		t.Fatal("the legacy account did not read back")
	}
	if a.Kind != identity.KindSubscription {
		t.Errorf("Kind = %v, want subscription", a.Kind)
	}
	if a.Credit.State != "" || a.Credit.MonthlyLimit != nil || a.Credit.UsedCredits != nil {
		t.Errorf("Credit = %+v, want an empty secondary axis for a file that never had one", a.Credit)
	}
	if got := usage.ParseExtraUsageState(a.Credit.State); got != usage.ExtraUsageUnknown {
		t.Errorf("ParseExtraUsageState(%q) = %v, want unknown", a.Credit.State, got)
	}
}

// A credit balance that could not be read must not persist as zero: the credit
// gate branches on used_credits being nil and fails closed on money.
func TestApplyUsageKeepsAnUnreadableBalanceUnread(t *testing.T) {
	s := seed(t, identity.KindSubscription)

	if err := s.ApplyUsage("acct-1", creditSnapshot(usage.ExtraUsageEnabled, "", nil, nil), observed); err != nil {
		t.Fatalf("ApplyUsage() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Credit.MonthlyLimit != nil {
		t.Errorf("Credit.MonthlyLimit = %v, want nil — a null limit means unlimited, not a cap of that value", *a.Credit.MonthlyLimit)
	}
	if a.Credit.UsedCredits != nil {
		t.Errorf("Credit.UsedCredits = %v, want nil — an unreadable spend must not persist as $0 spent", *a.Credit.UsedCredits)
	}
}

func TestCreditBalanceSurvivesTheTOMLRoundTrip(t *testing.T) {
	s := seed(t, identity.KindSubscription)
	snap := subscriptionSnapshot()
	snap.ExtraUsage = usage.ExtraUsageFor(usage.ExtraUsageInput{State: usage.ExtraUsageBlocked, DisabledReason: "org_spend_cap_reached", Currency: "USD", MonthlyLimit: pf(50000), UsedCredits: pf(49950)})
	if err := s.ApplyUsage("acct-1", snap, observed); err != nil {
		t.Fatal(err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Credit.State != "blocked" || a.Credit.DisabledReason != "org_spend_cap_reached" {
		t.Errorf("Credit = %+v", a.Credit)
	}
	if a.Credit.UsedCredits == nil || *a.Credit.UsedCredits != 499.5 {
		t.Errorf("Credit.UsedCredits = %v, want 499.50 dollars from 49950 cents", a.Credit.UsedCredits)
	}
	if a.Credit.Currency != "USD" {
		t.Errorf("Credit.Currency = %q; a stored amount with no currency is ambiguous", a.Credit.Currency)
	}
}
