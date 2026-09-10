package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func seedUnverifiedSubscriptionOnBackoff(t *testing.T) store.Account {
	t.Helper()
	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{UUID: "pending", Provider: provider.Claude, Email: "pending@example.com", Tier: "claude_max", ProfileFetchedAt: tickEpoch.Add(-time.Hour), AddedAt: tickEpoch.Add(-24 * time.Hour)}
	if err := s.Add(a, oauthBlob("RT-pending")); err != nil {
		t.Fatal(err)
	}
	seedEntry(t, a.UUID, usage.Entry{Snapshot: snapshotWith(20), FetchedAt: tickEpoch.Add(-time.Hour), NextPollAt: tickEpoch.Add(time.Hour), Poll: usage.PollState{Interval: 30 * time.Minute, LastRateLimited: tickEpoch.Add(-time.Minute)}})
	return a
}

func TestTickChecksSubscriptionDespiteQuotaBackoffBeforeItCanBeSelected(t *testing.T) {
	a := seedUnverifiedSubscriptionOnBackoff(t)
	var profiles, usages atomic.Int32
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) { usages.Add(1); return snapshotWith(20), nil })
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		profiles.Add(1)
		return &identity.Profile{AccountUUID: a.UUID, SubscriptionStatus: "canceled"}, nil
	}
	tick(t, e)
	if profiles.Load() != 1 || usages.Load() != 0 {
		t.Fatalf("profile/usage calls = %d/%d", profiles.Load(), usages.Load())
	}
	if liveUUID(t) == a.UUID {
		t.Fatal("unverified subscription was selected before its profile was checked")
	}
	if !seedAccountRead(t, a.UUID).SubscriptionInactive() {
		t.Fatal("canceled profile did not reach the store")
	}
	tick(t, e)
	if got := e.Snapshot().Accounts[0].State; got != StateSubscriptionInactive {
		t.Fatalf("state = %s", got)
	}
	entry, _ := cacheEntry(t, a.UUID)
	if !entry.NextPollAt.Equal(tickEpoch.Add(time.Hour)) {
		t.Fatal("profile refresh changed quota backoff")
	}
}

func TestManualRefreshChecksSubscriptionWhileLeavingQuotaHeld(t *testing.T) {
	a := seedUnverifiedSubscriptionOnBackoff(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	var usages atomic.Int32
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		usages.Add(1)
		return nil, errors.New("quota must stay held")
	})
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		return &identity.Profile{AccountUUID: a.UUID, SubscriptionStatus: "canceled"}, nil
	}
	results := e.Refresh(context.Background(), s, []store.Account{a}, config.Defaults(), "")
	if len(results) != 1 || results[0].State != RefreshHeld || usages.Load() != 0 {
		t.Fatalf("quota result = %+v", results)
	}
	if !seedAccountRead(t, a.UUID).SubscriptionInactive() {
		t.Fatal("held quota skipped the profile refresh")
	}
}

func TestFailedProfileChecksBackOffAcrossEngineRestarts(t *testing.T) {
	a := seedUnverifiedSubscriptionOnBackoff(t)
	var profiles atomic.Int32
	makeEngine := func() *Engine {
		e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
			t.Error("quota polled during backoff")
			return nil, nil
		})
		e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
			profiles.Add(1)
			return nil, errors.New("profile unavailable")
		}
		return e
	}
	e := makeEngine()
	tick(t, e)
	tick(t, e)
	restarted := makeEngine()
	tick(t, restarted)
	if profiles.Load() != 1 {
		t.Fatalf("profile attempts = %d, want one persisted retry claim", profiles.Load())
	}
	if got := restarted.Snapshot().Accounts[0].State; got != StateSubscriptionPending {
		t.Fatalf("unchecked profile state = %s", got)
	}
	got := seedAccountRead(t, a.UUID)
	if got.SubscriptionStatus != "" || !got.ProfileRetryAt.Equal(tickEpoch.Add(store.ProfileRetryInterval)) {
		t.Fatalf("failure altered subscription or lost deadline: %+v", got)
	}
	restarted.Now = func() time.Time { return tickEpoch.Add(store.ProfileRetryInterval) }
	tick(t, restarted)
	if profiles.Load() != 2 {
		t.Fatalf("profile did not retry after its own deadline: %d", profiles.Load())
	}
}
