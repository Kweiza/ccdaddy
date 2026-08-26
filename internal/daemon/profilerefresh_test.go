package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// pollFor runs one poll the way Refresh dispatches it, so these tests exercise
// the same seam `ccdad list --refresh` and the daemon's own tick both go
// through rather than a path only a test can reach.
func pollFor(t *testing.T, e *Engine, a store.Account) error {
	t.Helper()
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	return e.poll(context.Background(), a, cfg, configuredThresholds(cfg), []string{a.UUID}, false)
}

func seatProfile(seat string) *identity.Profile {
	return &identity.Profile{
		AccountUUID:      "acct-1",
		Email:            "seat@example.com",
		OrganizationUUID: "org-9",
		OrganizationType: "claude_enterprise",
		RateLimitTier:    "default_claude_zero",
		SeatTier:         seat,
	}
}

// The repair path this item exists for. An account added before seat_tier was
// ever read carries "" for it and a zero ProfileFetchedAt, and since a switch
// began writing oauthAccount.seatTier from that field, the hole is no longer
// cosmetic: Claude Code's own Zu() cannot fire, so the seat loses the Opus tier
// it is entitled to. Nothing re-read the profile, so the only repair was a
// manual re-add.
func TestAPollRereadsAStaleProfileAndStoresTheSeatTier(t *testing.T) {
	isolate(t)
	acct := seedAccount(t, "acct-1", "org-9")

	var profiles atomic.Int32
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return creditOnlySnapshot(60.2255), nil
	})
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		profiles.Add(1)
		return seatProfile("enterprise_usage_based"), nil
	}

	if err := pollFor(t, e, acct); err != nil {
		t.Fatalf("poll() error = %v", err)
	}

	if got := profiles.Load(); got != 1 {
		t.Fatalf("the profile endpoint was called %d times, want 1", got)
	}
	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := after.Get("acct-1")
	if got.SeatTier != "enterprise_usage_based" {
		t.Errorf("SeatTier = %q, want enterprise_usage_based — the backfill did not land", got.SeatTier)
	}
	if got.RateLimitTier != "default_claude_zero" {
		t.Errorf("RateLimitTier = %q, want default_claude_zero", got.RateLimitTier)
	}
	if got.ProfileFetchedAt.IsZero() {
		t.Error("ProfileFetchedAt is zero — an unstamped write is re-read on every single poll")
	}
}

// The cost control. Every poll already spends a request on the usage endpoint,
// and the allowance belongs to the identity: doubling it for a fact that
// changes at most a few times in an account's life would be paid on every tick
// of every account, forever.
func TestAPollLeavesAFreshProfileAlone(t *testing.T) {
	isolate(t)
	seedAccount(t, "acct-1", "org-9")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return creditOnlySnapshot(60.2255), nil
	})
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		return seatProfile("enterprise_usage_based"), nil
	}

	// First poll measures it.
	acct := seedAccountRead(t, "acct-1")
	if err := pollFor(t, e, acct); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	var second atomic.Int32
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		second.Add(1)
		return seatProfile("enterprise_usage_based"), nil
	}
	// Re-read so the account carries the stamp the first poll wrote; the
	// caller's copy is a snapshot and the gate reads the caller's copy.
	acct = seedAccountRead(t, "acct-1")
	if err := pollFor(t, e, acct); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := second.Load(); got != 0 {
		t.Errorf("the profile endpoint was called %d times on a profile measured moments ago, want 0", got)
	}
}

// A profile lookup that fails must cost the poll NOTHING. The usage reading is
// what the poll exists for and it already succeeded; failing the poll on the
// secondary call would turn a profile-endpoint outage into a fleet with no
// usage readings, which is the axis every switch decision runs on.
func TestAFailedProfileLookupDoesNotFailThePoll(t *testing.T) {
	isolate(t)
	acct := seedAccount(t, "acct-1", "org-9")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return creditOnlySnapshot(60.2255), nil
	})
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		return nil, errors.New("the profile endpoint is down")
	}

	if err := pollFor(t, e, acct); err != nil {
		t.Fatalf("poll() error = %v; a failed profile lookup must not fail the reading", err)
	}

	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := after.Get("acct-1")
	if !got.ProfileFetchedAt.IsZero() {
		t.Errorf("ProfileFetchedAt = %v, want zero — a failed lookup must not buy %v of silence",
			got.ProfileFetchedAt, store.ProfileTTL)
	}
	if got.Kind == identity.KindAPIKey {
		t.Error("the usage reading did not land")
	}
}

// engineFor nils Freshen and ResolveOwner so a test cannot reach the network by
// forgetting to stub them. FetchProfile has to obey the same rule, and the
// production default is the opposite of the test one, so this pins the seam
// rather than the behaviour.
func TestEngineForRefusesTheProfileEndpointByDefault(t *testing.T) {
	isolate(t)
	acct := seedAccount(t, "acct-1", "org-9")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return creditOnlySnapshot(60.2255), nil
	})
	if e.FetchProfile != nil {
		t.Fatal("engineFor left FetchProfile wired to the real endpoint")
	}

	if err := pollFor(t, e, acct); err != nil {
		t.Fatalf("poll() error = %v; a nil FetchProfile must be a skipped step, not a crash", err)
	}
	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := after.Get("acct-1"); !got.ProfileFetchedAt.IsZero() {
		t.Error("a nil FetchProfile stamped the account anyway")
	}
}

// seedAccountRead re-reads an account the store already holds, so a caller can
// hand poll the CURRENT row rather than the one it seeded.
func seedAccountRead(t *testing.T, uuid string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("%s is not in the store", uuid)
	}
	return a
}
