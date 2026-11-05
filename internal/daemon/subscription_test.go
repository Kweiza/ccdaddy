package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestFailedUsageStillRefreshesACanceledSubscription(t *testing.T) {
	for _, failure := range []error{usage.ErrForbidden, usage.ErrRateLimited} {
		t.Run(failure.Error(), func(t *testing.T) {
			isolate(t)
			a := seedAccount(t, "acct-1", "org-9")
			a.ProfileFetchedAt = time.Now() // old versions stamped tier without reading status
			e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) { return nil, failure })
			e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
				return &identity.Profile{AccountUUID: a.UUID, OrganizationType: "claude_max", SubscriptionStatus: "canceled"}, nil
			}
			if err := pollFor(t, e, a); !errors.Is(err, failure) {
				t.Fatalf("poll error = %v", err)
			}
			got := seedAccountRead(t, a.UUID)
			if !got.SubscriptionInactive() {
				t.Fatalf("canceled subscription was not stored: %+v", got)
			}
			if got.Disabled {
				t.Fatal("subscription refresh changed the user's disabled flag")
			}
		})
	}
}

func TestPermissionFailureRechecksAnActiveSubscriptionBeforeTheDailyTTL(t *testing.T) {
	isolate(t)
	a := seedAccount(t, "acct-1", "org-9")
	a.SubscriptionStatus = "active"
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) { return nil, usage.ErrForbidden })
	a.ProfileFetchedAt = e.now().Add(-time.Hour)
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		return &identity.Profile{AccountUUID: a.UUID, SubscriptionStatus: "canceled"}, nil
	}
	_ = pollFor(t, e, a)
	if !seedAccountRead(t, a.UUID).SubscriptionInactive() {
		t.Fatal("permission refusal left the paid subscription cached for a day")
	}
}

// The usage endpoint answers a renewed account exactly as it answered the lapsed
// one, so only a profile re-read can notice the renewal, and it has to come
// sooner than the daily TTL or the account sits out of rotation for a day.
func TestATickNoticesARenewalWithinTheRetryInterval(t *testing.T) {
	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{
		UUID: "lapsed", Provider: provider.Claude, Email: "lapsed@example.com", Tier: "claude_free",
		SubscriptionStatus: "canceled", ProfileFetchedAt: tickEpoch.Add(-store.ProfileRetryInterval),
		AddedAt: tickEpoch.Add(-24 * time.Hour),
	}
	if err := s.Add(a, oauthBlob("RT-lapsed")); err != nil {
		t.Fatal(err)
	}
	var profiles atomic.Int32
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) { return snapshotWith(20), nil })
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		profiles.Add(1)
		return &identity.Profile{AccountUUID: a.UUID, OrganizationType: "claude_max", SubscriptionStatus: "active"}, nil
	}
	tick(t, e)
	if profiles.Load() != 1 {
		t.Fatalf("profile lookups = %d, want the canceled subscription re-read", profiles.Load())
	}
	got := seedAccountRead(t, a.UUID)
	if got.SubscriptionInactive() || got.Tier != "claude_max" {
		t.Fatalf("renewal was not stored: status %q tier %q", got.SubscriptionStatus, got.Tier)
	}
}

func TestCodexPollUpdatesThePlanFromTheUsageResponse(t *testing.T) {
	isolate(t)
	a := seedCodexAccount(t, "cx-1")
	e := codexEngine(t, codexTokensAreFine, func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
		return codexSnapshot(10), codexusage.Identity{UserID: a.UUID, AccountID: "acct-cx-1", PlanType: "free"}, nil
	})
	if err := e.codexPoll(context.Background(), a, strategy.Thresholds{}, false); err != nil {
		t.Fatal(err)
	}
	got := seedAccountRead(t, a.UUID)
	if got.Tier != "free" || got.ProfileFetchedAt.IsZero() {
		t.Fatalf("plan update missing: %+v", got)
	}
	if got.Disabled {
		t.Fatal("free Codex quota was disabled")
	}
}
