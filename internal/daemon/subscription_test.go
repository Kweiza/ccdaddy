package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/identity"
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
