package oauth

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
)

// countingServer answers each request from replies[i] and records every body.
func countingServer(t *testing.T, replies ...func(w http.ResponseWriter)) (*Client, *[]map[string]string) {
	t.Helper()
	var n atomic.Int32
	bodies := make([]map[string]string, 0, len(replies))
	done := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &decoded)
		bodies = append(bodies, decoded)
		i := int(n.Add(1)) - 1
		if i < len(replies) {
			replies[i](w)
		} else {
			w.WriteHeader(http.StatusTeapot)
		}
	})
	t.Cleanup(func() { close(done) })
	return c, &bodies
}

func ok200(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func status(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

// Claude Code's refresh caller never sends the bare five-scope set when the
// stored credential carries expansion scopes: it sends
// `eo([...UYe, ...preservableScopesFrom(stored)])`. Dropping user:plugins on a
// refresh silently removes a capability the account was granted.
func TestRefreshWithPreservesExpansionScopes(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2","refresh_token":"RT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "RT",
		Scopes: []string{
			"user:profile", "user:inference", "user:sessions:claude_code",
			"user:mcp_servers", "user:file_upload", "user:plugins",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "user:profile user:inference user:sessions:claude_code " +
		"user:mcp_servers user:file_upload user:plugins"
	if got := (*bodies)[0]["scope"]; got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
}

// Only the three PRESERVABLE_EXPANSION_SCOPES survive the filter. A stored
// org:create_api_key must NOT come back — Claude Code drops it on refresh.
func TestRefreshWithFiltersToPreservableScopesOnly(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "RT",
		Scopes:       []string{"org:create_api_key", "user:inference", "user:some:future:scope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := (*bodies)[0]["scope"]; got != RefreshScopeString {
		t.Fatalf("scope = %q, want the bare refresh set %q", got, RefreshScopeString)
	}
}

// Claude Code dedupes with `[...new Set(e)]`, which drops every repeat, not
// just adjacent ones. A stored list that repeats a scope non-adjacently must
// still produce each scope once.
func TestRefreshWithDedupesNonAdjacentRepeats(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "RT",
		Scopes: []string{
			"user:plugins", "user:inference", "user:projects:read", "user:plugins",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields((*bodies)[0]["scope"])
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("scope %q appears %d times in %q", s, n, (*bodies)[0]["scope"])
		}
	}
	want := RefreshScopeString + " user:plugins user:projects:read"
	if (*bodies)[0]["scope"] != want {
		t.Fatalf("scope = %q, want %q", (*bodies)[0]["scope"], want)
	}
}

func TestPreservableScopesFrom(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"user:profile"}, nil},
		{[]string{"user:plugins"}, []string{"user:plugins"}},
		{[]string{"user:projects:write", "user:profile", "user:projects:read"},
			[]string{"user:projects:write", "user:projects:read"}},
	} {
		got := PreservableScopesFrom(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("PreservableScopesFrom(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("PreservableScopesFrom(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}

// A credential with its own clientId is not the default first-party client, so
// Claude Code sends the stored scopes verbatim rather than its own set.
func TestRefreshWithSendsStoredScopesVerbatimForACustomClient(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "RT",
		Scopes:       []string{"user:inference", "user:profile"},
		ClientID:     "custom-client-uuid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (*bodies)[0]["scope"], "user:inference user:profile"; got != want {
		t.Fatalf("scope = %q, want the stored set verbatim %q", got, want)
	}
	if got := (*bodies)[0]["client_id"]; got != "custom-client-uuid" {
		t.Fatalf("client_id = %q, want the credential's own client", got)
	}
}

// Neither an inference scope nor a subscription means it is not the first-party
// default either, even with no explicit clientId.
func TestRefreshWithSendsStoredScopesWhenNotFirstParty(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "RT",
		Scopes:       []string{"user:profile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := (*bodies)[0]["scope"], "user:profile"; got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
}

// A subscriptionType makes it first-party even without an inference scope.
func TestRefreshWithTreatsASubscriptionAsFirstParty(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken:     "RT",
		Scopes:           []string{"user:profile", "user:plugins"},
		SubscriptionType: "max",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := RefreshScopeString + " user:plugins"
	if got := (*bodies)[0]["scope"]; got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
}

// The zero-value params must reproduce the plain Refresh wire body exactly.
func TestRefreshWithNoScopesMatchesPlainRefresh(t *testing.T) {
	c, bodies := countingServer(t, ok200(`{"access_token":"AT2"}`))
	if _, err := c.RefreshWith(context.Background(), RefreshParams{RefreshToken: "RT"}); err != nil {
		t.Fatal(err)
	}
	if got := (*bodies)[0]["scope"]; got != RefreshScopeString {
		t.Fatalf("scope = %q, want %q", got, RefreshScopeString)
	}
	if got := (*bodies)[0]["client_id"]; got != ClientID {
		t.Fatalf("client_id = %q, want %q", got, ClientID)
	}
}

// Claude Code retries ONCE with the raw stored scopes when the endpoint rejects
// the widened set with 400 invalid_scope. Without it, an account whose stored
// scopes the server no longer honours cannot refresh at all.
func TestRefreshWithRetriesWithStoredScopesOnInvalidScope(t *testing.T) {
	c, bodies := countingServer(t,
		status(http.StatusBadRequest, `{"error":"invalid_scope"}`),
		ok200(`{"access_token":"AT2","refresh_token":"RT2"}`),
	)
	stored := []string{"user:inference", "user:profile", "user:plugins"}
	got, err := c.RefreshWith(context.Background(), RefreshParams{RefreshToken: "RT", Scopes: stored})
	if err != nil {
		t.Fatalf("RefreshWith() = %v, want the retry to succeed", err)
	}
	if got.AccessToken != "AT2" {
		t.Fatalf("AccessToken = %q, want AT2", got.AccessToken)
	}
	if len(*bodies) != 2 {
		t.Fatalf("made %d requests, want exactly 2", len(*bodies))
	}
	if s := (*bodies)[0]["scope"]; !strings.Contains(s, "user:plugins") {
		t.Errorf("first attempt scope = %q, want the widened set", s)
	}
	if got, want := (*bodies)[1]["scope"], "user:inference user:profile user:plugins"; got != want {
		t.Fatalf("retry scope = %q, want the stored set verbatim %q", got, want)
	}
}

// The retry is keyed to invalid_scope alone. A dead refresh token must fail on
// the first attempt: refreshing is not idempotent and must not be re-sent.
func TestRefreshWithDoesNotRetryOnInvalidGrant(t *testing.T) {
	c, bodies := countingServer(t,
		status(http.StatusBadRequest, `{"error":"invalid_grant"}`),
		ok200(`{"access_token":"SHOULD-NOT-HAPPEN"}`),
	)
	_, err := c.RefreshWith(context.Background(), RefreshParams{
		RefreshToken: "DEAD", Scopes: []string{"user:inference", "user:plugins"},
	})
	var te *TokenError
	if !errors.As(err, &te) || te.Kind != TokenErrorInvalidCode {
		t.Fatalf("err = %v, want a *TokenError with TokenErrorInvalidCode", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("made %d requests, want exactly 1: a refresh must not be re-sent", len(*bodies))
	}
}

// No stored scopes means nothing to fall back TO, so there is no second attempt.
func TestRefreshWithDoesNotRetryWithoutStoredScopes(t *testing.T) {
	c, bodies := countingServer(t,
		status(http.StatusBadRequest, `{"error":"invalid_scope"}`),
		ok200(`{"access_token":"SHOULD-NOT-HAPPEN"}`),
	)
	_, err := c.RefreshWith(context.Background(), RefreshParams{RefreshToken: "RT"})
	var te *TokenError
	if !errors.As(err, &te) || te.Kind != TokenErrorInvalidScope {
		t.Fatalf("err = %v, want a *TokenError with TokenErrorInvalidScope", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("made %d requests, want exactly 1", len(*bodies))
	}
}

// Claude Code's error parser reads `error` as a string OR as an object with a
// `type` field: `code: typeof r==="string" ? r : (r && typeof r==="object" ? r.type : undefined)`.
// Classifying only the string shape would miss a dead refresh token entirely.
func TestErrorCodeAcceptsBothWireShapes(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       TokenErrorKind
	}{
		{"string", `{"error":"invalid_grant"}`, TokenErrorInvalidCode},
		{"object", `{"error":{"type":"invalid_grant"}}`, TokenErrorInvalidCode},
		{"string scope", `{"error":"invalid_scope"}`, TokenErrorInvalidScope},
		{"object scope", `{"error":{"type":"invalid_scope"}}`, TokenErrorInvalidScope},
		{"unrelated", `{"error":"invalid_request"}`, TokenErrorStatus},
		{"object unrelated", `{"error":{"type":"invalid_request"}}`, TokenErrorStatus},
		{"no error key", `{"detail":"nope"}`, TokenErrorStatus},
		{"not json", `nope`, TokenErrorStatus},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, c.body)
			})
			_, err := cl.Refresh(context.Background(), "RT")
			var te *TokenError
			if !errors.As(err, &te) {
				t.Fatalf("err = %v, want a *TokenError", err)
			}
			if te.Kind != c.want {
				t.Fatalf("Kind = %v, want %v", te.Kind, c.want)
			}
		})
	}
}

// The allowlist is the only thing keeping wireErrorCode from being a path by
// which arbitrary upstream bytes leave the response body. Classification hides
// that today because both call sites compare against a literal, so pin the
// function's own contract: nothing outside the closed set ever comes back.
func TestWireErrorCodeReturnsOnlyTheClosedSet(t *testing.T) {
	for _, body := range []string{
		`{"error":"totally-made-up"}`,
		`{"error":{"type":"totally-made-up"}}`,
		`{"error":"<script>alert(1)</script>"}`,
		`{"error":"invalid_grant\ninjected: log line"}`,
		`{"error":{"type":"unauthorized_client"}}`,
		`{"error":{"type":"access_denied"}}`,
		`{"error":123}`,
		`{"error":null}`,
		`{"error":["invalid_grant"]}`,
		`{}`,
		`not json at all`,
		``,
	} {
		if got := wireErrorCode([]byte(body)); got != "" {
			t.Errorf("wireErrorCode(%s) = %q, want \"\": only the closed set may leave this function", body, got)
		}
	}
	for body, want := range map[string]string{
		`{"error":"invalid_grant"}`:          "invalid_grant",
		`{"error":{"type":"invalid_grant"}}`: "invalid_grant",
		`{"error":"invalid_scope"}`:          "invalid_scope",
		`{"error":{"type":"invalid_scope"}}`: "invalid_scope",
	} {
		if got := wireErrorCode([]byte(body)); got != want {
			t.Errorf("wireErrorCode(%s) = %q, want %q", body, got, want)
		}
	}
}

// A 401 is a rejected credential whatever the body says — Claude Code's DTe
// accepts 400 *or* 401. Every other 401 test here also sends an invalid_grant
// body, so without this one the status arm itself is unpinned.
func Test401WithNoErrorBodyIsStillInvalidCode(t *testing.T) {
	for _, body := range []string{``, `{}`, `{"error":"something_else"}`, `<html>nope</html>`} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, body)
		})
		_, err := c.Refresh(context.Background(), "RT")
		var te *TokenError
		if !errors.As(err, &te) {
			t.Fatalf("body %q: err = %v, want a *TokenError", body, err)
		}
		if te.Kind != TokenErrorInvalidCode {
			t.Errorf("body %q: Kind = %v, want TokenErrorInvalidCode", body, te.Kind)
		}
	}
}

// Whatever the classification, no byte of the body may reach the message.
func TestInvalidScopeErrorStillWithholdsTheBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_scope","error_description":"LEAK-ME"}`)
	})
	_, err := c.Refresh(context.Background(), "RT")
	if err == nil || strings.Contains(err.Error(), "LEAK-ME") {
		t.Fatalf("err = %v, must not carry the upstream body", err)
	}
}

var _ = httptest.NewServer
