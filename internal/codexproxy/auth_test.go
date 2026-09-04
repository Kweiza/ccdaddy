package codexproxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
)

// refusingBody fails on the first read and remembers that it was read at all.
// It is how a test proves the bearer was checked BEFORE the body: an
// unauthenticated request that buffers 100 MiB before refusing is a way for
// anything on the machine to exhaust the daemon.
type refusingBody struct {
	mu   sync.Mutex
	read bool
}

func (b *refusingBody) Read([]byte) (int, error) {
	b.mu.Lock()
	b.read = true
	b.mu.Unlock()
	return 0, errors.New("the proxy read a body it had no business reading")
}

func (b *refusingBody) wasRead() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}

func TestABadBearerIsRefusedWithoutReadingTheBody(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	body := &refusingBody{}
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, body)
	r.Header.Set("Authorization", "Bearer nobody-issued-this")
	// A caller announcing 100 MiB. Nothing may be buffered on its word.
	r.ContentLength = 100 << 20
	r.Header.Set("Content-Length", strconv.Itoa(100<<20))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if body.wasRead() {
		t.Fatal("the body was read for a request whose bearer was refused")
	}
	want := `{"error":{"type":"ccdad_unknown_launch","message":"ccdad: this codex was not launched by the running ccdad daemon (or its launch record is gone). Quit codex and start it again; ` + "`ccdad which`" + ` shows the account it will use."}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestADeadLaunchIsRefusedLikeAnUnknownOne(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Lookup = func(_, _ string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
		return codexlaunch.Record{}, codexlaunch.Dead, nil
	}
	s := f.server(t, cfg)

	w := post(s, "a-reaped-secret", nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ccdad_unknown_launch") {
		t.Fatalf("body = %s, want the unknown-launch answer", w.Body.String())
	}
}

func TestALookupFailureIsARefusalAndNotAFault(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Lookup = func(_, _ string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
		return codexlaunch.Record{}, codexlaunch.Unknown, errors.New("the launch directory is unreadable")
	}
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAValidBearerGetsPastTheGateAndItsBodyIsRead(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	w := post(s, unpinnedSecret, nil, `{"input":["hello"]}`)
	// What is asserted here is the GATE, not the answer: forwarding lands two
	// tasks later, and until then an authenticated request falls through to the
	// proxy's own unavailable answer.
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("a valid launch bearer was refused: %s", w.Body.String())
	}
}

func TestTheBearerIsCheckedAtMostOncePerAuthWindow(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	var (
		mu    sync.Mutex
		calls int
	)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cfg := f.config()
	cfg.Now = func() time.Time { return now }
	cfg.Lookup = func(_, _ string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return codexlaunch.Record{}, codexlaunch.Valid, nil
	}
	s := f.server(t, cfg)

	post(s, unpinnedSecret, nil, `{}`)
	post(s, unpinnedSecret, nil, `{}`)
	mu.Lock()
	within := calls
	mu.Unlock()
	if within != 1 {
		t.Fatalf("the launch record was checked %d times inside one window, want 1", within)
	}

	now = now.Add(authTTL + time.Second)
	post(s, unpinnedSecret, nil, `{}`)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != 2 {
		t.Fatalf("the launch record was checked %d times in total, want 2 once the window passed", after)
	}
}

func TestADeadLaunchIsEvictedFromTheCache(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	var (
		mu     sync.Mutex
		result = codexlaunch.Valid
		calls  int
	)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cfg := f.config()
	cfg.Now = func() time.Time { return now }
	cfg.Lookup = func(_, _ string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return codexlaunch.Record{}, result, nil
	}
	s := f.server(t, cfg)

	post(s, unpinnedSecret, nil, `{}`)
	now = now.Add(authTTL + time.Second)
	mu.Lock()
	result = codexlaunch.Dead
	mu.Unlock()
	if w := post(s, unpinnedSecret, nil, `{}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d after the launch died, want 401", w.Code)
	}
	// A reaped launch must leave nothing behind in the hit cache, and the map
	// is the only place that fact can be read from. authorize reaches its
	// eviction only on the cache-MISS path — a hit that is still inside the
	// window returns before the lookup is ever called — so by the time a Dead
	// answer comes back the entry is already absent or already stale, and the
	// request after it is refused whether or not anything was deleted. That is
	// measured and not assumed: with both forgetHit calls removed, every status
	// assertion in this file stays green, so a second post here would have
	// asserted nothing the first one did not.
	s.mu.Lock()
	cached := len(s.auth)
	s.mu.Unlock()
	if cached != 0 {
		t.Fatalf("the reaped launch is still cached: %d entries, want 0", cached)
	}
}

func TestTooManyUnauthenticatedRequestsAreRefusedWithoutTouchingTheFilesystem(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	entered := make(chan struct{}, MaxUnauthenticated)
	release := make(chan struct{})
	cfg := f.config()
	cfg.Lookup = func(_, _ string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
		entered <- struct{}{}
		<-release
		return codexlaunch.Record{}, codexlaunch.Valid, nil
	}
	s := f.server(t, cfg)

	var wg sync.WaitGroup
	for i := 0; i < MaxUnauthenticated; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			post(s, "secret-"+strconv.Itoa(i), nil, `{}`)
		}(i)
	}
	for i := 0; i < MaxUnauthenticated; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("the checks never all reached the lookup")
		}
	}

	w := post(s, "one-too-many", nil, `{}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d with the gate full, want 401", w.Code)
	}
	close(release)
	wg.Wait()
}

func TestABodyOverTheCapIsRefusedRatherThanBuffered(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, io.LimitReader(zeroes{}, MaxBody+1))
	if _, ok := readBody(r); ok {
		t.Fatal("readBody accepted a body over the cap")
	}
	r = httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader("small"))
	body, ok := readBody(r)
	if !ok || string(body) != "small" {
		t.Fatalf("readBody() = (%q, %v), want (\"small\", true)", body, ok)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }

func TestBearerOfReadsOnlyABearerHeader(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER  abc": "abc",
		"Basic abc":   "",
		"abc":         "",
		"":            "",
		"Bearer ":     "",
	}
	for header, want := range cases {
		if got := bearerOf(header); got != want {
			t.Errorf("bearerOf(%q) = %q, want %q", header, got, want)
		}
	}
}
