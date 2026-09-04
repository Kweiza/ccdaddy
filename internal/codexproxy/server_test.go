package codexproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Nothing in production sets Upstream, Client or Lookup: the daemon builds a
// Config out of a store root, a port, the refresher and the store hooks, and
// leaves those three at their zero values. The three nil-defaults in newServer
// are therefore the ONLY thing that points a real daemon at chatgpt.com, hands
// it a transport whose wait for the status line is bounded, and gives it any
// way at all to validate a launch bearer. Every other test in this package
// builds its server from the fixture, and the fixture sets all three, so
// without this one test the whole package stays green with the defaults
// deleted -- and the daemon that shipped would POST to the empty string
// through a nil *http.Client and check bearers with a nil func. The never-500
// guard would dress each of those panics up as ccdad_unavailable, so every
// codex request on the machine would 429 forever.
func TestAServerBuiltFromNothingButARootTakesTheDefaults(t *testing.T) {
	s, err := newServer(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("newServer() = %v, want nil", err)
	}
	if s.cfg.Upstream != DefaultUpstream {
		t.Errorf("Upstream = %q, want DefaultUpstream", s.cfg.Upstream)
	}
	if s.cfg.Client == nil {
		t.Error("Client is nil, want this package's own client; a forwarded turn would nil-deref")
	}
	if s.cfg.Lookup == nil {
		t.Error("Lookup is nil, want codexlaunch.Lookup; every bearer check would nil-deref")
	}
}

func TestHealthAnswersTheVersionAndThePortWithoutABearer(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Port = 24242
	s := f.server(t, cfg)

	r := httptest.NewRequest(http.MethodGet, HealthPath, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"ccdad":"test","port":24242}` {
		t.Fatalf("body = %s, want {\"ccdad\":\"test\",\"port\":24242}", got)
	}
}

func TestEverythingOtherThanTheTwoRoutesIsFourOhFour(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/responses"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodPost, "/ccdad/health"},
		{http.MethodGet, "/ccdad/anything"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, nil)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
			var doc struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("body is not the branded JSON: %v (%s)", err, w.Body.String())
			}
			if doc.Error.Type != "ccdad_not_found" {
				t.Fatalf("error type = %q, want ccdad_not_found", doc.Error.Type)
			}
		})
	}
}

func TestNewBindsLoopbackAndRecordsThePortItGot(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.PortSource = "derived"
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if s.Port() == 0 {
		t.Fatal("Port() = 0 after a successful bind")
	}
	// The port asked for here is 0 -- "any free one" -- so the first Listen
	// always succeeds and no fallback happened. Nothing else in this package
	// ever pins FellBack false, and the flag is easy to get wrong in a way
	// every other test tolerates: computing it from the OUTCOME
	// (fellBack = port != cfg.Port) rather than from the branch the bind was
	// actually taken in is true here, and true on every clean daemon start. A
	// daemon built that way logs the relaunch warning, and `ccdad status`
	// shows it, every single time it starts -- which trains a reader to ignore
	// the one signal that explains codex's endless reconnect spinner. This is
	// t.Error rather than t.Fatal so the health check below still runs.
	if s.FellBack() {
		t.Error("FellBack() = true after binding the port that was asked for")
	}
	res, err := http.Get("http://127.0.0.1:" + strconv.Itoa(s.Port()) + HealthPath)
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", res.StatusCode)
	}
}

func TestAnOccupiedAutomaticPortFallsBack(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	occupied := held.Addr().(*net.TCPAddr).Port

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Port, cfg.PortSource = occupied, "recorded"
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New() = %v, want a fallback rather than an error", err)
	}
	defer s.Close()
	if !s.FellBack() {
		t.Error("FellBack() = false after binding a port other than the one asked for")
	}
	if s.Port() == occupied || s.Port() == 0 {
		t.Fatalf("Port() = %d, want a different bound port", s.Port())
	}
}

func TestAnOccupiedConfiguredPortIsARefusal(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	occupied := held.Addr().(*net.TCPAddr).Port

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Port, cfg.PortSource = occupied, "config"
	s, err := New(cfg)
	if err == nil {
		s.Close()
		t.Fatal("New() = nil error on a configured port somebody else holds")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(occupied)) {
		t.Fatalf("the refusal does not name the port: %v", err)
	}
}

func TestServeReturnsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.PortSource = "derived"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil on a cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// Serve has TWO ways out and the daemon depends on both. The context one is
// above; this is the other, and it is the one a wrong implementation hangs on:
// the listener is taken away underneath a Serve whose context nobody is ever
// going to cancel, which is exactly what `Close()` on a bound-but-abandoned
// proxy does. A Serve that waited for the context here would leave the daemon's
// shutdown blocked on a channel nothing closes.
func TestServeReturnsWhenTheListenerIsClosedUnderIt(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.PortSource = "derived"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background()) }()
	if cerr := s.Close(); cerr != nil {
		t.Fatalf("Close() = %v, want nil", cerr)
	}
	// What is asserted is that Serve RETURNED. Whatever error the closed
	// listener reported is the operating system's business, not this test's.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its listener was closed under it")
	}
}

// A fault inside a handler must never reach codex as a 5xx: codex answers a 500
// with thirty requests over twenty-five seconds and a 429 with one.
func TestAFaultBeforeAnyBytesIsAFourTwentyNineAndNeverAFiveHundred(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(fmt.Errorf("a fault nobody planned for"))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ResponsesPath, nil))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	var doc struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not the branded JSON: %v (%s)", err, w.Body.String())
	}
	if doc.Error.Type != "ccdad_unavailable" {
		t.Fatalf("error type = %q, want ccdad_unavailable", doc.Error.Type)
	}
	for _, line := range f.logged() {
		if strings.Contains(line, "a fault nobody planned for") {
			return
		}
	}
	t.Fatalf("the fault was never logged: %v", f.logged())
}

// The other half of the never-500 rule, and the half nothing else in this
// package reaches: a fault that lands AFTER the first byte of the answer has
// already gone out. The status line is spent by then -- trackedWriter.WriteHeader
// returns early once wrote is true -- so writeUnavailable cannot replace it,
// and the branded JSON would simply be appended to the event stream, which
// would then end cleanly. codex reads a cleanly ended stream as a finished
// turn and keeps the half-answer it has. Breaking the connection is the only
// honest ending left, so what is pinned here is that NOTHING was appended: the
// bytes the client got are exactly the bytes the handler had streamed.
func TestAFaultAfterTheFirstByteBreaksTheConnectionRatherThanBrandingIt(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	s := f.server(t, f.config())

	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		panic(fmt.Errorf("a fault after the first byte"))
	}))
	w := httptest.NewRecorder()
	func() {
		defer func() {
			// A nil recover has to fail too, and this is why the comparison is
			// written against http.ErrAbortHandler rather than against nil:
			// with the already-written branch gone the handler returns
			// normally, and a check that only rejected a non-nil wrong value
			// would sail straight past the bug. t.Errorf rather than t.Fatalf
			// so the body assertion below -- the one that names the actual
			// harm -- still gets to speak.
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler", v)
			}
		}()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ResponsesPath, nil))
	}()

	if got := w.Body.String(); got != "data: one\n\n" {
		t.Fatalf("body = %q, want only what the handler had already streamed", got)
	}
}

// The upstream is FIXED -- DefaultUpstream, or whatever the daemon was built
// with -- and nothing the endpoint answers may move it. A 307 or 308 is
// exactly such a move: Go's default client follows up to ten hops, and for
// those two codes it replays the BODY. The body of a codex turn is the whole
// thread history including its encrypted reasoning, and it travels beside the
// workspace id and codex's own installation id in the turn metadata; a hop
// that stays on the upstream's own hostname carries the OAuth access token
// too. Following one would hand all of that to a host the configuration never
// named, and send() then reads harvestHeaders off whatever answered, so the
// redirect target could also have a quota reading committed for the account
// that paid. The Codex refresher guards the same way for the same reason.
//
// What is pinned here is the DEFAULT client, with Config.Client left nil,
// because nil is what the daemon hands in and defaultClient is therefore the
// only client production ever forwards through.
func TestTheDefaultClientNeverFollowsAnUpstreamRedirect(t *testing.T) {
	var elsewhereHits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", elsewhere.URL+"/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	cfg := f.config()
	cfg.Client = nil
	a := f.add("uuid-a", "a@example.com", "access-a")
	f.serving(t, a.UUID)
	s := f.server(t, cfg)

	w := post(s, unpinnedSecret, nil, `{"input":["secret prompt text"]}`)

	if n := elsewhereHits.Load(); n != 0 {
		t.Fatalf("the redirect target was fetched %d time(s); the upstream is fixed and a 3xx may not move it", n)
	}
	if got := len(f.took()); got != 1 {
		t.Fatalf("the fixed upstream saw %d requests, want exactly 1", got)
	}
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 handed back to codex unfollowed", w.Code)
	}
}

// The never-500 guarantee has to survive a fault raised by the delegate's own
// WriteHeader, and there is a reachable one: net/http's response READER accepts
// any three-digit status, including 000-099, while its WRITER panics in
// checkWriteHeaderCode on anything below 100. writeBack hands the upstream's
// status straight through, so an upstream status line of `HTTP/1.1 000 Nothing`
// becomes WriteHeader(0) on a real connection and panics inside it.
//
// The guard can only answer while the status line is unspent, so trackedWriter
// must not call it spent until the delegate has actually written it. If it
// marks it first, this recovers into panic(http.ErrAbortHandler) and codex gets
// a closed connection with no status line at all -- a bare hang-up, which is
// the outcome the whole never-500 rule exists to avoid. A real listener and a
// real client rather than a recorder, because the harm being pinned is what
// arrives on the wire.
func TestAnUpstreamStatusTooLowToWriteIsStillAnsweredNotHungUpOn(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	cfg := f.config()
	cfg.Upstream = rawStatusLineUpstream(t, "HTTP/1.1 000 Nothing")
	a := f.add("uuid-a", "a@example.com", "access-a")
	f.serving(t, a.UUID)
	s := f.server(t, cfg)

	proxy := httptest.NewServer(s.Handler())
	defer proxy.Close()

	req, err := http.NewRequest(http.MethodPost, proxy.URL+ResponsesPath, strings.NewReader(`{"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	res, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatalf("the client got no status line at all: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body is not the branded JSON: %v (%s)", err, body)
	}
	if doc.Error.Type != "ccdad_unavailable" {
		t.Fatalf("error type = %q, want ccdad_unavailable", doc.Error.Type)
	}
}

// rawStatusLineUpstream serves one hand-written status line and nothing else.
// It is a bare listener rather than an httptest server because the whole point
// is to answer with a status line net/http's writer would refuse to produce.
func rawStatusLineUpstream(t *testing.T, statusLine string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = io.WriteString(c, statusLine+"\r\nContent-Length: 0\r\n\r\n")
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + ResponsesPath
}
