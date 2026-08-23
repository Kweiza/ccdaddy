package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	verifier    string
	calls       int
	onExchange  func()
}

func (p *tokenProbe) seen() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.redirectURI, p.calls
}

// exchangedVerifier is the code_verifier the exchange sent. It is the only
// handle a test has on the PKCE pair Login generated, and so the only way to
// tell one challenge from two.
func (p *tokenProbe) exchangedVerifier() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifier
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
			Verifier    string `json:"code_verifier"`
		}
		_ = json.Unmarshal(body, &payload)

		p.mu.Lock()
		p.redirectURI = payload.RedirectURI
		p.verifier = payload.Verifier
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

// When the paste wins, the exchange must echo the MANUAL redirect URI. The
// redirect_uri sent at exchange time must match the one the authorize URL used
// or the exchange fails with 400, which is the single rule that must not be got
// wrong.
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
// a code pasted from a different window. This arm is fatal rather than a
// re-prompt: a code carrying someone else's state is a possible CSRF.
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

// A paste without '#' is a re-prompt, not an abort: the loopback race may still
// be about to win, and a partial copy-paste is the likeliest user error in this
// flow.
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

// A caller-supplied deadline is a timeout, not an interruption. The exit
// contract reserves exit 130 for SIGINT and a supervisor keys on it, so
// collapsing the two would report "the human pressed Ctrl-C" when nothing was
// interrupted.
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

// Claude Code closes the loopback listener immediately at paste time, not after
// the exchange. Nothing can legitimately arrive on it once a code is in hand,
// and the exchange is a full network round trip.
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
// lines after the first — the re-prompt on a malformed paste has nothing to
// re-prompt into if the reader retires after one paste.
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
// restarts it on every turn — and the re-prompt path makes the loop turn for
// real, so a pipe dribbling malformed pastes could push the deadline out
// indefinitely. With the timer hoisted, a malformed paste consumes an attempt
// without buying any time.
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

// Both sides split on the FIRST '#', so a value carrying more than one keeps the
// remainder in the state half. Claude Code's `let[code,state]=s.split("#")`
// destructuring reaches the same code — it drops everything past the second '#'
// rather than keeping it, but the code half, which is the part that matters, is
// identical. A single-'#' fixture cannot tell first from last, which is why this
// case exists.
func TestSplitPasteSplitsOnTheFirstHash(t *testing.T) {
	code, state, err := SplitPaste("THE-CODE#THE#STATE")
	if err != nil {
		t.Fatalf("SplitPaste() = %v, want nil", err)
	}
	if code != "THE-CODE" {
		t.Fatalf("code = %q, want THE-CODE", code)
	}
	if state != "THE#STATE" {
		t.Fatalf("state = %q, want the remainder after the first '#'", state)
	}
}

// A machine that cannot bind loopback logs in by hand instead of failing.
// TestLoginFailsFastWhenNeitherPathExists does not constrain this — it asserts
// only that SOME error comes back when BOTH paths are gone, which a bind site
// that returned its error would satisfy just as well. This is the other half:
// the bind fails, stdin is a terminal, and the login still completes against
// the manual redirect.
func TestLoginDegradesToManualOnlyWhenTheLoopbackBindFails(t *testing.T) {
	client, probe := recordingTokenServer(t)

	announcedCh := make(chan string, 1)
	pasted := make(chan string, 1)
	go func() {
		u, err := url.Parse(<-announcedCh)
		if err != nil {
			return // Login will time out and the test will say so
		}
		pasted <- "PASTED-CODE#" + u.Query().Get("state")
	}()

	res, err := Login(context.Background(), LoginOptions{
		Timeout: 3 * time.Second,
		// True, and it must still not launch: there is no loopback URL to
		// launch, and the manual URL is not the browser's to open.
		OpenBrowser: true,
		Client:      client,
		Announce:    func(u string) { announcedCh <- u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		OpenURL: func(string) error {
			t.Error("the browser was launched with no loopback listener bound")
			return nil
		},
		listen: func(string) (*Listener, error) { return nil, errors.New("bind refused") },
	})
	if err != nil {
		t.Fatalf("Login() = %v, want the login to survive a failed loopback bind", err)
	}
	if res.ViaLoopback {
		t.Fatal("ViaLoopback = true, want false — no listener was ever bound")
	}
	if seen, calls := probe.seen(); seen != ManualRedirectURL || calls != 1 {
		t.Fatalf("exchange redirect_uri = %q after %d calls, want %q once", seen, calls, ManualRedirectURL)
	}
}

// OpenBrowser: false must actually suppress the launch. Dropping the flag from
// the condition leaves the suite green, because every other test either injects
// OpenURL with OpenBrowser true or sets OpenBrowser false and leaves OpenURL
// nil — and in that second case the mutant calls the real browser.Open and
// Login discards the error. On a machine with a browser, that mutant opens real
// tabs during `go test`.
func TestLoginDoesNotLaunchABrowserWhenOpenBrowserIsFalse(t *testing.T) {
	client, _ := recordingTokenServer(t)

	announcedCh := make(chan string, 1)
	pasted := make(chan string, 1)
	go func() {
		u, err := url.Parse(<-announcedCh)
		if err != nil {
			return
		}
		pasted <- "PASTED-CODE#" + u.Query().Get("state")
	}()

	launched := false
	_, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: false,
		Client:      client,
		Announce:    func(u string) { announcedCh <- u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		// Called on Login's goroutine, which is this one, so a plain bool is
		// safe here.
		OpenURL: func(string) error { launched = true; return nil },
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if launched {
		t.Fatal("the browser was launched with OpenBrowser: false")
	}
}

// The order is fixed: print the MANUAL url to stderr, then open the browser at
// the LOOPBACK url — and LoginOptions.Announce documents itself as firing
// "before the browser opens". That ordering is the whole justification for the
// launch being best-effort: the URL is already on screen, so a failed launch
// costs the user a copy and paste and nothing more. Swapping the two is green.
func TestLoginAnnouncesTheManualURLBeforeLaunchingTheBrowser(t *testing.T) {
	client, _ := recordingTokenServer(t)

	pasted := make(chan string, 1)
	var events []string
	_, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: true,
		Client:      client,
		Announce:    func(string) { events = append(events, "announce") },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		OpenURL: func(loopbackURL string) error {
			events = append(events, "open")
			// The state is taken from the URL under test rather than from the
			// announcement, so this cannot deadlock when the order is wrong.
			u, perr := url.Parse(loopbackURL)
			if perr != nil {
				return perr
			}
			pasted <- "PASTED-CODE#" + u.Query().Get("state")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if len(events) != 2 || events[0] != "announce" || events[1] != "open" {
		t.Fatalf("callbacks fired %v, want [announce open] — the URL must be on screen before the launch", events)
	}
}

// One attempt generates ONE verifier/challenge and ONE state, and builds TWO
// authorize URLs differing only in redirect_uri. The state twin is pinned by
// construction — the listener validates the callback's state against the
// attempt's — but the challenge is not, and a second NewPKCE() for the loopback
// URL is green here while being a 400 on the loopback path against the real
// endpoint.
func TestLoginBuildsBothAuthorizeURLsFromOnePKCEPair(t *testing.T) {
	client, probe := recordingTokenServer(t)

	pasted := make(chan string, 1)
	var manualURL, loopbackURL string
	_, err := Login(context.Background(), LoginOptions{
		Timeout:     3 * time.Second,
		OpenBrowser: true,
		Client:      client,
		Announce:    func(u string) { manualURL = u },
		Paste:       func() (<-chan string, func()) { return pasted, func() {} },
		OpenURL: func(u string) error {
			loopbackURL = u
			parsed, perr := url.Parse(u)
			if perr != nil {
				return perr
			}
			pasted <- "PASTED-CODE#" + parsed.Query().Get("state")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}

	verifier := probe.exchangedVerifier()
	if verifier == "" {
		t.Fatal("the exchange sent no code_verifier")
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	var states []string
	for _, tc := range []struct{ name, raw string }{
		{"manual", manualURL},
		{"loopback", loopbackURL},
	} {
		u, perr := url.Parse(tc.raw)
		if perr != nil {
			t.Fatalf("%s authorize URL %q does not parse: %v", tc.name, tc.raw, perr)
		}
		if got := u.Query().Get("code_challenge"); got != want {
			t.Fatalf("%s authorize URL carries code_challenge %q, want %q — the S256 of the verifier the exchange sent", tc.name, got, want)
		}
		states = append(states, u.Query().Get("state"))
	}
	if states[0] != states[1] || states[0] == "" {
		t.Fatalf("states = %q, want one non-empty value on both URLs", states)
	}
}

// Timeout: 0 means DefaultLoginTimeout. Every other test sets Timeout
// explicitly, so both ways of breaking this are green: substituting a tiny
// constant, and dropping the defaulting altogether — which leaves a
// zero-duration timer that fires on the first turn. Asserting the constant
// itself would prove nothing about Login, so what is asserted is that an unset
// Timeout does not expire underneath a login that is still waiting.
func TestLoginWithoutATimeoutDoesNotExpireImmediately(t *testing.T) {
	client, _ := recordingTokenServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := Login(ctx, LoginOptions{
		OpenBrowser: false,
		Client:      client,
		Announce:    func(string) {},
		Paste:       func() (<-chan string, func()) { return make(chan string), func() {} },
	})
	if errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("Login() = %v after %v with Timeout unset; the default deadline is not %v", err, time.Since(start), DefaultLoginTimeout)
	}
	if !errors.Is(err, ErrLoginInterrupted) {
		t.Fatalf("Login() = %v, want ErrLoginInterrupted", err)
	}
}

// The twin of TestLoginClosesTheListenerOnAPasteWin. A paste win closes the
// listener early and by hand; a loopback win has only the deferred close, so
// deleting that defer leaks a serving HTTP server and a bound port for the life
// of the process — and leaves the suite green, because every other loopback
// test stops caring once the token is in hand.
func TestLoginClosesTheListenerOnALoopbackWin(t *testing.T) {
	client, _ := recordingTokenServer(t)

	var port int
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
			redirect := q.Get("redirect_uri")
			lu, perr := url.Parse(redirect)
			if perr != nil {
				return perr
			}
			port, perr = strconv.Atoi(lu.Port())
			if perr != nil {
				return perr
			}
			target := redirect + "?code=LOOPBACK-CODE&state=" + url.QueryEscape(q.Get("state"))
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

	conn, derr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if derr == nil {
		conn.Close()
		t.Fatalf("port %d is still bound after Login returned; the loopback listener was never closed", port)
	}
}
