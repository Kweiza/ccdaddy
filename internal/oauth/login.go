package oauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/browser"
)

// DefaultLoginTimeout bounds a login attempt. Claude Code has no timeout at all;
// ccdad adds one so an abandoned `ccdad add` cannot hold a terminal forever.
const DefaultLoginTimeout = 300 * time.Second

// maxBadPastes bounds the re-prompt. A malformed paste is not fatal ([§6.4]) —
// the loopback race may still be about to win — but a pipe feeding garbage must
// not keep the attempt alive indefinitely.
const maxBadPastes = 5

var (
	// ErrLoginTimeout means neither path delivered a code in time.
	ErrLoginTimeout = errors.New("timed out waiting for the login to complete")
	// ErrLoginInterrupted means the caller cancelled. It is deliberately NOT
	// used for a context deadline: [§9.3] reserves exit 130 for SIGINT, and a
	// supervisor keys on it to mean a human pressed Ctrl-C.
	ErrLoginInterrupted = errors.New("login canceled")
)

// PasteSource yields pasted `code#state` lines. It returns the channel and a
// stop function.
//
// The stop function is best-effort by necessity: os.Stdin.Read is not
// cancellable in Go, so a reader already blocked on the terminal stays blocked
// until the process exits. What stop does reliably release is a reader blocked
// on handing its line over, which is why nothing ever waits for the goroutine.
type PasteSource func() (<-chan string, func())

// StdinPaste reads lines from standard input.
func StdinPaste() PasteSource { return pasteFrom(os.Stdin) }

// pasteFrom is StdinPaste with the reader injected, so the loop is testable
// without a terminal.
func pasteFrom(r io.Reader) PasteSource {
	return func() (<-chan string, func()) {
		// Buffered so the reader can hand off a line and move on even while the
		// login is between select turns.
		ch := make(chan string, 1)
		done := make(chan struct{})

		go func() {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				select {
				case ch <- line:
					// Keep reading. A malformed paste is a re-prompt ([§6.4]),
					// and retiring after one line would leave nothing to
					// re-prompt into.
				case <-done:
					return
				}
			}
		}()

		var once sync.Once
		return ch, func() { once.Do(func() { close(done) }) }
	}
}

// SplitPaste parses a pasted `code#state` value.
//
// Both halves are required. Claude Code requires them syntactically and then
// discards the pasted state; ccdad validates it against the attempt, which costs
// nothing and catches a code pasted from a different login window.
func SplitPaste(s string) (code, state string, err error) {
	s = strings.TrimSpace(s)
	before, after, found := strings.Cut(s, "#")
	if !found || before == "" || after == "" {
		return "", "", errors.New("that does not look like a full code — copy the whole value, including the part after the '#'")
	}
	return before, after, nil
}

// LoginOptions configures one attempt.
type LoginOptions struct {
	// Surface selects the subscription or console authorize endpoint.
	Surface Surface
	// Timeout bounds the attempt. Zero means DefaultLoginTimeout.
	Timeout time.Duration
	// OpenBrowser attempts a launch. Even when false the manual URL is announced.
	OpenBrowser bool
	// Paste supplies pasted lines. Nil disables the paste path, which is right
	// when stdin is not a terminal.
	Paste PasteSource
	// Announce receives the MANUAL authorize URL, before the browser opens.
	// Callers print it so the flow is visible and the URL can be used by hand.
	Announce func(manualURL string)
	// Rejected receives a message for a paste that could not be parsed. The
	// login keeps waiting: [§6.4] makes a malformed paste a re-prompt rather
	// than an abort, because the loopback race may still be about to win.
	Rejected func(msg string)
	// Client is the token endpoint client. Nil means NewClient().
	Client *Client
	// OpenURL launches the LOOPBACK authorize URL when OpenBrowser is set. Nil
	// means browser.Open. It is a field rather than a direct call because it is
	// the only handle anything outside Login has on the loopback port — without
	// it no test can drive the loopback path at all.
	OpenURL func(loopbackURL string) error

	// listen binds the loopback listener. Nil means Listen. Unexported: it
	// exists so a test can inject a bind failure, not as part of the API.
	listen func(expectedState string) (*Listener, error)
}

// LoginResult is a completed login.
type LoginResult struct {
	Token *TokenResponse
	// ViaLoopback records which path won. It is frozen at delivery time, not
	// re-derived afterwards: Go serves each callback in its own goroutine, so a
	// post-hoc check would race the handler.
	ViaLoopback bool
}

// winner is one delivered outcome, carrying the path it arrived on.
type winner struct {
	code        string
	viaLoopback bool
	err         error
}

// awaitWinner joins the two producers and returns the first outcome.
//
// It also returns how many times the select fired. That count is the only thing
// a test can assert to prove a closed producer channel is nil'd out rather than
// re-selected on: a busy loop returns the same error at the same wall-clock
// moment as a correct one, and differs only in turning millions of times to get
// there.
//
// One winner is guaranteed by shape rather than by a mutex: this is a single
// select loop on a single goroutine, and each arm returns from the turn that
// produced it. The concurrency is already absorbed upstream — by
// Listener.sendOnce on the callback side and by the paste reader's single
// hand-off on the other.
func awaitWinner(
	ctx context.Context,
	state string,
	callbackCh <-chan CallbackResult,
	pasteCh <-chan string,
	timeout time.Duration,
	reject func(msg string),
) (winner, int, error) {
	if reject == nil {
		reject = func(string) {}
	}
	// The timer is created ONCE. Rebuilding it inside the loop would restart the
	// deadline on every iteration and it would never fire.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	badPastes := 0
	turns := 0
	for {
		turns++
		select {
		case res, ok := <-callbackCh:
			if !ok {
				// A closed channel stays ready forever. Nil it out or this
				// select spins at full CPU until the deadline.
				callbackCh = nil
				continue
			}
			return winner{code: res.Code, viaLoopback: true, err: res.Err}, turns, nil

		case line, ok := <-pasteCh:
			if !ok {
				pasteCh = nil
				continue
			}
			code, pastedState, err := SplitPaste(line)
			if err != nil {
				badPastes++
				if badPastes >= maxBadPastes {
					return winner{}, turns, fmt.Errorf("gave up after %d unreadable pastes: %w", badPastes, err)
				}
				reject(err.Error())
				continue
			}
			if pastedState != state {
				// [§6.4] makes this arm fatal: a code carrying someone else's
				// state is a possible CSRF, not a typo.
				return winner{err: errors.New("that code did not match the login ccdad started — it may belong to a different login window")}, turns, nil
			}
			return winner{code: code, viaLoopback: false}, turns, nil

		case <-timer.C:
			return winner{}, turns, ErrLoginTimeout

		case <-ctx.Done():
			// A caller-supplied deadline is a timeout, not an interruption.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return winner{}, turns, ErrLoginTimeout
			}
			return winner{}, turns, ErrLoginInterrupted
		}
	}
}

// Login runs the full attempt.
//
// Both paths run at once, exactly as Claude Code does: a loopback listener is
// always started, the MANUAL url is shown to the user, and the browser is
// pointed at the LOOPBACK url. Whichever returns the code first wins, and the
// token exchange must echo back the redirect_uri belonging to that path. Sending
// the other one is a 400.
func Login(ctx context.Context, opts LoginOptions) (*LoginResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultLoginTimeout
	}
	if opts.Client == nil {
		opts.Client = NewClient()
	}
	if opts.Announce == nil {
		opts.Announce = func(string) {}
	}
	listen := opts.listen
	if listen == nil {
		listen = Listen
	}
	openURL := opts.OpenURL
	if openURL == nil {
		openURL = browser.Open
	}

	pkce, err := NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := NewState()
	if err != nil {
		return nil, err
	}

	listener, err := listen(state)
	if err != nil {
		// A machine that cannot bind loopback can still log in by hand, so
		// degrade instead of failing.
		listener = nil
	}
	if listener != nil {
		defer listener.Close()
	}

	var pasteCh <-chan string
	if opts.Paste != nil {
		ch, stop := opts.Paste()
		pasteCh = ch
		defer stop()
	}
	var callbackCh <-chan CallbackResult
	if listener != nil {
		callbackCh = listener.Results()
	}
	if callbackCh == nil && pasteCh == nil {
		// Neither path exists: the loopback bind failed and stdin is not a
		// terminal. Nothing can ever arrive, so do not sit on the deadline.
		return nil, errors.New("no way to receive the login: the loopback listener could not bind and stdin is not a terminal")
	}

	manualURL := AuthorizeURL(AuthorizeParams{
		Surface: opts.Surface, Challenge: pkce.Challenge, State: state,
		RedirectURI: ManualRedirectURL,
	})
	opts.Announce(manualURL)

	if listener != nil && opts.OpenBrowser {
		loopbackURL := AuthorizeURL(AuthorizeParams{
			Surface: opts.Surface, Challenge: pkce.Challenge, State: state,
			RedirectURI: LoopbackRedirectURI(listener.Port()),
		})
		// Best effort. The manual URL is already on screen, so a failed launch
		// costs the user nothing but a copy and paste.
		_ = openURL(loopbackURL)
	}

	got, _, err := awaitWinner(ctx, state, callbackCh, pasteCh, opts.Timeout, opts.Rejected)
	if err != nil {
		return nil, err
	}
	if got.err != nil {
		return nil, got.err
	}

	// The exchange must echo the redirect_uri of the path that won.
	redirectURI := ManualRedirectURL
	if got.viaLoopback {
		redirectURI = LoopbackRedirectURI(listener.Port())
	} else if listener != nil {
		// [§6.6] Claude Code closes the listener immediately at paste time,
		// not after the exchange. Nothing can legitimately arrive on it once a
		// code is in hand, and the exchange is a full network round trip. Close
		// is idempotent, so the deferred one above stays harmless.
		_ = listener.Close()
	}

	token, err := opts.Client.ExchangeCode(ctx, got.code, pkce.Verifier, redirectURI, state)
	if err != nil {
		return nil, fmt.Errorf("completing the login: %w", err)
	}
	return &LoginResult{Token: token, ViaLoopback: got.viaLoopback}, nil
}
