package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

func (e *Engine) profileDue(a store.Account, now time.Time) bool {
	return e.FetchProfile != nil && e.AccessToken != nil && a.Provider == provider.Claude && a.ProfileStale(now) &&
		(!a.ProfileRetryAt.After(now) || a.ProfileRetryAt.Sub(now) > store.ProfileTTL)
}

// pollProfile never reads or changes the usage schedule. The retry claim is
// persisted before obtaining a token, so token failures are throttled too.
func (e *Engine) pollProfile(ctx context.Context, a store.Account) {
	ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
	defer cancel()
	e.checkProfile(ctx, a, "", e.now(), false)
}

func (e *Engine) checkProfile(ctx context.Context, a store.Account, token string, now time.Time, force bool) {
	if e.FetchProfile == nil {
		return
	}
	claimed := false
	err := store.WithStore(func(s *store.Store) error {
		var err error
		claimed, err = s.BeginProfileRefresh(a.UUID, now, force)
		return err
	})
	if err != nil || !claimed {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			e.logf("claiming %s's profile refresh failed: %v", a.UUID, err)
		}
		return
	}
	if token == "" {
		if e.AccessToken == nil {
			return
		}
		token, err = e.AccessToken(ctx, a.UUID)
		if err != nil {
			e.logf("reading %s's token for its profile failed: %v", a.UUID, err)
			return
		}
	}
	p, err := e.FetchProfile(ctx, token)
	if err != nil {
		e.logf("re-reading %s's profile failed: %v", a.UUID, err)
		return
	}
	if p == nil || p.AccountUUID != a.UUID {
		e.logf("re-reading %s's profile returned a different or missing account", a.UUID)
		return
	}
	if err := store.WithStore(func(s *store.Store) error { return s.ApplyProfile(a.UUID, p, now) }); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.logf("recording %s's profile failed: %v", a.UUID, err)
	}
}

func (e *Engine) refreshProfiles(ctx context.Context, s *store.Store, accounts []store.Account) {
	var wg sync.WaitGroup
	for _, a := range accounts {
		if !e.profileDue(a, e.now()) || !pollable(s, a) {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); e.pollProfile(ctx, a) }()
	}
	wg.Wait()
}
