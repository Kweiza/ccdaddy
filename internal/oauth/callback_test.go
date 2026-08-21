package oauth

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpGetClient is bounded on purpose. A wedged handler is exactly the failure
// the delivery guard exists to prevent, and an unbounded client would turn that
// into a hung test binary instead of a readable failure.
//
// It is not named testClient: token_test.go already has one.
var httpGetClient = &http.Client{Timeout: 5 * time.Second}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := httpGetClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func callbackURL(l *Listener, query string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback?%s", l.Port(), query)
}

func TestListenBindsAnEphemeralLoopbackPort(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	defer l.Close()

	if l.Port() <= 0 {
		t.Fatalf("Port() = %d, want a bound port", l.Port())
	}
}

// The callback must be reachable only from this machine. Binding "" or
// "0.0.0.0" would expose a code-accepting endpoint to the network.
func TestListenBindsLoopbackOnly(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	addr, ok := l.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("Addr() = %T, want *net.TCPAddr", l.ln.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("bound IP = %s, want a loopback address", addr.IP)
	}

	// And prove it from the outside: no non-loopback interface answers.
	for _, ip := range interfaceIPv4s(t) {
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(ip.String(), fmt.Sprint(l.Port())), 2*time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("the callback answered on %s, want loopback only", ip)
		}
	}
}

func interfaceIPv4s(t *testing.T) []net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	var out []net.IP
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() {
			continue
		}
		if v4 := n.IP.To4(); v4 != nil {
			out = append(out, v4)
		}
	}
	if len(out) == 0 {
		t.Skip("no non-loopback IPv4 interface to probe")
	}
	return out
}

// An empty state disables CSRF validation silently: every callback that omits
// state would then match. Refuse at the door rather than serve unguarded.
func TestListenRejectsAnEmptyState(t *testing.T) {
	l, err := Listen("")
	if err == nil {
		l.Close()
		t.Fatal(`Listen("") = nil error, want a refusal`)
	}
	if l != nil {
		t.Fatalf(`Listen("") = %v listener, want nil`, l)
	}
}

func TestCallbackDeliversCodeOnMatchingState(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	status, body := get(t, callbackURL(l, "code=THE-CODE&state=STATE"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Fatalf("body is not the success page: %q", body)
	}
	if !strings.Contains(body, "You're logged in") {
		t.Fatalf("body is not the success page: %q", body)
	}

	select {
	case got := <-l.Results():
		if got.Err != nil {
			t.Fatalf("Err = %v, want nil", got.Err)
		}
		if got.Code != "THE-CODE" {
			t.Fatalf("Code = %q, want THE-CODE", got.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
}

// A state mismatch is a security stop, not a retry.
func TestCallbackRejectsStateMismatch(t *testing.T) {
	l, err := Listen("EXPECTED")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	status, body := get(t, callbackURL(l, "code=C&state=WRONG"))
	if !strings.Contains(body, "Login blocked") {
		t.Fatalf("body is not the blocked page: %q", body)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	select {
	case got := <-l.Results():
		if got.Err == nil {
			t.Fatal("Err = nil, want a state-mismatch error")
		}
		if got.Code != "" {
			t.Fatalf("Code = %q, want empty on a state mismatch", got.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
}

// A favicon probe or a stray local request must be answered and ignored, not
// treated as a failed login.
func TestCallbackIgnoresUnrelatedPaths(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	status, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/favicon.ico", l.Port()))
	if !strings.Contains(body, "Nothing at this address") {
		t.Fatalf("body is not the not-found page: %q", body)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}

	select {
	case got := <-l.Results():
		t.Fatalf("an unrelated request produced a result: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCallbackWithoutCodeIsIgnored(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	status, body := get(t, callbackURL(l, "state=STATE"))
	if !strings.Contains(body, "No code in the callback") {
		t.Fatalf("body is not the no-code page: %q", body)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	select {
	case got := <-l.Results():
		t.Fatalf("a codeless callback produced a result: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCallbackOAuthErrorDelivers(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	_, body := get(t, callbackURL(l, "error=access_denied&state=STATE"))
	if !strings.Contains(body, "Login canceled") {
		t.Fatalf("body is not the declined page: %q", body)
	}

	select {
	case got := <-l.Results():
		if got.Err == nil {
			t.Fatal("Err = nil, want a rejection")
		}
		if !strings.Contains(got.Err.Error(), "declined") {
			t.Fatalf("Err = %q, want it to describe a declined authorization", got.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
}

// Any page open in the browser during a login can issue a cross-origin GET at
// the loopback port. An error callback that does not carry our state is not
// ours, and must not be able to end the login.
func TestCallbackErrorWithoutOurStateDoesNotEndTheLogin(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"wrong state", "error=access_denied&state=WRONG"},
		{"no state at all", "error=access_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := Listen("EXPECTED")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()

			status, body := get(t, callbackURL(l, tc.query))
			if !strings.Contains(body, "Login blocked") {
				t.Fatalf("body is not the blocked page: %q", body)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}

			select {
			case got := <-l.Results():
				t.Fatalf("a forged error callback ended the login: %+v", got)
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// error_description is uncapped upstream text. It must not reach the page, the
// error, or a log line.
func TestCallbackNeverReflectsUpstreamErrorText(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	secret := "UPSTREAM-TEXT-THAT-MUST-NOT-ESCAPE"
	_, body := get(t, callbackURL(l,
		"error=server_error&error_description="+secret+"&state=STATE"))

	if strings.Contains(body, secret) {
		t.Fatal("the callback page reflected error_description")
	}
	select {
	case got := <-l.Results():
		if got.Err == nil {
			t.Fatal("Err = nil, want a rejection")
		}
		if strings.Contains(got.Err.Error(), secret) {
			t.Fatalf("the delivered error leaked error_description: %q", got.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
}

// An error value outside RFC 6749's set is where the bytes are least
// trustworthy, so none of them survive the boundary.
func TestCallbackWithholdsUnrecognizedErrorCode(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const upstream = "not_a_real_code"
	_, body := get(t, callbackURL(l, "error="+upstream+"&state=STATE"))
	if strings.Contains(body, upstream) {
		t.Fatal("the callback page reflected the upstream error code")
	}
	select {
	case got := <-l.Results():
		if got.Err == nil {
			t.Fatal("Err = nil, want a rejection")
		}
		if strings.Contains(got.Err.Error(), upstream) {
			t.Fatalf("the delivered error leaked the upstream error code: %q", got.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
}

// Parsing into a closed set is only worth doing if the parsed value survives to
// the caller: that is what lets a login log WHY it failed without ever touching
// the upstream bytes.
func TestCallbackDeliversTheParsedRejection(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  AuthorizeRejection
	}{
		{"error=access_denied&state=STATE", RejectionDeclined},
		{"error=temporarily_unavailable&state=STATE", RejectionUpstream},
		{"error=invalid_scope&state=STATE", RejectionRefused},
		{"error=not_a_real_code&state=STATE", RejectionUnrecognized},
	} {
		t.Run(tc.query, func(t *testing.T) {
			l, err := Listen("STATE")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()

			get(t, callbackURL(l, tc.query))

			select {
			case got := <-l.Results():
				var rejErr *RejectionError
				if !errors.As(got.Err, &rejErr) {
					t.Fatalf("Err = %T(%v), want a *RejectionError", got.Err, got.Err)
				}
				if rejErr.Rejection != tc.want {
					t.Fatalf("Rejection = %v, want %v", rejErr.Rejection, tc.want)
				}
				if rejErr.LogDetail() != tc.want.LogDetail() {
					t.Fatalf("LogDetail() = %q, want %q", rejErr.LogDetail(), tc.want.LogDetail())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no result delivered")
			}
		})
	}
}

func TestParseRejectionMapsToClosedSet(t *testing.T) {
	cases := map[string]AuthorizeRejection{
		"access_denied":             RejectionDeclined,
		"server_error":              RejectionUpstream,
		"temporarily_unavailable":   RejectionUpstream,
		"invalid_request":           RejectionRefused,
		"unauthorized_client":       RejectionRefused,
		"unsupported_response_type": RejectionRefused,
		"invalid_scope":             RejectionRefused,
		"something_invented":        RejectionUnrecognized,
	}
	for in, want := range cases {
		if got := parseRejection(in); got != want {
			t.Errorf("parseRejection(%q) = %v, want %v", in, got, want)
		}
	}
	// Every arm's text is one of our own literals, so the closed-set property is
	// what there is to check here. The claim that upstream bytes never survive
	// is carried end-to-end by TestCallbackWithholdsUnrecognizedErrorCode; a
	// substring assertion on LogDetail() could not fail for any implementation.
	for _, r := range []AuthorizeRejection{RejectionDeclined, RejectionUpstream, RejectionRefused, RejectionUnrecognized} {
		if r.LogDetail() == "" || r.UserMessage() == "" {
			t.Errorf("rejection %v has an empty message", r)
		}
	}
}

// A browser can fire the callback more than once (a reload, a retry, a
// preconnect). Only the first delivery is admitted, and every later request is
// still answered instead of wedging the handler on a full channel.
func TestCallbackDeliversAtMostOneResult(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	url := callbackURL(l, "code=THE-CODE&state=STATE")
	for i := range 3 {
		res, err := httpGetClient.Get(url)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		res.Body.Close()
	}

	select {
	case got := <-l.Results():
		if got.Code != "THE-CODE" {
			t.Fatalf("Code = %q, want THE-CODE", got.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}
	select {
	case got := <-l.Results():
		t.Fatalf("a second result was delivered: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("first Close() = %v, want nil", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

// Close races the browser by construction: the tab can still be mid-request.
func TestCloseDuringInFlightRequest(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}

	url := callbackURL(l, "code=THE-CODE&state=STATE")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := httpGetClient.Get(url)
			if err == nil {
				res.Body.Close()
			}
		}()
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close() during an in-flight request = %v, want nil", err)
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}

	// At most one result, and reading it never blocks forever.
	select {
	case <-l.Results():
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case got := <-l.Results():
		t.Fatalf("a second result was delivered: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// The address bar holds the authorization code while this page is open.
func TestCallbackPageCarriesHardeningHeaders(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	res, err := httpGetClient.Get(callbackURL(l, "code=THE-CODE&state=STATE"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	for _, h := range []struct{ key, want string }{
		{"Content-Type", "text/html; charset=utf-8"},
		{"Cache-Control", "no-store"},
		{"Referrer-Policy", "no-referrer"},
		{"X-Content-Type-Options", "nosniff"},
	} {
		if got := res.Header.Get(h.key); got != h.want {
			t.Errorf("header %s = %q, want %q", h.key, got, h.want)
		}
	}
}

// The CLI owns stderr during a login — it is carrying the authorize URL and the
// paste prompt. net/http's default logger would dump handler panics and
// connection noise straight over that.
func TestListenerNeverLogsToStderr(t *testing.T) {
	l, err := Listen("STATE")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if l.srv.ErrorLog == nil {
		t.Fatal("srv.ErrorLog = nil, so net/http logs to the standard logger, i.e. stderr")
	}
	if w := l.srv.ErrorLog.Writer(); w != io.Discard {
		t.Fatalf("srv.ErrorLog writes to %#v, want io.Discard", w)
	}
}
