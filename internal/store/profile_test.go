package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

// enterpriseProfile is the shape measured on a live claude_enterprise seat on
// 2026-08-26: the organization holds a contracted subscription while the seat
// is metered in money, so seat_tier is the only field that says so.
func enterpriseProfile() *identity.Profile {
	return &identity.Profile{
		AccountUUID:      "acct-1",
		Email:            "a@example.com",
		OrganizationUUID: "org-9",
		OrganizationType: "claude_enterprise",
		RateLimitTier:    "default_claude_zero",
		SeatTier:         "enterprise_usage_based",
		BillingType:      "stripe_subscription_contracted",
	}
}

// The whole point of the field: an account added before seat_tier was captured
// holds "" for it, and nothing in the tree ever looked again. ApplyProfile is
// the second look.
func TestApplyProfileFillsInTheStringsAddTimeNeverRead(t *testing.T) {
	s := seed(t, identity.KindCredit)

	if err := s.ApplyProfile("acct-1", enterpriseProfile(), observed); err != nil {
		t.Fatalf("ApplyProfile() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.SeatTier != "enterprise_usage_based" {
		t.Errorf("SeatTier = %q, want enterprise_usage_based", a.SeatTier)
	}
	if a.Tier != "claude_enterprise" {
		t.Errorf("Tier = %q, want the RAW wire value claude_enterprise", a.Tier)
	}
	if a.RateLimitTier != "default_claude_zero" {
		t.Errorf("RateLimitTier = %q, want default_claude_zero", a.RateLimitTier)
	}
	if a.OrganizationUUID != "org-9" {
		t.Errorf("OrganizationUUID = %q, want org-9", a.OrganizationUUID)
	}
	if !a.ProfileFetchedAt.Equal(observed) {
		t.Errorf("ProfileFetchedAt = %v, want %v — an unstamped write cannot be told from one that never happened",
			a.ProfileFetchedAt, observed)
	}
}

// THE LOAD-BEARING ONE. Kind belongs to the usage axis: ApplyUsage revises it
// through identity.ReclassifyOnUsage, which calls Classify with a NIL profile
// and the real UsageShape. Add-time Classify runs the other way round -- a real
// profile and an EMPTY shape -- so re-running it here would overwrite a
// refinement made on evidence with the cruder guess that preceded it.
func TestApplyProfileLeavesKindToTheUsageAxis(t *testing.T) {
	s := seed(t, identity.KindCredit)

	// A profile that add-time Classify would file as a subscription, arriving
	// at an account a usage reading has already proven to be credit.
	p := enterpriseProfile()
	p.OrganizationType = "claude_max"
	p.RateLimitTier = "default_claude_max_20x"
	p.SeatTier = ""
	if err := s.ApplyProfile("acct-1", p, observed); err != nil {
		t.Fatalf("ApplyProfile() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.Kind != identity.KindCredit {
		t.Fatalf("Kind = %v, want credit — ApplyProfile overwrote what the usage axis decided", a.Kind)
	}
}

// A seat that reports no tier is EVIDENCE OF NONE, not absence of evidence.
// Every pro and max account measured answers seat_tier null, so a profile that
// says so must be able to clear a value the account used to carry -- an
// organization can move a seat off usage-based metering.
func TestApplyProfileClearsAStringTheProfileNowReportsAsEmpty(t *testing.T) {
	s := seed(t, identity.KindSubscription)
	if err := s.ApplyProfile("acct-1", enterpriseProfile(), observed); err != nil {
		t.Fatalf("seeding the tier: %v", err)
	}

	later := observed.Add(48 * time.Hour)
	p := enterpriseProfile()
	p.SeatTier = ""
	if err := s.ApplyProfile("acct-1", p, later); err != nil {
		t.Fatalf("ApplyProfile() error = %v", err)
	}

	a, _ := reopen(t).Get("acct-1")
	if a.SeatTier != "" {
		t.Errorf("SeatTier = %q, want empty — the wire reported none and that is an answer", a.SeatTier)
	}
	if !a.ProfileFetchedAt.Equal(later) {
		t.Errorf("ProfileFetchedAt = %v, want %v", a.ProfileFetchedAt, later)
	}
}

// A nil profile is the OTHER tri-state: the lookup failed. Blanking the fields
// on it would turn one unreachable endpoint into a fleet that forgot its tiers,
// and the stamp must not move either or the failure would buy a day of silence.
func TestApplyProfileRefusesANilProfile(t *testing.T) {
	s := seed(t, identity.KindCredit)
	if err := s.ApplyProfile("acct-1", enterpriseProfile(), observed); err != nil {
		t.Fatalf("seeding the tier: %v", err)
	}

	if err := s.ApplyProfile("acct-1", nil, observed.Add(48*time.Hour)); err == nil {
		t.Fatal("ApplyProfile(nil) = nil error; a failed lookup must not be recorded as a reading")
	}

	a, _ := reopen(t).Get("acct-1")
	if a.SeatTier != "enterprise_usage_based" {
		t.Errorf("SeatTier = %q; a nil profile blanked a field it had no evidence about", a.SeatTier)
	}
	if !a.ProfileFetchedAt.Equal(observed) {
		t.Errorf("ProfileFetchedAt = %v, want it left at %v", a.ProfileFetchedAt, observed)
	}
}

func TestApplyProfileReportsAnAccountItCannotFind(t *testing.T) {
	s := seed(t, identity.KindCredit)
	err := s.ApplyProfile("acct-missing", enterpriseProfile(), observed)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyProfile() error = %v, want ErrNotFound", err)
	}
}

// ProfileStale is what decides whether a poll spends a request on the profile
// endpoint. The zero value has to read STALE: an account added before the field
// existed carries it, and that account is the whole reason this exists.
func TestProfileStale(t *testing.T) {
	now := observed
	for _, tc := range []struct {
		name    string
		fetched time.Time
		want    bool
	}{
		{"never measured", time.Time{}, true},
		{"measured a minute ago", now.Add(-time.Minute), false},
		{"measured just inside the ttl", now.Add(-ProfileTTL + time.Minute), false},
		{"measured exactly a ttl ago", now.Add(-ProfileTTL), true},
		{"measured well outside the ttl", now.Add(-72 * time.Hour), true},
		// A stamp from the future is a clock that moved, not a fresh reading.
		// Treating it as fresh would freeze the account until real time caught
		// up, which for a badly set clock is never.
		{"stamped absurdly in the future", now.Add(72 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Account{ProfileFetchedAt: tc.fetched}
			if got := a.ProfileStale(now); got != tc.want {
				t.Errorf("ProfileStale() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Claude Code keeps its own cached profile for exactly a day -- the gate is
// `Date.now()-profileFetchedAt < XH` with `XH=86400000` in 2.1.246 -- and this
// tree copies the number rather than inventing one, so the two do not drift
// into re-fetching on different days.
func TestProfileTTLMatchesClaudeCodesOwn(t *testing.T) {
	if ProfileTTL != 24*time.Hour {
		t.Errorf("ProfileTTL = %v, want 24h — Claude Code's XH is 86400000 ms", ProfileTTL)
	}
}
