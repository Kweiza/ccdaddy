package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// capture is handed from the httptest handler goroutine to the test goroutine
// over a buffered channel, so the read has a happens-before edge. Assigning to
// captured variables instead is a data race the detector only sometimes sees.
type capture struct {
	method  string
	headers http.Header
	body    map[string]string
}

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient()
	c.TokenEndpoint = srv.URL
	return c
}

// recordingClient answers every request with body and reports what it received.
func recordingClient(t *testing.T, status int, body string) (*Client, <-chan capture) {
	t.Helper()
	ch := make(chan capture, 1)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &decoded)
		ch <- capture{method: r.Method, headers: r.Header.Clone(), body: decoded}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	return c, ch
}

func TestExchangeCodeSendsJSONBody(t *testing.T) {
	c, ch := recordingClient(t, http.StatusOK,
		`{"access_token":"AT","refresh_token":"RT","expires_in":3600,"scope":"user:profile"}`)

	if _, err := c.ExchangeCode(context.Background(), "CODE", "VERIFIER", "REDIRECT", "STATE"); err != nil {
		t.Fatalf("ExchangeCode() = %v, want nil", err)
	}
	got := <-ch

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if ct := got.headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "CODE",
		"redirect_uri":  "REDIRECT",
		"client_id":     ClientID,
		"code_verifier": "VERIFIER",
		"state":         "STATE",
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, got.body[k], v)
		}
	}
	for k := range got.body {
		if _, ok := want[k]; !ok {
			t.Errorf("body carries an unexpected key %q = %q; the exchange body is Claude Code's exactly", k, got.body[k])
		}
	}
}

// Claude Code's exchange sets only Content-Type in its own code (function `maa`
// in the 2.1.238 bundle). ccdad matches that and does not forge the headers
// axios adds beneath it — see NewClient and the post() comment for why. What
// this pins is only that nobody re-adds an Accept header by hand.
func TestExchangeCodeSetsOnlyContentType(t *testing.T) {
	c, ch := recordingClient(t, http.StatusOK, `{"access_token":"AT"}`)
	if _, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s"); err != nil {
		t.Fatal(err)
	}
	got := <-ch

	if v := got.headers.Get("Accept"); v != "" {
		t.Errorf("Accept = %q, want it unset: Claude Code's exchange sets only Content-Type", v)
	}
}

func TestExchangeCodeParsesAccountIdentity(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"access_token":"AT","refresh_token":"RT","expires_in":3600,
			"scope":"user:profile user:inference",
			"account":{"uuid":"acct-1","email_address":"user@example.com"},
			"organization":{"uuid":"org-1"}
		}`)
	})

	got, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "AT" || got.RefreshToken != "RT" || got.ExpiresIn != 3600 {
		t.Fatalf("token fields = %+v", got)
	}
	// The exchange response carries the email, so a browser login needs no
	// extra profile request to label the account.
	if got.Account.EmailAddress != "user@example.com" {
		t.Fatalf("Account.EmailAddress = %q, want user@example.com", got.Account.EmailAddress)
	}
	if got.Account.UUID != "acct-1" {
		t.Fatalf("Account.UUID = %q, want acct-1", got.Account.UUID)
	}
	if got.Organization.UUID != "org-1" {
		t.Fatalf("Organization.UUID = %q, want org-1", got.Organization.UUID)
	}
}

func TestExchangeCodeParsesScopeAndRefreshLifetime(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AT","refresh_token":"RT","expires_in":3600,
			"refresh_token_expires_in":7776000,"scope":"user:profile user:inference"}`)
	})
	got, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "user:profile user:inference" {
		t.Errorf("Scope = %q, want the scope string; it is stored as scopes[]", got.Scope)
	}
	// This becomes refreshTokenExpiresAt in the credential blob.
	if got.RefreshTokenExpiresIn != 7776000 {
		t.Errorf("RefreshTokenExpiresIn = %d, want 7776000", got.RefreshTokenExpiresIn)
	}
}

func TestExchangeCode401IsInvalidCode(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	})

	_, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("ExchangeCode() = %v, want a *TokenError", err)
	}
	if te.Kind != TokenErrorInvalidCode {
		t.Fatalf("Kind = %v, want TokenErrorInvalidCode", te.Kind)
	}
	if te.Status != http.StatusUnauthorized {
		t.Fatalf("Status = %d, want 401", te.Status)
	}
}

func TestExchangeCode500IsTypedStatusError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("ExchangeCode() = %v, want a *TokenError", err)
	}
	if te.Kind != TokenErrorStatus || te.Status != http.StatusInternalServerError {
		t.Fatalf("got Kind=%v Status=%d, want TokenErrorStatus and 500", te.Kind, te.Status)
	}
}

// The endpoint's response body is upstream text of unbounded length. It must
// never reach a user-facing message or a log line.
func TestTokenErrorMessageWithholdsResponseBody(t *testing.T) {
	secret := "SECRET-UPSTREAM-DETAIL"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"`+secret+`"}`)
	})

	_, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	if err == nil {
		t.Fatal("ExchangeCode() = nil, want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error message leaked the upstream body: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error message %q should name the HTTP status", err.Error())
	}
}

// TokenError must carry the status and nothing else. A field holding any part of
// the response body would let the endpoint forge log lines, so the SHAPE of the
// struct is pinned, not just the text of Error().
func TestTokenErrorCarriesNothingButKindAndStatus(t *testing.T) {
	ty := reflect.TypeOf(TokenError{})
	want := map[string]reflect.Kind{"Kind": reflect.Int, "Status": reflect.Int}
	if ty.NumField() != len(want) {
		t.Fatalf("TokenError has %d fields, want exactly %d; no part of the response body may be stored", ty.NumField(), len(want))
	}
	for i := range ty.NumField() {
		f := ty.Field(i)
		if k, ok := want[f.Name]; !ok || f.Type.Kind() != k {
			t.Fatalf("unexpected field %s %s on TokenError", f.Name, f.Type)
		}
	}
}

// A zero-value TokenError must not read as a real classification, or
// "was never set" and "is a transport failure" become indistinguishable.
func TestTokenErrorKindZeroValueIsNotAClassification(t *testing.T) {
	var k TokenErrorKind
	for _, real := range []TokenErrorKind{TokenErrorTransport, TokenErrorInvalidCode, TokenErrorStatus} {
		if k == real {
			t.Fatalf("the zero TokenErrorKind equals %v; the kinds must start at 1", real)
		}
	}
	if got, want := TokenErrorTransport.String(), "transport"; got != want {
		t.Errorf("TokenErrorTransport.String() = %q, want %q", got, want)
	}
	if got, want := TokenErrorKind(0).String(), "TokenErrorKind(0)"; got != want {
		t.Errorf("TokenErrorKind(0).String() = %q, want %q", got, want)
	}
}

func TestRefreshSendsRefreshGrant(t *testing.T) {
	c, ch := recordingClient(t, http.StatusOK, `{"access_token":"AT2","refresh_token":"RT2","expires_in":60}`)

	res, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "OLD-RT"})
	if err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}
	got := <-ch

	if got.body["grant_type"] != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", got.body["grant_type"])
	}
	if got.body["refresh_token"] != "OLD-RT" {
		t.Fatalf("refresh_token = %q, want OLD-RT", got.body["refresh_token"])
	}
	if got.body["client_id"] != ClientID {
		t.Fatalf("client_id = %q, want %q", got.body["client_id"], ClientID)
	}
	if _, ok := got.body["state"]; ok {
		t.Errorf("the refresh grant must not carry state; got %q", got.body["state"])
	}
	if res.AccessToken != "AT2" {
		t.Fatalf("AccessToken = %q, want AT2", res.AccessToken)
	}
}

// Claude Code's refresh sends a scope parameter, and it is NOT the authorize
// set: it drops org:create_api_key. The live ~/.claude/.credentials.json holds
// exactly these five, so a refresh that omits the parameter — which RFC 6749
// reads as "keep the original grant" — would store six scopes where Claude Code
// stores five, in the very file ccdad swaps.
func TestRefreshSendsClaudeCodesNarrowedScopeSet(t *testing.T) {
	const want = "user:profile user:inference user:sessions:claude_code " +
		"user:mcp_servers user:file_upload"
	c, ch := recordingClient(t, http.StatusOK, `{"access_token":"AT2"}`)
	if _, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "OLD-RT"}); err != nil {
		t.Fatal(err)
	}
	got := <-ch

	if got.body["scope"] != want {
		t.Fatalf("refresh scope = %q, want %q", got.body["scope"], want)
	}
}

// RFC 6749 §6 makes issuing a NEW refresh token optional, and Claude Code
// defends against its absence: `let {access_token:u, refresh_token:d=e, ...}`
// in the 2.1.238 bundle defaults the field to the token it just sent. Returning
// "" instead would clobber a still-valid token in the credential file, and the
// next refresh would post an empty one, earn a 400 invalid_grant, and trip the
// anti-flap quarantine — a healthy account destroyed by a response Claude Code
// survives untouched.
func TestRefreshKeepsTheSentTokenWhenTheResponseOmitsOne(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AT2","expires_in":28800,"scope":"user:profile"}`)
	})
	got, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "STILL-VALID-RT"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "STILL-VALID-RT" {
		t.Fatalf("RefreshToken = %q, want the token we sent back unchanged", got.RefreshToken)
	}
}

// The fallback must not shadow a token the endpoint did rotate to.
func TestRefreshPrefersARotatedTokenOverTheOneItSent(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AT2","refresh_token":"ROTATED-RT","expires_in":28800}`)
	})
	got, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "OLD-RT"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "ROTATED-RT" {
		t.Fatalf("RefreshToken = %q, want ROTATED-RT", got.RefreshToken)
	}
}

func TestRefresh400InvalidGrantIsInvalidCode(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"LEAK"}`)
	})
	_, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "DEAD"})
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("Refresh() = %v, want a *TokenError", err)
	}
	// A dead refresh token is what the anti-flap quarantine fires on; it must
	// not read as a generic bad status.
	if te.Kind != TokenErrorInvalidCode {
		t.Fatalf("Kind = %v, want TokenErrorInvalidCode", te.Kind)
	}
	if strings.Contains(err.Error(), "LEAK") {
		t.Fatalf("error leaked the body: %q", err.Error())
	}
}

// Only RFC 6749's closed set is consulted; any other error code stays a plain
// status failure, so the probe cannot become a general body-reading path.
func TestRefresh400OtherErrorStaysAStatusError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_request"}`)
	})
	_, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "RT"})
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("Refresh() = %v, want a *TokenError", err)
	}
	if te.Kind != TokenErrorStatus {
		t.Fatalf("Kind = %v, want TokenErrorStatus", te.Kind)
	}
}

func TestExchangeCodeTransportFailure(t *testing.T) {
	c := NewClient()
	c.TokenEndpoint = "http://127.0.0.1:1/nope" // nothing listens on port 1

	_, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	var te *TokenError
	if !errors.As(err, &te) {
		t.Fatalf("ExchangeCode() = %v, want a *TokenError", err)
	}
	if te.Kind != TokenErrorTransport {
		t.Fatalf("Kind = %v, want TokenErrorTransport", te.Kind)
	}
}

func TestExchangeCodeRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cancel()

	_, err := c.ExchangeCode(ctx, "c", "v", "r", "s")
	// Ctrl-C must stay recognizable as cancellation: it is exit 130, not the
	// exit 1 a transport failure gets.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExchangeCode() = %v, want an error matching context.Canceled", err)
	}
}

// Cancelling while the body is still streaming must also read as cancellation.
// The guard on the Do error does not cover this: by then the request has an
// answer and the read is what gets interrupted, so a missing guard here reports
// Ctrl-C as an unreachable endpoint and maps it to exit 1 instead of 130.
//
// The wait before cancelling is there to land in the body read rather than in
// Do. If it landed in Do instead the test would still pass — it just would not
// exercise this path — so the wait can never make it fail spuriously.
func TestExchangeCodeRespectsCancellationWhileReadingTheBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	flushed := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A length the handler never finishes writing, so the client blocks in
		// the body read until the context is cancelled.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"access_token":"AT",`)
		w.(http.Flusher).Flush()
		close(flushed)
		<-r.Context().Done()
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := c.ExchangeCode(ctx, "c", "v", "r", "s")
		errCh <- err
	}()

	<-flushed
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExchangeCode() = %v, want an error matching context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExchangeCode did not return after the context was cancelled")
	}
}

func TestExchangeCodeCapsResponseBody(t *testing.T) {
	// Truncating at the cap cuts this body mid-string, so it stops being valid
	// JSON. Without the cap it parses and the call succeeds.
	pad := strings.Repeat("x", maxTokenResponse)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"pad":"`+pad+`","access_token":"AT"}`)
	})
	if _, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s"); err == nil {
		t.Fatal("ExchangeCode() = nil, want an error: the response body must be read through a cap")
	}
}

func TestExchangeCodeRejectsResponseWithoutAccessToken(t *testing.T) {
	// A 200 with no token must not become a stored credential that can never
	// authenticate.
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"refresh_token":"RT","expires_in":3600}`)
	})
	if _, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s"); err == nil {
		t.Fatal("ExchangeCode() = nil, want an error for a response with no access token")
	}
}

func TestExchangeCodeRejectsNonJSONResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json`)
	})
	if _, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s"); err == nil {
		t.Fatal("ExchangeCode() = nil, want an error for a body that is not JSON")
	}
}

// encoding/json fills the fields it decoded BEFORE the one that failed, so a
// decode error can coexist with a populated access token. That makes the JSON
// guard load-bearing rather than redundant with the empty-token guard: without
// it this response becomes a stored credential whose expiry is silently zero.
func TestExchangeCodeRejectsPartiallyDecodableResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AT","expires_in":"not-a-number"}`)
	})
	if _, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s"); err == nil {
		t.Fatal("ExchangeCode() = nil, want an error: a body that fails to decode must not become a credential")
	}
}

// The token endpoint never legitimately redirects, and following one is a
// credential-disclosure path: 307 and 308 replay the POST body, which carries
// the refresh token and the PKCE verifier, to whatever host the redirect names.
// Worse, every 3xx — 303 included, where the body is dropped — would let that
// host supply the access and refresh tokens ccdad then writes into the
// credential file. A 3xx is a failed exchange, not a hop.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		io.WriteString(w, `{"access_token":"EVIL-TOKEN","refresh_token":"EVIL-RT"}`)
	}))
	t.Cleanup(evil.Close)

	for _, code := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, evil.URL, code)
		})
		res, err := c.Refresh(context.Background(), RefreshParams{RefreshToken: "SUPER-SECRET-REFRESH-TOKEN"})
		if err == nil {
			t.Fatalf("redirect %d: Refresh() = %+v, want an error", code, res)
		}
		var te *TokenError
		if !errors.As(err, &te) {
			t.Fatalf("redirect %d: err = %v, want a *TokenError", code, err)
		}
		if te.Status != code {
			t.Errorf("redirect %d: Status = %d, want the redirect status itself", code, te.Status)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the redirect target was contacted %d times; it must never be reached", n)
	}
}

// Same guard as PKCE, on a more valuable secret. The refuting reviewer was
// right that no caller exists today — but no caller exists for anything in this
// package yet, and part 3 hands this exact struct to the credential writer.
func TestTokenResponseDoesNotLeakTokensWhenPrinted(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"sk-ant-oat01-SECRET","refresh_token":"sk-ant-ort01-SECRET",
			"expires_in":3600,"scope":"user:profile"}`)
	})
	got, err := c.ExchangeCode(context.Background(), "c", "v", "r", "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%s", "%#v", "%q"} {
		for _, arg := range []any{*got, got, struct{ T TokenResponse }{*got}} {
			s := fmt.Sprintf(format, arg)
			for _, secret := range []string{got.AccessToken, got.RefreshToken} {
				if strings.Contains(s, secret) {
					t.Errorf("fmt.Sprintf(%q, %T) leaked a token: %s", format, arg, s)
				}
			}
		}
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient()
	if c.TokenEndpoint != TokenURL {
		t.Fatalf("TokenEndpoint = %q, want %q", c.TokenEndpoint, TokenURL)
	}
	// A login that hangs on a stalled connection never returns to the prompt.
	if c.HTTP == nil || c.HTTP.Timeout != tokenRequestTimeout {
		t.Fatalf("HTTP = %+v, want a client with a %s timeout", c.HTTP, tokenRequestTimeout)
	}
}
