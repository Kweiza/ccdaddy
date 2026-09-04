package codexproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
)

func TestAnUnauthorizedAnswerIsRefreshedOnceAndReplayed(t *testing.T) {
	var calls int32
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	var refreshes int32
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	cfg.RefreshFunc = func(_ context.Context, uuid, triggeredBy string) (codexauth.Outcome, error) {
		atomic.AddInt32(&refreshes, 1)
		if triggeredBy != "access-a" {
			t.Errorf("the refresher was told it saw %q, want the token the attempt used", triggeredBy)
		}
		f.rotate(uuid, "access-a-rotated")
		return codexauth.Outcome{Kind: codexauth.Rotated}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the refresh (%s)", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("the refresher was asked %d times, want 1", got)
	}
	took := f.took()
	if len(took) != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", len(took))
	}
	if got := took[1].header.Get("Authorization"); got != "Bearer access-a-rotated" {
		t.Fatalf("the replay Authorization = %q, want the header rebuilt from the rotated token", got)
	}
}

// A 401 that survives a successful refresh is the endpoint's answer about the
// account, not ccdad's bookkeeping. It is passed through and marks nothing.
func TestAPersistentUnauthorizedAfterARefreshIsPassedThrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"the upstream still says no"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	var refreshes int32
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	cfg.RefreshFunc = func(_ context.Context, uuid, _ string) (codexauth.Outcome, error) {
		atomic.AddInt32(&refreshes, 1)
		f.rotate(uuid, "access-a-rotated")
		return codexauth.Outcome{Kind: codexauth.Rotated}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream's 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "the upstream still says no") {
		t.Fatalf("body = %s, want the upstream's own", w.Body.String())
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("the refresher was asked %d times, want 1", got)
	}
}

func TestATerminalRefreshMovesToTheNextAccount(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	cfg.RefreshFunc = func(_ context.Context, _, _ string) (codexauth.Outcome, error) {
		return codexauth.Outcome{Kind: codexauth.Terminal, Code: "refresh_token_expired"}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the next account's 200 (%s)", w.Code, w.Body.String())
	}
	if n := len(f.took()); n != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", n)
	}
}

func TestATerminalRefreshWithNothingLeftAsksForANewLogin(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"expired"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	cfg.RefreshFunc = func(_ context.Context, _, _ string) (codexauth.Outcome, error) {
		return codexauth.Outcome{Kind: codexauth.Terminal, Code: "refresh_token_reused"}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	want := "{\"error\":{\"type\":\"ccdad_needs_relogin\",\"message\":\"ccdad: a@example.com needs a new login; run `ccdad codex add`\"}}"
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

// codex answers a 401 with six requests over six seconds. Those six are met
// from the refresher's own cooldown rather than from the token endpoint, which
// is why this answer is a 401 and not something that invites a longer retry.
func TestATransientRefreshWithNothingLeftIsTheBrandedRefusal(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"expired"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	cfg.RefreshFunc = func(_ context.Context, _, _ string) (codexauth.Outcome, error) {
		return codexauth.Outcome{Kind: codexauth.Transient, Code: "network"}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	want := `{"error":{"type":"ccdad_refresh_transient","message":"ccdad: token refresh failed temporarily; ccdad will retry"}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestTheRefresherIsAskedAtMostOncePerRequest(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"expired"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	var refreshes int32
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	cfg.RefreshFunc = func(_ context.Context, _, _ string) (codexauth.Outcome, error) {
		atomic.AddInt32(&refreshes, 1)
		return codexauth.Outcome{Kind: codexauth.Transient, Code: "network"}, nil
	}
	s := f.server(t, cfg)

	post(s, unpinnedSecret, nil, `{"input":[]}`)
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("the refresher was asked %d times for one request, want 1", got)
	}
}

// The upstream's own 401 about a REPLACEMENT account, after the search moved
// on from a rate-limited one. Every other 4xx from a replacement is answered
// with the original 429; a 401 is not, because it is the endpoint telling this
// user something about this account that a rate-limit body would hide.
func TestAPersistentUnauthorizedOnAReplacementIsPassedThrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_at":1790000000}}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"the replacement account is refused"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a", "uuid-b"} }
	// Rotated but the store is left alone, so the replay is refused again --
	// which is the case the pass-through rule is written for.
	cfg.RefreshFunc = func(context.Context, string, string) (codexauth.Outcome, error) {
		return codexauth.Outcome{Kind: codexauth.Rotated}, nil
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "thread-1"}, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the replacement's 401 passed through (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "the replacement account is refused") {
		t.Fatalf("body = %s, want the upstream's own", w.Body.String())
	}
}

// "The network is down" and "log in again" are different sentences, and the
// second one sends a user to re-authenticate an account that is fine.
//
// The seeded LimitBook and the pinned clock are the load-bearing part of this
// setup, not scenery. With an empty book the two paths through the tail agree
// by accident: the unreachable arm answers ccdad_unavailable, and so does
// outOfAccounts' own last arm, because with nothing waiting and nothing dead it
// falls through to exactly the same writer. Deleting the arm this test is
// written for would then change no assertion in it, which is the definition of
// a test that pins nothing. One candidate already in the book splits the two:
// the arm still answers ccdad_unavailable, while outOfAccounts now sees a
// candidate whose throttle has not lifted and answers usage_limit_reached with
// a resets_at -- so codex would sit on a quota clock waiting out a network
// outage, which is the regression worth catching. The clock is pinned because
// LimitedUntil expires a record on read, so a wall clock would make the seed
// stale on any machine whose date has passed the one written here.
func TestAnUnreachableUpstreamIsNotAMissingLogin(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	f.add("uuid-a", "a@example.com", "access-a")
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := closed.URL
	closed.Close()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	book := &LimitBook{}
	book.MarkLimitedFor("uuid-a", now.Add(time.Hour), true)
	cfg := f.config()
	cfg.Upstream = url
	cfg.Now = func() time.Time { return now }
	cfg.Book = book
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ccdad_unavailable") {
		t.Fatalf("body = %s, want the unavailable answer and not a relogin", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ccdad_needs_relogin") {
		t.Fatalf("an upstream nobody could reach was reported as an account needing a new login: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "usage_limit_reached") {
		t.Fatalf("an upstream nobody could reach was reported as a quota window to wait out: %s", w.Body.String())
	}
}

func TestWithNoRefresherAnUnauthorizedAnswerIsPassedThrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"expired"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	cfg := f.config()
	cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream's 401", w.Code)
	}
	if n := len(f.took()); n != 1 {
		t.Fatalf("the upstream saw %d requests, want 1 with no refresher wired", n)
	}
}

// The rule this whole proxy is organised around: codex answers a 429 with ONE
// request and a 500 with thirty over twenty-five seconds, so a fault inside
// ccdad must never reach it as a 5xx. Every row below induces a different
// internal failure.
//
// An upstream that itself answers 5xx is NOT in this table: passing an
// endpoint's own answer through is not ccdad failing.
func TestTheProxyNeverAnswersFiveHundred(t *testing.T) {
	allowed := map[int]bool{
		http.StatusOK:              true,
		http.StatusUnauthorized:    true,
		http.StatusNotFound:        true,
		http.StatusTooManyRequests: true,
	}

	ok := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	}

	cases := []struct {
		name string
		run  func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"everything works", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			return post(f.server(t, f.config()), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the account store is unreadable", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			f.accountsErr = errors.New("accounts.toml is unreadable")
			return post(f.server(t, f.config()), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the store holds no codex accounts", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			return post(f.server(t, f.config()), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the credential blob has no codex key", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			f.creds["uuid-a"] = cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"not-a-codex-token"}`)}
			return post(f.server(t, f.config()), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the credential read fails", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			cfg := f.config()
			cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
			cfg.Credentials = func(string) (cclink.Blob, error) { return nil, errors.New("the credential file is unreadable") }
			return post(f.server(t, cfg), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the upstream refuses the connection", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			closed := httptest.NewServer(http.HandlerFunc(ok))
			url := closed.URL
			closed.Close()
			cfg := f.config()
			cfg.Upstream = url
			cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
			return post(f.server(t, cfg), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the launch lookup fails", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			cfg := f.config()
			cfg.Lookup = func(string, string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
				return codexlaunch.Record{}, codexlaunch.Unknown, errors.New("the launches directory is unreadable")
			}
			return post(f.server(t, cfg), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"the refresher itself errors", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{}`)
			})
			f.add("uuid-a", "a@example.com", "access-a")
			cfg := f.config()
			cfg.RankedEligible = func() []string { return []string{"uuid-a"} }
			cfg.RefreshFunc = func(context.Context, string, string) (codexauth.Outcome, error) {
				return codexauth.Outcome{}, errors.New("the token endpoint could not be reached")
			}
			return post(f.server(t, cfg), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"a hook panics inside the handler", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			f.add("uuid-a", "a@example.com", "access-a")
			cfg := f.config()
			cfg.Now = func() time.Time { panic("the clock hook is broken") }
			return post(f.server(t, cfg), unpinnedSecret, nil, `{"input":[]}`)
		}},
		{"an unknown route", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			s := f.server(t, f.config())
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
			return w
		}},
		{"a GET to the responses route", func(t *testing.T) *httptest.ResponseRecorder {
			f := newFixture(t, ok)
			s := f.server(t, f.config())
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, ResponsesPath, nil))
			return w
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := c.run(t)
			if !allowed[w.Code] {
				t.Fatalf("status = %d, want one of 200, 401, 404 or 429 (%s)", w.Code, w.Body.String())
			}
		})
	}
}
