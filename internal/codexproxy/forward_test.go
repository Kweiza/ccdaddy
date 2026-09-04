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

// The full strip list, spelled here rather than derived from the
// implementation, so a header quietly removed from the map fails this test.
var strippedByName = []string{
	"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Host",
	"Forwarded", "Via", "Te", "Trailer", "Transfer-Encoding", "Connection",
	"Upgrade", "Expect", "Content-Length",
	"Proxy-Connection", "Proxy-Anything",
	"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host",
}

// cannotTravel are the strip-list names that could not reach the upstream even
// if this proxy forwarded the caller's header untouched, so asserting on them
// on the forwarding path would prove nothing. Host, Content-Length,
// Transfer-Encoding and Trailer are in net/http's own reqWriteExcludeHeader
// (net/http/request.go), so the client drops them out of req.Header whatever
// this proxy does. Authorization is rebuilt by send from the store on every
// attempt, so a caller's own can never survive; putting one on the way in only
// fails the launch check before any of this runs. Expect is left out for a
// third reason: an Expect the server does not understand makes the fake
// upstream's own net/http answer 417 before the recording handler is reached,
// which would fail the test with a status code instead of naming the header
// that leaked.
var cannotTravel = map[string]bool{
	"Authorization":     true,
	"Host":              true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Trailer":           true,
	"Expect":            true,
}

func TestOutboundHeadersDropExactlyTheStripList(t *testing.T) {
	in := http.Header{}
	for _, name := range strippedByName {
		in.Set(name, "dropped")
	}
	kept := map[string]string{
		"Session-Id":            "session-1",
		"Thread-Id":             "thread-1",
		"Originator":            "codex_cli_rs",
		"X-Codex-Turn-Metadata": `{"turn_id":"t-1","installation_id":"i-1"}`,
		"Content-Type":          "application/json",
		"Accept":                "text/event-stream",
		"Openai-Beta":           "responses=experimental",
		"X-Codex-Turn-State":    "ts-1",
	}
	for name, value := range kept {
		in.Set(name, value)
	}

	out := outboundHeader(in)

	for _, name := range strippedByName {
		if got := out.Get(name); got != "" {
			t.Errorf("%s survived the strip with value %q", name, got)
		}
	}
	for name, value := range kept {
		if got := out.Get(name); got != value {
			t.Errorf("%s = %q, want %q — ccdad rewrites nothing it does not have to", name, got, value)
		}
	}
}

// The strip list is a property of the FORWARDING PATH, not of the pure function
// that computes it. An implementation that never calls outboundHeader, or calls
// it and then re-copies the caller's header over the result, still satisfies
// TestOutboundHeadersDropExactlyTheStripList -- measured: replacing send's
// `req.Header = outboundHeader(in.Header)` with `req.Header = in.Header.Clone()`
// leaves every other test in this file green while thirteen of these names
// arrive at the upstream carrying ccdad's own bearer. So this test smuggles
// each one through a real POST and looks at what the upstream actually got.
func TestSmuggledCredentialAndHopHeadersNeverReachTheUpstream(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())

	var smuggled []string
	headers := map[string]string{}
	for _, name := range strippedByName {
		if cannotTravel[name] {
			continue
		}
		smuggled = append(smuggled, name)
		headers[name] = "smuggled-" + name
	}
	w := post(s, unpinnedSecret, headers, `{"input":[]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	took := f.took()
	if len(took) != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", len(took))
	}
	for _, name := range smuggled {
		if got := took[0].header.Get(name); got != "" {
			t.Errorf("%s reached chatgpt.com as %q, on a request carrying ccdad's own bearer", name, got)
		}
	}
}

func TestTheUpstreamGetsCcdadsBearerAndTheTurnMetadataVerbatim(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())

	metadata := `{"turn_id":"t-1","installation_id":"i-1"}`
	body := `{"input":["hello"],"store":false}`
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	r.Header.Set("X-Codex-Turn-Metadata", metadata)
	// A caller's own length, which must not survive: the proxy rebuilds it.
	r.Header.Set("Content-Length", "999")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	took := f.took()
	if len(took) != 1 {
		t.Fatalf("the upstream saw %d requests, want 1", len(took))
	}
	got := took[0]
	if want := "Bearer access-a"; got.header.Get("Authorization") != want {
		t.Errorf("upstream Authorization = %q, want %q", got.header.Get("Authorization"), want)
	}
	if want := "workspace-uuid-a"; got.header.Get(accountIDHeader) != want {
		t.Errorf("upstream %s = %q, want %q", accountIDHeader, got.header.Get(accountIDHeader), want)
	}
	if got.header.Get("X-Codex-Turn-Metadata") != metadata {
		t.Errorf("the turn metadata was rewritten: %q", got.header.Get("X-Codex-Turn-Metadata"))
	}
	if string(got.body) != body {
		t.Errorf("upstream body = %q, want %q", got.body, body)
	}
	if got.length != int64(len(body)) {
		t.Errorf("upstream Content-Length = %d, want %d", got.length, len(body))
	}
	if h := got.header.Get("Content-Length"); h != "" && h != strconv.Itoa(len(body)) {
		t.Errorf("upstream Content-Length header = %q, want the recomputed %d", h, len(body))
	}
}

func TestUpstreamCookiesAndEdgeHeadersNeverReachTheClient(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=abc")
		w.Header().Set("Cf-Ray", "abc-LHR")
		w.Header().Set("Cf-Cache-Status", "DYNAMIC")
		w.Header().Set("X-Codex-Primary-Used-Percent", "42")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`{"type":"response.completed"}`))
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, name := range []string{"Set-Cookie", "Cf-Ray", "Cf-Cache-Status"} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s reached the client with value %q", name, got)
		}
	}
	if got := w.Header().Get("X-Codex-Primary-Used-Percent"); got != "42" {
		t.Errorf("the codex rate-limit header was dropped: %q", got)
	}
}

func TestTheAnswerReachesTheClientBeforeTheUpstreamHasFinished(t *testing.T) {
	gate := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-gate
		io.WriteString(w, "data: two\n\n")
		w.(http.Flusher).Flush()
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+ResponsesPath, strings.NewReader(`{"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	res, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	first := make([]byte, len("data: one\n\n"))
	readFirst := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(res.Body, first)
		readFirst <- err
	}()
	select {
	case err := <-readFirst:
		if err != nil {
			t.Fatalf("reading the first event: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("the first event never arrived; the answer is being buffered rather than streamed")
	}
	if string(first) != "data: one\n\n" {
		t.Fatalf("first event = %q", first)
	}
	close(gate)
	rest, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("rest = %q, want the second event", rest)
	}
}

func TestAnUpstreamStreamThatBreaksMidwayBreaksTheClientStream(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		// The upstream connection dies mid-turn.
		panic(http.ErrAbortHandler)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+ResponsesPath, strings.NewReader(`{"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	res, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the upstream's 200", res.StatusCode)
	}
	// A cleanly terminated body would tell codex the turn finished. It did not.
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("the client read a complete body from a stream that broke mid-turn")
	}
}

func TestANonSuccessAnswerIsPassedThroughWithItsBody(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// The same three headers the streaming test sets, because the buffered
		// answer has its OWN header copy and nothing else in this package
		// exercises it: deleting writeBack's copyResponseHeader call leaves
		// every other test here green -- measured. What survives such a
		// writeBack is the Content-Length it sets itself, and a Content-Type
		// net/http sniffs off the body, so a JSON error would reach codex
		// labelled text/plain with the upstream's own headers gone.
		w.Header().Set("Set-Cookie", "session=abc")
		w.Header().Set("Cf-Ray", "abc-LHR")
		w.Header().Set("X-Codex-Primary-Used-Percent", "42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"no"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())

	w := post(s, unpinnedSecret, nil, `{"input":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream's 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Fatalf("body = %s, want the upstream's own", w.Body.String())
	}
	for _, name := range []string{"Set-Cookie", "Cf-Ray"} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s reached the client with value %q", name, got)
		}
	}
	if got := w.Header().Get("X-Codex-Primary-Used-Percent"); got != "42" {
		t.Errorf("the codex rate-limit header was dropped: %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want the upstream's application/json", got)
	}
}

func TestTheLogNeverCarriesABearerOrABody(t *testing.T) {
	metadata := map[string]string{"X-Codex-Turn-Metadata": `{"turn_id":"a-metadata-marker"}`}

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"a-body-marker"}}`)
	})
	f.add("uuid-a", "a@example.com", "access-a")
	s := f.server(t, f.config())

	post(s, unpinnedSecret, metadata, `{"input":["a-request-marker"]}`)
	post(s, "a-refused-secret", nil, `{"input":["a-request-marker"]}`)

	// Neither post above logs a single line -- measured: an upstream 400 is
	// passed through silently and a refused bearer is answered without a word.
	// A scan over an empty log passes no matter what this file writes, so the
	// two failure paths that DO log are driven on purpose below.
	//
	// The first is the transport failure. Closing the upstream makes send's
	// Do fail, which is the "could not reach the upstream" line. Close is
	// idempotent and newFixture already registered it as a cleanup, so calling
	// it here is safe.
	f.upstream.Close()
	post(s, unpinnedSecret, metadata, `{"input":["a-request-marker"]}`)

	// The second is a stream that dies after the status is already on the
	// wire, which is the "ended early" line -- the one most likely to grow an
	// access token, since streamBack has the attempt in hand. It needs its own
	// upstream and a real listener in front of the proxy: streamBack answers a
	// broken stream by panicking with http.ErrAbortHandler, and a recorder
	// would carry that panic into the test rather than into a connection.
	broken := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	broken.add("uuid-a", "a@example.com", "access-a")
	front := httptest.NewServer(broken.server(t, broken.config()).Handler())
	defer front.Close()
	req, err := http.NewRequest(http.MethodPost, front.URL+ResponsesPath, strings.NewReader(`{"input":["a-request-marker"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+unpinnedSecret)
	for name, value := range metadata {
		req.Header.Set(name, value)
	}
	if res, err := front.Client().Do(req); err == nil {
		// The read is what waits for the break, and it is expected to fail.
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}

	lines := append(f.logged(), broken.logged()...)
	// Naming the two lines, rather than counting them, is what keeps this test
	// from going quietly vacuous a second time: a later task adding a log
	// statement elsewhere in the request path would satisfy a bare "something
	// was logged" check while both of these had moved out of reach.
	for _, want := range []string{"could not reach the upstream", "ended early"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("nothing matching %q was logged, so the scan below proves nothing about that path: %q", want, lines)
		}
	}

	forbidden := []string{
		unpinnedSecret,      // the launch bearer
		"a-refused-secret",  // a refused launch bearer
		"access-a",          // the upstream Authorization
		"refresh-uuid-a",    // the refresh token
		"a-request-marker",  // the request body
		"a-body-marker",     // the response body
		"a-metadata-marker", // the turn metadata
	}
	for _, line := range lines {
		for _, secret := range forbidden {
			if strings.Contains(line, secret) {
				t.Errorf("the log carries %q: %s", secret, line)
			}
		}
	}
}
