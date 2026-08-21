package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// tokenProbe records what the token endpoint was handed, and how often.
type tokenProbe struct {
	mu          sync.Mutex
	redirectURI string
	calls       int
	onExchange  func()
}

func (p *tokenProbe) seen() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.redirectURI, p.calls
}

func (p *tokenProbe) hook(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onExchange = f
}

func recordingTokenServer(t *testing.T) (*Client, *tokenProbe) {
	t.Helper()
	p := &tokenProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			RedirectURI string `json:"redirect_uri"`
		}
		_ = json.Unmarshal(body, &payload)

		p.mu.Lock()
		p.redirectURI = payload.RedirectURI
		p.calls++
		hook := p.onExchange
		p.mu.Unlock()

		if hook != nil {
			hook()
		}
		io.WriteString(w, `{"access_token":"AT","refresh_token":"RT","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.TokenEndpoint = srv.URL
	return c, p
}

func TestSplitPaste(t *testing.T) {
	code, state, err := SplitPaste("  THE-CODE#THE-STATE\n")
	if err != nil {
		t.Fatalf("SplitPaste() = %v, want nil", err)
	}
	if code != "THE-CODE" || state != "THE-STATE" {
		t.Fatalf("SplitPaste() = (%q, %q), want (THE-CODE, THE-STATE)", code, state)
	}
}

func TestSplitPasteRejectsMissingHalves(t *testing.T) {
	for _, in := range []string{"", "onlycode", "#onlystate", "code#", "   "} {
		if _, _, err := SplitPaste(in); err == nil {
			t.Errorf("SplitPaste(%q) = nil, want an error", in)
		}
	}
}

// When the paste wins, the exchange must echo the MANUAL redirect URI. Spec §6.1
// calls sending the other one the single rule that must not be got wrong.
func TestLoginPasteWinnerUsesManualRedirect(t *testing.T) {
	client, probe := recordingTokenServer(t)

	announcedCh := make(chan string, 1)
	forAssert := make(chan string, 1)
	pasted := make(chan string, 1)

	// The announced URL carries the state; feed it back the way a user would.
	go func() {
		announced := <-announcedCh
		forAssert <- announced
		u, err := url.Parse(announced)
		if err != nil {
			return // Login will time out and the test will say so
		}
		pasted <- "PASTED-CODE#" + u.Query().Get("state")
	}()

	res, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(u string) { announcedCh <- u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if res.ViaLoopback {
		t.Fatal("ViaLoopback = true, want false for a pasted code")
	}
	if res.Token == nil || res.Token.AccessToken != "AT" {
		t.Fatalf("Token = %+v, want the exchanged token", res.Token)
	}
	if seen, calls := probe.seen(); seen != ManualRedirectURL || calls != 1 {
		t.Fatalf("exchange redirect_uri = %q after %d calls, want %q once", seen, calls, ManualRedirectURL)
	}

	// The URL shown to the user must be the MANUAL one, so the page it lands on
	// displays a code to copy. Parse it — Encode percent-encodes the slashes, so
	// substring surgery on the raw URL cannot match.
	announced := <-forAssert
	u, err := url.Parse(announced)
	if err != nil {
		t.Fatalf("announced URL %q does not parse: %v", announced, err)
	}
	if got := u.Query().Get("redirect_uri"); got != ManualRedirectURL {
		t.Fatalf("announced redirect_uri = %q, want the manual URL %q", got, ManualRedirectURL)
	}
}

// The twin of the test above: when the loopback callback wins, the exchange must
// echo the LOOPBACK redirect URI, and the browser must have been pointed at the
// loopback URL rather than the manual one.
func TestLoginLoopbackWinnerUsesLoopbackRedirect(t *testing.T) {
	client, probe := recordingTokenServer(t)

	var wantRedirect string
	res, err := Login(context.Background(), LoginOptions{
		Timeout:     5 * time.Second,
		OpenBrowser: true,
		Client:      client,
		Announce:    func(string) {},
		OpenURL: func(loopbackURL string) error {
			u, perr := url.Parse(loopbackURL)
			if perr != nil {
				return perr
			}
			q := u.Query()
			wantRedirect = q.Get("redirect_uri")
			target := wantRedirect + "?code=LOOPBACK-CODE&state=" + url.QueryEscape(q.Get("state"))
			// Drive the callback exactly as a browser would.
			go func() {
				if r, gerr := http.Get(target); gerr == nil {
					r.Body.Close()
				}
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if !res.ViaLoopback {
		t.Fatal("ViaLoopback = false, want true for a loopback callback")
	}
	if res.Token == nil || res.Token.AccessToken != "AT" {
		t.Fatalf("Token = %+v, want the exchanged token", res.Token)
	}
	if !strings.HasPrefix(wantRedirect, "http://localhost:") {
		t.Fatalf("the browser got redirect_uri %q, want the loopback URL", wantRedirect)
	}
	if seen, calls := probe.seen(); seen != wantRedirect || calls != 1 {
		t.Fatalf("exchange redirect_uri = %q after %d calls, want %q once", seen, calls, wantRedirect)
	}
}

// A pasted state that does not match this attempt is rejected. Claude Code
// discards the pasted state; ccdad validates it, which costs nothing and catches
// a code pasted from a different window. Spec §6.4 makes this arm fatal.
func TestLoginRejectsMismatchedPastedState(t *testing.T) {
	client, _ := recordingTokenServer(t)
	pasted := make(chan string, 1)
	pasted <- "CODE#NOT-THE-RIGHT-STATE"

	_, err := Login(context.Background(), LoginOptions{
		Timeout:     500 * time.Millisecond,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
	})
	if err == nil {
		t.Fatal("Login() = nil, want a state-mismatch error")
	}
	if !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("err = %q, want a state-mismatch message", err)
	}
}

// Spec §6.4 makes a paste without '#' a re-prompt, not an abort: the loopback
// race may still be about to win, and a partial copy-paste is the likeliest user
// error in this flow.
func TestLoginRepromptsOnAMalformedPaste(t *testing.T) {
	client, probe := recordingTokenServer(t)

	announcedCh := make(chan string, 1)
	pasted := make(chan string, 4)
	pasted <- "no-hash-here"
	pasted <- "still-not-right"

	var rejectedMu sync.Mutex
	var rejected []string

	go func() {
		u, err := url.Parse(<-announcedCh)
		if err != nil {
			return
		}
		pasted <- "GOOD-CODE#" + u.Query().Get("state")
	}()

	res, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(u string) { announcedCh <- u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		Rejected: func(msg string) {
			rejectedMu.Lock()
			defer rejectedMu.Unlock()
			rejected = append(rejected, msg)
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil — a malformed paste must not end the login", err)
	}
	if res.ViaLoopback {
		t.Fatal("ViaLoopback = true, want false")
	}
	if _, calls := probe.seen(); calls != 1 {
		t.Fatalf("exchange happened %d times, want once", calls)
	}
	rejectedMu.Lock()
	defer rejectedMu.Unlock()
	if len(rejected) != 2 {
		t.Fatalf("Rejected fired %d times (%v), want twice", len(rejected), rejected)
	}
}

// Bounded, though: a pathological pipe must not spin forever feeding garbage.
func TestLoginGivesUpAfterTooManyMalformedPastes(t *testing.T) {
	client, _ := recordingTokenServer(t)

	pasted := make(chan string, maxBadPastes+2)
	for range maxBadPastes + 2 {
		pasted <- "garbage-without-a-hash"
	}

	_, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
	})
	if err == nil {
		t.Fatal("Login() = nil, want an error after too many unreadable pastes")
	}
	if errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("Login() = %v, want the give-up error rather than a timeout", err)
	}
}

func TestLoginTimesOut(t *testing.T) {
	client, _ := recordingTokenServer(t)
	start := time.Now()

	_, err := Login(context.Background(), LoginOptions{
		Timeout:     200 * time.Millisecond,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return make(chan string), func() {} },
	})
	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("Login() = %v, want ErrLoginTimeout", err)
	}
	// The deadline must actually arrive. Note this does NOT catch a time.After
	// rebuilt inside the select: with no producer active the select simply
	// blocks and a rebuilt timer still fires on schedule. What bounds that
	// defect is the turn count in TestAwaitWinnerDoesNotSpinOnAClosedChannel —
	// a loop that cannot iterate cannot push its deadline out.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Login returned after %v; the 200ms deadline was not honoured", elapsed)
	}
}

// The regression test for the busy loop. A closed channel stays ready forever;
// if the select re-fires on it, the loop turns millions of times before the
// deadline. Bounding the turn count is the only assertion a busy loop fails —
// the wall clock and the returned error look identical either way.
func TestAwaitWinnerDoesNotSpinOnAClosedChannel(t *testing.T) {
	closed := make(chan string)
	close(closed)

	_, turns, err := awaitWinner(context.Background(), "S", nil, closed, 150*time.Millisecond, nil)
	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("err = %v, want ErrLoginTimeout", err)
	}
	if turns > 4 {
		t.Fatalf("the select turned %d times in 150ms; the closed channel is being re-selected instead of nil'd", turns)
	}
}

// The same guard on the callback side.
func TestAwaitWinnerDoesNotSpinOnAClosedCallbackChannel(t *testing.T) {
	closed := make(chan CallbackResult)
	close(closed)

	_, turns, err := awaitWinner(context.Background(), "S", closed, nil, 150*time.Millisecond, nil)
	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("err = %v, want ErrLoginTimeout", err)
	}
	if turns > 4 {
		t.Fatalf("the select turned %d times in 150ms; the closed channel is being re-selected instead of nil'd", turns)
	}
}

func TestLoginHonoursContextCancellation(t *testing.T) {
	client, _ := recordingTokenServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := Login(ctx, LoginOptions{
		Timeout:     5 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return make(chan string), func() {} },
	})
	if !errors.Is(err, ErrLoginInterrupted) {
		t.Fatalf("Login() = %v, want ErrLoginInterrupted", err)
	}
}

// A caller-supplied deadline is a timeout, not an interruption. Spec §9.3
// reserves exit 130 for SIGINT and a supervisor keys on it, so collapsing the
// two would report "the human pressed Ctrl-C" when nothing was interrupted.
func TestLoginContextDeadlineIsATimeoutNotAnInterrupt(t *testing.T) {
	client, _ := recordingTokenServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := Login(ctx, LoginOptions{
		Timeout:  5 * time.Second,
		Client:   client,
		Announce: func(string) {},
		Paste:    func() (<-chan string, func()) { return make(chan string), func() {} },
	})
	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("Login() = %v, want ErrLoginTimeout for a context deadline", err)
	}
	if errors.Is(err, ErrLoginInterrupted) {
		t.Fatalf("Login() = %v, want it NOT to report an interruption", err)
	}
}

// The loopback bind failing and stdin not being a terminal can happen together —
// a sandbox that blocks bind(2), invoked from a pipe. Nothing could ever arrive,
// so waiting out the full deadline would make the user sit for five minutes.
func TestLoginFailsFastWhenNeitherPathExists(t *testing.T) {
	client, _ := recordingTokenServer(t)
	start := time.Now()

	_, err := Login(context.Background(), LoginOptions{
		Timeout:     30 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       nil,
		listen:      func(string) (*Listener, error) { return nil, errors.New("bind refused") },
	})
	if err == nil {
		t.Fatal("Login() = nil, want an error when no path can deliver a code")
	}
	if errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("Login() = %v, want it to fail fast rather than sit on the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Login sat for %v before giving up", elapsed)
	}
}

// Spec §6.6: Claude Code closes the loopback listener immediately at paste time,
// not after the exchange. Nothing can legitimately arrive on it once a code is
// in hand, and the exchange is a full network round trip.
func TestLoginClosesTheListenerOnAPasteWin(t *testing.T) {
	client, probe := recordingTokenServer(t)

	portCh := make(chan int, 1)
	boundDuringExchange := make(chan bool, 1)
	pasted := make(chan string, 1)
	announcedCh := make(chan string, 1)

	probe.hook(func() {
		port := <-portCh
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err == nil {
			conn.Close()
			boundDuringExchange <- true
			return
		}
		boundDuringExchange <- false
	})

	go func() {
		u, err := url.Parse(<-announcedCh)
		if err != nil {
			return
		}
		pasted <- "PASTED-CODE#" + u.Query().Get("state")
	}()

	_, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: true,
		Client:      client,
		Announce:    func(u string) { announcedCh <- u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		OpenURL: func(loopbackURL string) error {
			u, perr := url.Parse(loopbackURL)
			if perr != nil {
				return perr
			}
			lu, perr := url.Parse(u.Query().Get("redirect_uri"))
			if perr != nil {
				return perr
			}
			p, perr := strconv.Atoi(lu.Port())
			if perr != nil {
				return perr
			}
			portCh <- p
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if bound := <-boundDuringExchange; bound {
		t.Fatal("the loopback port was still bound during the exchange; a paste win must close it first")
	}
}

// Both paths can arrive together. Exactly one winner may be admitted — a second
// exchange would burn the other code and could double-spend the attempt.
func TestLoginExchangesExactlyOnceWhenBothPathsArrive(t *testing.T) {
	client, probe := recordingTokenServer(t)

	pasted := make(chan string, 1)
	res, err := Login(context.Background(), LoginOptions{
		Timeout:     5 * time.Second,
		OpenBrowser: true,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		OpenURL: func(loopbackURL string) error {
			u, perr := url.Parse(loopbackURL)
			if perr != nil {
				return perr
			}
			q := u.Query()
			target := q.Get("redirect_uri") + "?code=LOOPBACK-CODE&state=" + url.QueryEscape(q.Get("state"))
			if r, gerr := http.Get(target); gerr == nil {
				r.Body.Close()
			}
			// Now both producers are ready before the join even starts.
			pasted <- "PASTED-CODE#" + q.Get("state")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if res.Token == nil {
		t.Fatal("Token = nil, want the exchanged token")
	}
	if _, calls := probe.seen(); calls != 1 {
		t.Fatalf("the token endpoint was called %d times, want exactly once", calls)
	}
}

// os.Stdin.Read is not cancellable, so the reader must be able to hand its line
// off without the login having to still be listening. A buffered channel is what
// makes that possible.
func TestStdinPasteChannelIsBuffered(t *testing.T) {
	ch, stop := StdinPaste()()
	defer stop()
	if cap(ch) != 1 {
		t.Fatalf("cap(ch) = %d, want 1", cap(ch))
	}
}

// Blank lines are skipped and whitespace trimmed, and the reader keeps serving
// lines after the first — spec §6.4's re-prompt has nothing to re-prompt into if
// the reader retires after one paste.
func TestPasteFromKeepsReadingAfterTheFirstLine(t *testing.T) {
	ch, stop := pasteFrom(strings.NewReader("\n   \n  CODE#STATE  \nSECOND#LINE\n"))()
	defer stop()

	for _, want := range []string{"CODE#STATE", "SECOND#LINE"} {
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("line = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no line delivered, want %q", want)
		}
	}
}

// The deadline is built once, outside the loop. Rebuilding it inside the select
// restarts it on every turn — and the re-prompt path ([§6.4]) makes the loop
// turn for real, so a pipe dribbling malformed pastes could push the deadline
// out indefinitely. With the timer hoisted, a malformed paste consumes an
// attempt without buying any time.
func TestAwaitWinnerDeadlineIsNotPushedOutByRepromptTurns(t *testing.T) {
	pasteCh := make(chan string, 1)
	stop := make(chan struct{})
	defer close(stop)

	// Dribble malformed lines faster than the deadline, but slower than the
	// select can consume them.
	go func() {
		for {
			select {
			case <-time.After(120 * time.Millisecond):
				select {
				case pasteCh <- "malformed-no-hash":
				case <-stop:
					return
				}
			case <-stop:
				return
			}
		}
	}()

	start := time.Now()
	_, _, err := awaitWinner(context.Background(), "S", nil, pasteCh, 200*time.Millisecond, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("err = %v, want ErrLoginTimeout — a rebuilt deadline lets the re-prompts run to the give-up bound instead", err)
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("the deadline arrived after %v for a 200ms timeout; it is being rebuilt inside the loop", elapsed)
	}
}
