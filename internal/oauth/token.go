package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// tokenRequestTimeout matches Claude Code's own 30s on this endpoint.
const tokenRequestTimeout = 30 * time.Second

// maxTokenResponse caps how much of a token response is read. The endpoint is
// upstream and its body length is not ours to trust.
const maxTokenResponse = 1 << 20

// Account identifies the logged-in user. It rides along on the exchange, which
// is why a browser login can label the account without a second request.
type Account struct {
	UUID         string `json:"uuid"`
	EmailAddress string `json:"email_address"`
}

// Organization is the org the account belongs to.
type Organization struct {
	UUID string `json:"uuid"`
}

// TokenResponse is the token endpoint's success body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`

	// RefreshTokenExpiresIn is present on some responses. Keep it: a stored
	// credential that drops it loses the only signal that a refresh token is
	// near the end of its life.
	RefreshTokenExpiresIn int64 `json:"refresh_token_expires_in"`

	Account      Account      `json:"account"`
	Organization Organization `json:"organization"`
}

// String redacts both tokens so a TokenResponse printed with %v, %+v, %s or
// %#v — in an error, a debug line, or a struct that embeds it — cannot leak
// them. This is the same guard PKCE carries, for the same reason and on a
// strictly more valuable secret: the credential writer in part 3 is handed this
// struct, and one %+v in a future log line is all it takes.
func (r TokenResponse) String() string {
	return fmt.Sprintf("TokenResponse{AccessToken:REDACTED, RefreshToken:REDACTED, "+
		"ExpiresIn:%d, Scope:%q, RefreshTokenExpiresIn:%d, Account:%+v, Organization:%+v}",
		r.ExpiresIn, r.Scope, r.RefreshTokenExpiresIn, r.Account, r.Organization)
}

// GoString keeps %#v redacted too; without it fmt falls back to the raw struct.
func (r TokenResponse) GoString() string { return r.String() }

// TokenErrorKind classifies a token-endpoint failure.
type TokenErrorKind int

const (
	// TokenErrorTransport means the request never got an HTTP answer. The kinds
	// start at 1 so a zero-value TokenError never reads as a classification
	// something actually made.
	TokenErrorTransport TokenErrorKind = iota + 1
	// TokenErrorInvalidCode is a 401, or any status carrying RFC 6749's
	// invalid_grant: the code or the refresh token was rejected.
	TokenErrorInvalidCode
	// TokenErrorInvalidScope is a 400 carrying RFC 6749's invalid_scope: the
	// endpoint refused the scope set, not the credential. RefreshWith retries
	// once with the credential's own scopes when it sees this.
	TokenErrorInvalidScope
	// TokenErrorStatus is any other non-200.
	TokenErrorStatus
)

func (k TokenErrorKind) String() string {
	switch k {
	case TokenErrorTransport:
		return "transport"
	case TokenErrorInvalidCode:
		return "invalid_code"
	case TokenErrorInvalidScope:
		return "invalid_scope"
	case TokenErrorStatus:
		return "status"
	}
	return fmt.Sprintf("TokenErrorKind(%d)", int(k))
}

// TokenError is a typed token-endpoint failure.
//
// It carries the HTTP status and nothing else from the wire. The response body
// is upstream text of unbounded length with no newline stripping, so putting it
// in an error — which reaches stderr and the log — would let the endpoint forge
// log lines. The status is enough to act on.
type TokenError struct {
	Kind   TokenErrorKind
	Status int
}

func (e *TokenError) Error() string {
	switch e.Kind {
	case TokenErrorTransport:
		return "could not reach the Claude token endpoint"
	case TokenErrorInvalidCode:
		return fmt.Sprintf("the authorization was rejected (HTTP %d); start the login again", e.Status)
	case TokenErrorInvalidScope:
		return fmt.Sprintf("the token endpoint refused the requested scopes (HTTP %d)", e.Status)
	default:
		return fmt.Sprintf("the token endpoint refused the request (HTTP %d)", e.Status)
	}
}

// Client talks to the token endpoint.
type Client struct {
	HTTP          *http.Client
	TokenEndpoint string
}

// NewClient returns a Client with Claude Code's 30s timeout on this endpoint.
//
// Unlike Claude Code's axios call it refuses to FOLLOW a redirect. The token
// endpoint never legitimately sends one, and following it hands the credentials
// to whoever the redirect names: 307 and 308 replay the POST body — refresh
// token and PKCE verifier included — and every 3xx would let the redirect
// target supply the tokens ccdad writes to disk. Against a well-behaved server
// this deviation is unobservable.
func NewClient() *Client {
	httpClient := &http.Client{
		Timeout: tokenRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// Clone the stdlib transport so this client gets its own connection pool
	// while still honouring the proxy environment variables. If something in
	// the process replaced DefaultTransport with another RoundTripper, leave
	// Transport nil — the client then uses that replacement — rather than
	// asserting the type and panicking inside a login.
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		httpClient.Transport = tr.Clone()
	}
	return &Client{HTTP: httpClient, TokenEndpoint: TokenURL}
}

// ExchangeCode trades an authorization code for tokens.
//
// redirectURI MUST be the one used in the authorize request that produced this
// code. A login races a loopback redirect against a manual paste, and the two
// use different redirect URIs; echoing the wrong one back is a 400.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI, state string) (*TokenResponse, error) {
	return c.post(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     ClientID,
		"code_verifier": verifier,
		"state":         state,
	})
}

// RefreshParams describes one refresh against a stored credential.
//
// Only RefreshToken is required, and params carrying nothing else reproduce a
// default first-party refresh byte for byte. The other fields exist because
// Claude Code's refresh is a function of the STORED credential, not of the
// token alone: see refreshScopes.
type RefreshParams struct {
	RefreshToken string

	// Scopes is the scope set stored with the credential (`scopes[]` in
	// .credentials.json), not the set to request. What gets requested is
	// derived from it.
	Scopes []string

	// SubscriptionType is the credential's subscriptionType, if any. It is one
	// of the two things that mark a credential as the default first-party
	// client.
	SubscriptionType string

	// ClientID is the credential's own clientId, for a credential that did not
	// come from Claude Code's public client. Setting it changes which scopes
	// are requested as well as which client is named.
	ClientID string
}

// isFirstParty mirrors Claude Code's
// `Boolean((IZ(f.scopes) || f.subscriptionType) && !f.clientId)`.
func (p RefreshParams) isFirstParty() bool {
	if p.ClientID != "" {
		return false
	}
	return slices.Contains(p.Scopes, "user:inference") || p.SubscriptionType != ""
}

// refreshScopes is Claude Code's
// `m = d ? eo([...UYe, ...preservableScopesFrom(f.scopes)]) : f.scopes`.
//
// The first-party branch does NOT send the stored set: it sends Claude Code's
// own five plus whatever expansion scopes the credential already holds. Sending
// the bare five instead would silently strip user:plugins and the project
// scopes from an expanded account; sending the stored set would re-request
// org:create_api_key, which a refresh drops.
func (p RefreshParams) refreshScopes() []string {
	if !p.isFirstParty() {
		return p.Scopes
	}
	return dedupe(append(slices.Clone(RefreshScopes), PreservableScopesFrom(p.Scopes)...))
}

// Refresh trades a refresh token for a new pair, as the default first-party
// client with no stored scopes to preserve.
//
// A caller holding a stored credential should use RefreshWith instead: this
// form cannot know about expansion scopes or a custom client, and would strip
// them.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	return c.RefreshWith(ctx, RefreshParams{RefreshToken: refreshToken})
}

// RefreshWith trades a refresh token for a new pair, deriving the request from
// the stored credential the way Claude Code does.
//
// On a 400 invalid_scope it retries ONCE with the credential's own scopes,
// which is Claude Code's `tengu_oauth_refresh_invalid_scope_fallback`. That is
// not a general retry — refreshing is not idempotent — but a 400 issued no
// token, so re-sending is safe, and without it an account whose stored scopes
// the endpoint no longer honours could never refresh at all.
func (c *Client) RefreshWith(ctx context.Context, p RefreshParams) (*TokenResponse, error) {
	out, err := c.refreshOnce(ctx, p, p.refreshScopes())

	var te *TokenError
	if errors.As(err, &te) && te.Kind == TokenErrorInvalidScope &&
		p.isFirstParty() && len(p.Scopes) > 0 {
		out, err = c.refreshOnce(ctx, p, p.Scopes)
	}
	if err != nil {
		return nil, err
	}

	// RFC 6749 §6 makes a NEW refresh token optional, and Claude Code defends
	// against its absence by defaulting the field to the token it just sent
	// (`refresh_token: d = e`). Handing back "" instead would overwrite a
	// still-valid token in the credential file; the next refresh would send an
	// empty one, take a 400 invalid_grant, and quarantine a healthy account.
	// This is the only place that holds both tokens, so no caller can fix it.
	if out.RefreshToken == "" {
		out.RefreshToken = p.RefreshToken
	}
	return out, nil
}

// dedupe keeps the first occurrence of each element, which is what Claude Code's
// `eo(e) { return [...new Set(e)] }` does. slices.Compact is NOT the same: it
// only collapses ADJACENT repeats, so a stored list naming a scope twice with
// something in between would send it twice.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (c *Client) refreshOnce(ctx context.Context, p RefreshParams, scopes []string) (*TokenResponse, error) {
	clientID := p.ClientID
	if clientID == "" {
		clientID = ClientID
	}
	// Claude Code's wire call falls back to its own set for an empty list:
	// `scope:(Array.isArray(t)&&t.length ? t : UYe).join(" ")`.
	scope := RefreshScopeString
	if len(scopes) > 0 {
		scope = strings.Join(scopes, " ")
	}
	return c.post(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": p.RefreshToken,
		"client_id":     clientID,
		"scope":         scope,
	})
}

func (c *Client) post(ctx context.Context, body map[string]string) (*TokenResponse, error) {
	// The endpoint takes a JSON body, not form encoding.
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("building token request: %w", err)
	}
	// Content-Type and nothing else, matching what Claude Code's exchange sets
	// in its own code (spec §3.2).
	//
	// That does NOT make the request indistinguishable on the wire, and it is
	// not meant to. Claude Code posts through axios, whose node adapter adds
	// User-Agent: axios/<ver> and a four-token Accept-Encoding beneath the
	// call, and whose defaults add Accept: application/json, text/plain, */*.
	// Go adds its own User-Agent and Accept-Encoding: gzip instead. Matching
	// those would mean forging a pinned axios version that drifts with every
	// Claude Code release — a worse lie than an honest one — so ccdad matches
	// the header the code sets and leaves the rest to its own client.
	req.Header.Set("Content-Type", "application/json")

	res, err := c.HTTP.Do(req)
	if err != nil {
		// Cancellation must stay recognizable as cancellation: Ctrl-C is exit
		// 130, not the exit 1 a transport failure gets.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &TokenError{Kind: TokenErrorTransport}
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxTokenResponse))
	if err != nil {
		// Same reasoning as the Do error above, and it has to be repeated: a
		// cancellation that lands while the body is streaming is still a
		// cancellation, not an unreachable endpoint.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &TokenError{Kind: TokenErrorTransport}
	}
	if res.StatusCode != http.StatusOK {
		kind := TokenErrorStatus
		switch {
		case res.StatusCode == http.StatusUnauthorized, wireErrorCode(data) == "invalid_grant":
			kind = TokenErrorInvalidCode
		case wireErrorCode(data) == "invalid_scope":
			kind = TokenErrorInvalidScope
		}
		return nil, &TokenError{Kind: kind, Status: res.StatusCode}
	}

	var out TokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("the token endpoint returned a body that is not valid JSON")
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("the token endpoint returned no access token")
	}
	return &out, nil
}

// wireErrorCode extracts the RFC 6749 error code from a failure body, mapping
// anything outside the closed set to "".
//
// It accepts both shapes Claude Code accepts. Its parser is
// `code: typeof r === "string" ? r : (r && typeof r === "object" ? r.type : undefined)`,
// so an endpoint answering {"error":{"type":"invalid_grant"}} is a dead refresh
// token to Claude Code; reading only the string shape would miss it and leave
// §7.2's quarantine signal unfired.
//
// Only a member of the closed set is ever returned — an unrecognised code
// becomes "" — so no byte of the response body escapes and TokenError keeps its
// no-bytes-from-the-wire guarantee.
func wireErrorCode(data []byte) string {
	var wire struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || len(wire.Error) == 0 {
		return ""
	}

	var code string
	if err := json.Unmarshal(wire.Error, &code); err != nil {
		var object struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(wire.Error, &object); err != nil {
			return ""
		}
		code = object.Type
	}

	// The allowlist is what keeps this from becoming a body-reading path.
	if slices.Contains([]string{"invalid_grant", "invalid_scope"}, code) {
		return code
	}
	return ""
}
