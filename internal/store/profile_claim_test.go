package store

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

func TestOnlyOneConcurrentProfileLookupCanClaimAnAccount(t *testing.T) {
	seed(t, identity.KindSubscription)
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open()
			if err != nil {
				t.Error(err)
				return
			}
			ok, err := s.BeginProfileRefresh("acct-1", observed, false)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("profile claims = %d", claims.Load())
	}
	if got, _ := reopen(t).Get("acct-1"); !got.ProfileRetryAt.Equal(observed.Add(ProfileRetryInterval)) {
		t.Fatal("retry claim was not persisted")
	}
}

func TestSuccessfulProfileRefreshClearsRetryWithoutAllowingImmediateForcedChecks(t *testing.T) {
	s := seed(t, identity.KindSubscription)
	if ok, err := s.BeginProfileRefresh("acct-1", observed, false); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	p := enterpriseProfile()
	p.SubscriptionStatus = "active"
	if err := s.ApplyProfile("acct-1", p, observed); err != nil {
		t.Fatal(err)
	}
	if a, _ := reopen(t).Get("acct-1"); !a.ProfileRetryAt.IsZero() {
		t.Fatal("success retained the failed-lookup deadline")
	}
	if ok, err := s.BeginProfileRefresh("acct-1", observed.Add(time.Minute), true); err != nil || ok {
		t.Fatalf("early forced claim = %v, %v", ok, err)
	}
	if ok, err := s.BeginProfileRefresh("acct-1", observed.Add(ProfileRetryInterval), true); err != nil || !ok {
		t.Fatalf("due forced claim = %v, %v", ok, err)
	}
}
