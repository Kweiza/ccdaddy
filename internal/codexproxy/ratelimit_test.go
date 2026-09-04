package codexproxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// limited429 is the answer codex's own upstream sends when an account is spent.
const limited429 = `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1790000000,"plan_type":"pro"}}`

// twoAccounts serves 429 from the first attempt and 200 from every later one.
func rateLimitedThenFine(t *testing.T, retryAfter string) (*fixture, *int32) {
	t.Helper()
	var calls int32
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, limited429)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	return f, &calls
}

// A pin is a promise about which account pays. A 429 on a pinned launch is
// answered, never routed around.
func TestALaunchPinNeverBillsAnotherAccount(t *testing.T) {
	f, _ := rateLimitedThenFine(t, "")
	f.serving(t, "uuid-a")
	s := f.server(t, f.config())

	w := post(s, pinnedPrefix+"uuid-a", map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's 429", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != limited429 {
		t.Fatalf("body =\n%s\nwant the upstream's own\n%s", got, limited429)
	}
	if n := len(f.took()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1 — a pin was routed around", n)
	}
	if _, ok := s.book.LimitedUntil("uuid-a", time.Now()); !ok {
		t.Error("the rate limit was not recorded in the book")
	}
}

func TestAThreadsFirstRequestIsReplayedOnTheNextAccount(t *testing.T) {
	f, _ := rateLimitedThenFine(t, "")
	f.serving(t, "uuid-a")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, map[string]string{
		threadIDHeader:       "thread-1",
		"X-Codex-Turn-State": "ts-1",
	}, `{"input":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the replacement's 200 (%s)", w.Code, w.Body.String())
	}
	took := f.took()
	if len(took) != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", len(took))
	}
	if got := took[0].header.Get("Authorization"); got != "Bearer access-a" {
		t.Errorf("first attempt Authorization = %q, want the pointed account's", got)
	}
	if got := took[1].header.Get("Authorization"); got != "Bearer access-b" {
		t.Errorf("replay Authorization = %q, want the next account's", got)
	}
	if got := took[0].header.Get("X-Codex-Turn-State"); got != "ts-1" {
		t.Errorf("first attempt lost the turn state: %q", got)
	}
	// The turn state is meaningful only to the account that issued it.
	if got := took[1].header.Get("X-Codex-Turn-State"); got != "" {
		t.Errorf("the replay carried the turn state %q to another account", got)
	}
	// The thread now belongs to whoever answered it.
	if uuid, ok := s.threadAccount("thread-1"); !ok || uuid != "uuid-b" {
		t.Errorf("threadAccount() = (%q, %v), want uuid-b", uuid, ok)
	}
}

func TestAThreadWithPriorResponsesIsNotMovedByDefault(t *testing.T) {
	f, _ := rateLimitedThenFine(t, "")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)
	s.rememberThread("thread-1", "uuid-a")

	w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the 429 (%s)", w.Code, w.Body.String())
	}
	if n := len(f.took()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1 — a thread carrying one account's reasoning was moved", n)
	}
}

func TestAThreadWithPriorResponsesMovesWhenCrossAccountReplayIsOn(t *testing.T) {
	f, _ := rateLimitedThenFine(t, "")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	cfg.CrossAccountReplay = true
	s := f.server(t, cfg)
	s.rememberThread("thread-1", "uuid-a")

	w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the replacement's 200 (%s)", w.Code, w.Body.String())
	}
	if n := len(f.took()); n != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", n)
	}
}

func TestAReplacementThatFailsAnswersTheOriginalRateLimit(t *testing.T) {
	var calls int32
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, limited429)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the original 429", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != limited429 {
		t.Fatalf("body =\n%s\nwant the original rate-limit answer", got)
	}
}

func TestNothingIsReplayedOnceAByteHasReachedTheClient(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	s := f.server(t, cfg)

	func() {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	}()

	if n := len(f.took()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1 — a turn was replayed after bytes had reached the client", n)
	}
}

func TestTheRecordedLimitPrefersRetryAfterThenTheBodyThenAMinute(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	reset := time.Unix(1790000000, 0).UTC()

	cases := []struct {
		name       string
		retryAfter string
		body       string
		wantUntil  time.Time
		wantKnown  bool
	}{
		{"retry-after wins", "120", limited429, now.Add(2 * time.Minute), true},
		{"body resets_at", "", limited429, reset, true},
		{"neither", "", `{"error":{"type":"usage_limit_reached"}}`, now.Add(unknownLimitFor), false},
		{"unparseable body", "", `not json`, now.Add(unknownLimitFor), false},
		{"a retry-after of zero is not an answer", "0", `{}`, now.Add(unknownLimitFor), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
			cfg := f.config()
			cfg.Now = func() time.Time { return now }
			s := f.server(t, cfg)

			h := http.Header{}
			if c.retryAfter != "" {
				h.Set("Retry-After", c.retryAfter)
			}
			s.recordLimit("uuid-a", &attempt{uuid: "uuid-a", status: 429, header: h, body: []byte(c.body)})

			until, ok := s.book.LimitedUntil("uuid-a", now)
			if !ok {
				t.Fatal("the account was not held out at all")
			}
			if !until.Equal(c.wantUntil) {
				t.Fatalf("held until %v, want %v", until, c.wantUntil)
			}
			if _, known := s.book.ResetsAt("uuid-a", now); known != c.wantKnown {
				t.Fatalf("ResetsAt known = %v, want %v", known, c.wantKnown)
			}
		})
	}
}

// The magnitude of the fallback is the one thing the table above cannot state.
// Its three fallback rows are phrased as now.Add(unknownLimitFor), so every
// expectation shrinks along with the constant: change the constant to
// 60 * time.Minute and all five subtests stay green. Nothing downstream catches
// it either -- LimitBook.MarkLimitedFor stores the deadline it is handed with no
// cap and LimitedUntil returns it unaltered, so an hour really does flow through
// -- and no other test in this package reads the book after a 429 that named no
// reset. An account would be held out for an hour on a guess, which is the exact
// outcome the constant's own comment says it exists to prevent.
//
// So this is the one test that cannot be phrased in terms of the constant, and
// it is written the way internal/pollpolicy/pollpolicy_test.go already guards
// its own table of cadences: state the number literally, once, in a test whose
// only job is to hold it.
func TestTheUnknownLimitIsOneMinute(t *testing.T) {
	if unknownLimitFor != 60*time.Second {
		t.Errorf("unknownLimitFor = %s, want 60s -- the wait ccdad promises when a 429 named no reset", unknownLimitFor)
	}
}
