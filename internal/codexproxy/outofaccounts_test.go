package codexproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two instants after the pinned clock the two waiting tests below install.
// They are 2026-09-21T14:13:20Z and 2026-09-21T14:21:40Z, which is why the
// clock is pinned rather than left as time.Now: `recordLimit` records a body's
// resets_at as KNOWN only while it is still in the future, so a wall clock
// would turn these two tests red on that date and stay red.
const (
	earlierReset = 1790000000
	laterReset   = 1790000500
)

// The waiting answer is the EARLIEST reset among the accounts still waiting,
// and this table is what pins "earliest" rather than "whichever reading the
// loop took last". The fake upstream keys on the Authorization header instead
// of on a call counter, because a counter makes each account's reset a function
// of the order the loop happens to walk -- and that order is precisely what
// must not decide the answer. Row one puts the earlier reset on the account
// tried first, so it is red on an accumulator that keeps the LAST reading; row
// two puts it on the account tried last, so it is red on one that keeps the
// FIRST. Neither row alone catches both.
//
// Each row builds its own fixture and its own server on purpose. The LimitBook
// and the thread map both hang off the *Server, so a shared one would carry row
// one's recorded limits into row two, and a thread that already has an account
// answers the first 429 straight back to the client instead of ever running the
// search out of accounts -- a second way for a row to pass having measured
// nothing.
func TestEveryAccountWaitingAnswersWithTheEarliestReset(t *testing.T) {
	cases := []struct {
		name    string
		resetOf map[string]int
	}{
		{"earliest reset first", map[string]int{"access-a": earlierReset, "access-b": laterReset}},
		{"earliest reset last", map[string]int{"access-a": laterReset, "access-b": earlierReset}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				reset, ok := c.resetOf[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
				if !ok {
					t.Error("the upstream was reached with a bearer no account in this row owns")
					reset = laterReset
				}
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.Itoa(reset)+`}}`)
			})
			f.add("uuid-a", "a@example.com", "access-a")
			f.add("uuid-b", "b@example.com", "access-b")
			cfg := f.config()
			cfg.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
			cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
			s := f.server(t, cfg)

			w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", w.Code)
			}
			if n := len(f.took()); n != 2 {
				t.Fatalf("the upstream saw %d requests, want both accounts tried", n)
			}
			want := `{"error":{"type":"usage_limit_reached","resets_at":` + strconv.Itoa(earlierReset) + `}}`
			if got := strings.TrimSpace(w.Body.String()); got != want {
				t.Fatalf("body =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// codex renders an absent resets_at as "try again later" and a zero one as a
// date in 1970, so a reset nothing stated is OMITTED rather than zeroed.
func TestAWaitingAccountWithNoStatedResetOmitsTheField(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"limit reached"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	cfg := f.config()
	cfg.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	want := `{"error":{"type":"usage_limit_reached"}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestOnlyDeadAccountsGetTheBrandedRelogin(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the upstream was reached for an account with no usable credential")
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.forget("uuid-a")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	want := "{\"error\":{\"type\":\"ccdad_needs_relogin\",\"message\":\"ccdad: a@example.com needs a new login; run `ccdad add codex`\"}}"
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestAPinnedSingletonThatIsDeadNamesThatAccount(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the upstream was reached for an account with no usable credential")
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.forget("uuid-b")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	w := post(s, pinnedPrefix+"uuid-b", nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "b@example.com") {
		t.Fatalf("body = %s, want the pinned account named", w.Body.String())
	}
}

// One account that could not be REACHED and one that has no credential at all
// is the request that separates the two sources the answer is built from. The
// dead one is named; the unreachable one must not be, because "the network is
// down" and "log in again" send a user to two different places.
//
// The proxy is pointed at a listener that has already been closed, so uuid-a's
// send fails in the transport and never becomes an attempt at all, while
// uuid-b's never leaves this process: its credential blob is gone. Reading DEAD
// off the candidate order rather than off what the search marked dead names
// a@example.com here -- the account that is fine.
func TestAnUnreachableAccountIsNeverNamedAlongsideADeadOne(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the fixture's own upstream was reached; this test points the proxy at a closed listener")
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.forget("uuid-b")
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := closed.URL
	closed.Close()
	cfg := f.config()
	cfg.Upstream = url
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "b@example.com") {
		t.Fatalf("body = %s, want the account with no credential named", body)
	}
	if strings.Contains(body, "a@example.com") {
		t.Fatalf("body = %s, names an account whose only trouble was an upstream nobody could reach", body)
	}
	// The two accounts have to have failed for the two DIFFERENT reasons, or
	// the assertions above are describing a partition that never happened: if
	// send ever dialled before it read the credential, both would land in the
	// unreachable set and this would quietly become a weaker test that still
	// passed.
	var sawUnreachable, sawDead bool
	for _, line := range f.logged() {
		if strings.Contains(line, "could not reach the upstream") && strings.Contains(line, "uuid-a") {
			sawUnreachable = true
		}
		if strings.Contains(line, "cannot serve") && strings.Contains(line, "uuid-b") {
			sawDead = true
		}
	}
	if !sawUnreachable || !sawDead {
		t.Fatalf("logs = %v, want uuid-a reported as unreachable and uuid-b as unable to serve", f.logged())
	}
}

func TestAStoreWithNoCodexAccountsAtAllIsUnavailableRatherThanAFault(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ccdad_unavailable") {
		t.Fatalf("body = %s, want the unavailable answer", w.Body.String())
	}
}

// WAITING and DEAD present at the SAME time is the only request that can say
// which arm of the partition wins, and until this row existed nothing drove it:
// every other test in this file produces one state or the other, so swapping
// the two switch arms left the whole package green.
//
// Account a is out of quota with a stated reset; account b's credential blob is
// gone, which is what a revoked grant looks like from the proxy's side. The
// answer has to be the 429. Getting it backwards tells the user that
// b@example.com needs a new login -- an account that was never the reason this
// turn failed -- and leaves the one that will come back in an hour unmentioned.
func TestAWaitingAccountIsAnsweredEvenWhenAnotherIsDead(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.Itoa(earlierReset)+`}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	f.forget("uuid-b")
	cfg := f.config()
	cfg.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	want := `{"error":{"type":"usage_limit_reached","resets_at":` + strconv.Itoa(earlierReset) + `}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	// Both halves of the partition really were populated. Without this the row
	// degrades into the waiting-only case above, which passes whichever order
	// the arms are in, and the assertion above would be measuring nothing.
	if n := len(f.took()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want only the account that still had a credential to send", n)
	}
	var sawDead bool
	for _, line := range f.logged() {
		if strings.Contains(line, "cannot serve") && strings.Contains(line, "uuid-b") {
			sawDead = true
		}
	}
	if !sawDead {
		t.Fatalf("logs = %v, want uuid-b reported as unable to serve", f.logged())
	}
}

// An account can be in BOTH sets at once, and then it is dead: the book keeps a
// 429 for as long as the window it describes, so an account that ran out of
// quota an hour ago and has since lost its credential is still recorded as
// waiting when the next request finds it dead. Waiting has to be read as "has
// quota coming back", which a dead account does not, so the WAITING scan skips
// anything the search marked dead.
//
// Reporting it as merely waiting hands codex a reset time and a machine that
// waits out a clock for an account that will never answer again -- and never
// tells the user the one thing that would fix it. Nothing else here drives an
// account into both sets, so dropping that skip left the package green.
func TestADeadAccountIsNeverAnsweredAsMerelyWaiting(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.Itoa(earlierReset)+`}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	cfg := f.config()
	cfg.Now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	// The 429 that puts uuid-a in the book, with a reset far enough ahead of
	// the pinned clock that it is still in force for the second request.
	if first := post(s, unpinnedSecret, nil, `{"input":[]}`); first.Code != http.StatusTooManyRequests {
		t.Fatalf("the first request answered %d, want the 429 that records the limit", first.Code)
	}
	// And then the grant goes, between one request and the next.
	f.forget("uuid-a")

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	want := "{\"error\":{\"type\":\"ccdad_needs_relogin\",\"message\":\"ccdad: a@example.com needs a new login; run `ccdad add codex`\"}}"
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}
