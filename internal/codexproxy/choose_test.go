package codexproxy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// threeAccounts is the shape every choice test starts from.
func threeAccounts(t *testing.T) (*fixture, *Server) {
	t.Helper()
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.add("uuid-c", "c@example.com", "access-c")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b", "uuid-c"} }
	return f, f.server(t, cfg)
}

func TestALaunchPinBeatsTheThreadPinAndThePointer(t *testing.T) {
	f, s := threeAccounts(t)
	f.serving(t, "uuid-c")
	s.rememberThread("thread-7", "uuid-b")

	order, pinned := s.chooseOrder(codexlaunch.Record{Pin: "uuid-a"}, "thread-7")
	if !pinned {
		t.Fatal("a launch pin did not report as pinned")
	}
	if !reflect.DeepEqual(order, []string{"uuid-a"}) {
		t.Fatalf("order = %v, want exactly the pinned account", order)
	}
}

func TestAnExistingThreadFollowsTheServingPointer(t *testing.T) {
	f, s := threeAccounts(t)
	f.serving(t, "uuid-c")
	s.rememberThread("thread-7", "uuid-b")

	order, pinned := s.chooseOrder(codexlaunch.Record{}, "thread-7")
	if pinned {
		t.Error("a thread pin reported as a launch pin; a launch pin must never bill another account and a thread pin may")
	}
	if len(order) == 0 || order[0] != "uuid-c" {
		t.Fatalf("order = %v, want uuid-c first", order)
	}
	if !reflect.DeepEqual(order, []string{"uuid-c", "uuid-a", "uuid-b"}) {
		t.Fatalf("order = %v, want the serving account then the rest of the ranking", order)
	}
}

func TestANewThreadLeadsWithTheServingPointer(t *testing.T) {
	f, s := threeAccounts(t)
	f.serving(t, "uuid-c")

	order, _ := s.chooseOrder(codexlaunch.Record{}, "thread-new")
	if !reflect.DeepEqual(order, []string{"uuid-c", "uuid-a", "uuid-b"}) {
		t.Fatalf("order = %v, want the pointed account then the rest of the ranking", order)
	}
}

func TestAPointerNamingAnAccountTheStoreNoLongerHasReadsAsNoPointer(t *testing.T) {
	f, s := threeAccounts(t)
	f.serving(t, "uuid-removed")

	order, _ := s.chooseOrder(codexlaunch.Record{}, "thread-new")
	if !reflect.DeepEqual(order, []string{"uuid-a", "uuid-b", "uuid-c"}) {
		t.Fatalf("order = %v, want the ranking untouched", order)
	}
}

// disabled is a rotation policy, not a per-request gate: an account the user
// pointed at keeps serving, and the lane rotates away on its next decision.
//
// This is the one choice test that deliberately does NOT build on
// threeAccounts, and the reason is the rule it is trying to see. threeAccounts
// stubs RankedEligible with a fixed list that still contains uuid-c, and a
// stubbed ranking makes Disabled unobservable: chooseOrder returns the stub
// without ever calling eligible(), which is the only function in choose.go
// that reads a.Disabled, so the whole test would pass byte-for-byte unchanged
// with the Disabled = true line deleted. Left store-derived, the ranking
// genuinely excludes uuid-c, and the two halves of the rule then sit in one
// assertion: the tail ["uuid-a", "uuid-b"] is the evidence that uuid-c was
// dropped from eligibility, and uuid-c leading anyway is the evidence that the
// pointer is not filtered by it. That is why this compares the whole slice
// instead of only its first element.
func TestADisabledAccountStillServesWhenItIsPointedAt(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.add("uuid-c", "c@example.com", "access-c")
	f.mutate("uuid-c", func(a *store.Account) { a.Disabled = true })
	s := f.server(t, f.config())
	f.serving(t, "uuid-c")

	order, _ := s.chooseOrder(codexlaunch.Record{}, "thread-new")
	if !reflect.DeepEqual(order, []string{"uuid-c", "uuid-a", "uuid-b"}) {
		t.Fatalf("order = %v, want the disabled but pointed-at account leading the eligible rest", order)
	}
}

func TestWithNoRankingTheOrderComesFromTheStoreAndSkipsTheIneligible(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.add("uuid-c", "c@example.com", "access-c")
	f.add("uuid-d", "d@example.com", "access-d")
	f.mutate("uuid-b", func(a *store.Account) { a.Disabled = true })
	f.mutate("uuid-c", func(a *store.Account) { a.Elsewhere = true })
	f.mutate("uuid-d", func(a *store.Account) {
		a.CodexReloginFor = codexauth.RefreshTokenHash("refresh-uuid-d")
	})
	s := f.server(t, f.config())

	order, _ := s.chooseOrder(codexlaunch.Record{}, "thread-new")
	if !reflect.DeepEqual(order, []string{"uuid-a"}) {
		t.Fatalf("order = %v, want only the one eligible account", order)
	}
}

// The stand-in row has to carry Provider: provider.Codex for this test to be
// about credentials at all. eligible() drops a row on a.Provider !=
// provider.Codex BEFORE it ever looks a credential up, and the zero value of
// store.Account.Provider is the empty provider ID, so a row left with no
// provider set is thrown out by the provider term and the credential term is
// never reached for it. With the provider filled in, the missing credential is
// the only thing that can exclude uuid-x, which is what makes deleting the
// credential check from eligible() turn this red.
func TestAnAccountWithNoCodexCredentialIsNotEligible(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	f.accounts = append(f.accounts, store.Account{UUID: "uuid-x", Idx: 9, Provider: provider.Codex})
	s := f.server(t, f.config())

	order, _ := s.chooseOrder(codexlaunch.Record{}, "")
	if !reflect.DeepEqual(order, []string{"uuid-a"}) {
		t.Fatalf("order = %v, want only the account with a stored codex credential", order)
	}
}

// The provider term needs a row of its own, now that uuid-x above carries
// provider.Codex. The fixture's add() only ever builds Codex rows, so uuid-y
// is the only non-Codex account anywhere in this package, and it is given a
// stored credential on purpose: with a credential and no relogin mark against
// it, the provider check is the single remaining reason it can be excluded.
// Take that check out of eligible() and uuid-y joins the order. This is not a
// hypothetical row either -- most accounts in a real store are Claude ones,
// and every one of them is read by this same account reader.
func TestAnAccountFromAnotherProviderIsNotEligible(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	f.accounts = append(f.accounts, store.Account{UUID: "uuid-y", Idx: 10, Provider: provider.Claude})
	f.creds["uuid-y"] = codexauth.Credential{
		AccessToken:  "access-y",
		RefreshToken: "refresh-uuid-y",
		AccountID:    "workspace-uuid-y",
		UserID:       "uuid-y",
	}.ToBlob()
	s := f.server(t, f.config())

	order, _ := s.chooseOrder(codexlaunch.Record{}, "")
	if !reflect.DeepEqual(order, []string{"uuid-a"}) {
		t.Fatalf("order = %v, want only the codex account", order)
	}
}

// Concurrent responses update the remembered account while new requests read
// it for turn-state ownership. Routing must still follow the serving pointer.
func TestConcurrentRequestsFollowServingWhileRememberingThreadAccounts(t *testing.T) {
	f, s := threeAccounts(t)
	f.serving(t, "uuid-c")
	s.rememberThread("thread-7", "uuid-b")

	const workers = 64
	// Each worker writes only its own result.
	leads := make([]string, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				s.rememberThread("thread-7", "uuid-b")
				return
			}
			s.threadAccount("thread-7")
			order, _ := s.chooseOrder(codexlaunch.Record{}, "thread-7")
			if len(order) > 0 {
				leads[i] = order[0]
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < workers; i += 2 {
		if leads[i] != "uuid-c" {
			t.Fatalf("worker %d led with %q, want uuid-c: the serving pointer overrides the previous account", i, leads[i])
		}
	}
}

func TestTheThreadIdComesFromTheHeaderCodexSends(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, nil)
	if got := threadIDOf(r); got != "" {
		t.Fatalf("threadIDOf() = %q with no header, want the empty string", got)
	}
	r.Header.Set(threadIDHeader, "thread-7")
	if got := threadIDOf(r); got != "thread-7" {
		t.Fatalf("threadIDOf() = %q, want thread-7", got)
	}
}
