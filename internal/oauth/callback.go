package oauth

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// CallbackResult is one delivery from the loopback listener. Exactly one of
// Code and Err is set.
type CallbackResult struct {
	Code string
	Err  error
}

// AuthorizeRejection is the reason an authorize callback came back without a
// code, parsed into RFC 6749 §4.1.2.1's closed set.
//
// The callback's error and error_description parameters are uncapped upstream
// text. Parsing into a closed set at the boundary and discarding the bytes is
// what keeps them out of stderr, out of the log, and off the served page.
type AuthorizeRejection int

const (
	// RejectionDeclined is access_denied — the operator said no. The one arm
	// that is a choice rather than a failure.
	RejectionDeclined AuthorizeRejection = iota
	// RejectionUpstream is Anthropic's side; a retry can clear it.
	RejectionUpstream
	// RejectionRefused means the authorize REQUEST was refused, so a retry
	// sends the same thing again and fails the same way.
	RejectionRefused
	// RejectionUnrecognized is an error value outside RFC 6749's set — the case
	// where the bytes are least trustworthy, so none are kept.
	RejectionUnrecognized
)

func parseRejection(code string) AuthorizeRejection {
	switch code {
	case "access_denied":
		return RejectionDeclined
	case "server_error", "temporarily_unavailable":
		return RejectionUpstream
	case "invalid_request", "unauthorized_client", "unsupported_response_type", "invalid_scope":
		return RejectionRefused
	default:
		return RejectionUnrecognized
	}
}

// UserMessage is the canned, upstream-free description.
func (r AuthorizeRejection) UserMessage() string {
	switch r {
	case RejectionDeclined:
		return "you declined the authorization request"
	case RejectionUpstream:
		return "Anthropic is having trouble; try again shortly"
	default:
		return "Anthropic refused the login"
	}
}

// LogDetail is the OAuth error code for an operator log — always one of our
// own literals, never the browser's bytes.
func (r AuthorizeRejection) LogDetail() string {
	switch r {
	case RejectionDeclined:
		return "access_denied"
	case RejectionUpstream:
		return "server_error or temporarily_unavailable"
	case RejectionRefused:
		return "invalid_request, unauthorized_client, unsupported_response_type, or invalid_scope"
	default:
		return "an error code outside RFC 6749's set (withheld)"
	}
}

// RejectionError is the error a rejected authorize callback delivers.
//
// Parsing into a closed set is only worth doing if the parsed value survives to
// the caller: this is what lets a login report WHY it failed without any
// upstream byte ever reaching a message. Error() is the canned user text;
// LogDetail() is the OAuth error code for an operator log.
type RejectionError struct{ Rejection AuthorizeRejection }

func (e *RejectionError) Error() string     { return e.Rejection.UserMessage() }
func (e *RejectionError) LogDetail() string { return e.Rejection.LogDetail() }

// Listener is the loopback HTTP server that catches the browser redirect.
type Listener struct {
	ln        net.Listener
	srv       *http.Server
	results   chan CallbackResult
	state     string
	closeOnce sync.Once
	sendOnce  sync.Once
}

// Listen binds an ephemeral loopback port and starts serving.
//
// Binding to 127.0.0.1 specifically, not 0.0.0.0, keeps the callback
// unreachable from the network; only this machine can deliver a code.
//
// The redirect URI advertises "localhost" to match Claude Code, so a resolver
// that prefers IPv6 tries [::1] first and falls back to 127.0.0.1 once that is
// refused. If IPv6-only loopback ever needs supporting, bind a second listener
// on [::1] — never widen this one to every interface.
func Listen(expectedState string) (*Listener, error) {
	if expectedState == "" {
		// With no state to compare against, every callback that simply omits
		// state matches, and CSRF validation is off with nothing to show for it.
		return nil, errors.New("the callback listener needs a state value to validate against")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("binding the loopback callback listener: %w", err)
	}

	l := &Listener{
		ln:      ln,
		results: make(chan CallbackResult, 1),
		state:   expectedState,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", l.handle)
	l.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// The CLI owns stderr during a login — it is carrying the authorize URL
		// and the paste prompt. net/http must not write over that.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// Port is the bound port, for building the loopback redirect URI.
func (l *Listener) Port() int { return l.ln.Addr().(*net.TCPAddr).Port }

// Results delivers at most one CallbackResult.
func (l *Listener) Results() <-chan CallbackResult { return l.results }

// deliver sends at most one result. A browser can fire more than one request at
// a loopback port (preconnect, a retry, a reload), so the send is guarded:
// results is buffered for exactly one, and the caller reads it exactly once, so
// an unguarded second send would wedge its handler goroutine forever.
func (l *Listener) deliver(r CallbackResult) {
	l.sendOnce.Do(func() { l.results <- r })
}

func (l *Listener) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		// A favicon probe or a stray local request. Answer it and keep waiting;
		// it is not a failed login.
		writePage(w, http.StatusNotFound, pageNotFound)
		return
	}
	q := r.URL.Query()

	if errCode := q.Get("error"); errCode != "" {
		// An error callback that does not carry our state is not ours. Any page
		// open in the browser during a login can spray a forged one across the
		// ephemeral range; answer it and keep waiting rather than let it end the
		// login. RFC 6749 §4.1.2.1 has the real provider echo state here, so
		// requiring it costs nothing.
		if q.Get("state") != l.state {
			writePage(w, http.StatusBadRequest, pageBlocked)
			return
		}
		// Parse before anything else touches it. error_description is never
		// read at all.
		rejection := parseRejection(errCode)
		if rejection == RejectionDeclined {
			writePage(w, http.StatusBadRequest, pageDeclined)
		} else {
			writePage(w, http.StatusBadRequest, pageFailed)
		}
		l.deliver(CallbackResult{Err: &RejectionError{Rejection: rejection}})
		return
	}

	code := q.Get("code")
	if code == "" {
		writePage(w, http.StatusBadRequest, pageNoCode)
		return // keep waiting for the real callback
	}

	if q.Get("state") != l.state {
		writePage(w, http.StatusBadRequest, pageBlocked)
		l.deliver(CallbackResult{Err: errors.New("the login callback did not match this login attempt (possible CSRF); aborted")})
		return
	}

	writePage(w, http.StatusOK, pageSuccess)
	l.deliver(CallbackResult{Code: code})
}

// Close stops the listener. It is idempotent.
//
// closeOnce is not redundant with net/http, though a test cannot show it:
// Server.Close tolerates a second call in today's implementation, so removing
// the guard leaves the suite green. That tolerance is not a documented
// guarantee, and this type promises idempotence in its own right — the guard
// stays, and the surviving mutation is disclosed rather than papered over.
//
// This severs live connections instead of draining them, so a browser can lose
// the success page mid-transfer. That is the right trade here: the caller closes
// only once the exchange is done, and a tab that never finished loading must not
// hold up the CLI. Reach for Shutdown only if that ordering ever changes.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.srv.Close() })
	return err
}
