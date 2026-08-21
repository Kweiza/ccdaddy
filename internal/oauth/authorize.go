package oauth

import (
	"fmt"
	"net/url"
)

// Surface selects which authorize endpoint a login goes to.
type Surface int

const (
	// SurfaceClaudeAI is the subscription login. This is the default and the
	// right answer for Pro, Max, Team, and Enterprise seats.
	SurfaceClaudeAI Surface = iota
	// SurfaceConsole is the Console/API-billing login, for a credit-billed
	// account. It does not mint claude.ai credentials.
	SurfaceConsole
)

// String names the surface, so a failure message reads "console" and not "1".
func (s Surface) String() string {
	switch s {
	case SurfaceClaudeAI:
		return "claude.ai"
	case SurfaceConsole:
		return "console"
	}
	return fmt.Sprintf("Surface(%d)", int(s))
}

// AuthorizeParams is one authorize request.
type AuthorizeParams struct {
	Surface     Surface
	Challenge   string
	State       string
	RedirectURI string
}

// LoopbackRedirectURI is the redirect for the browser path. RFC 8252 wants a
// loopback IP, and Claude Code spells it "localhost", so ccdad does too — the
// string has to match what the authorization server saw.
//
// port must come from a bound listener. A zero port means the bind failed and
// the loopback path is disabled ([§6.4]), in which case the caller must not
// build this URL at all: sending http://localhost:0/callback would turn a local
// bug into an opaque 400 from the authorization server.
func LoopbackRedirectURI(port int) string {
	if port < 1 || port > 65535 {
		panic(fmt.Sprintf("oauth: LoopbackRedirectURI called with port %d; the loopback listener is not bound", port))
	}
	return fmt.Sprintf("http://localhost:%d/callback", port)
}

// AuthorizeURL builds the URL to send the user to.
//
// One login attempt builds this TWICE — once with ManualRedirectURL and once
// with LoopbackRedirectURI — sharing a single verifier, challenge, and state.
// The two differ only in redirect_uri. Whichever path returns the code first
// determines which redirect_uri the token exchange must send; sending the other
// one fails with 400.
//
// Parameter ORDER differs from Claude Code's: url.Values.Encode sorts keys,
// Claude Code appends them. No authorization server is order-sensitive, and the
// percent-encoding is identical, so only the set and the values matter — but a
// reader diffing the two against a browser trace should not read the reordering
// as a defect.
func AuthorizeURL(p AuthorizeParams) string {
	endpoint := ClaudeAIAuthorizeURL
	switch p.Surface {
	case SurfaceConsole:
		endpoint = ConsoleAuthorizeURL
	case SurfaceClaudeAI:
		// The zero value, and the default surface.
	default:
		// An unrecognised surface must not silently borrow the subscription
		// endpoint on purpose; it lands there only because the zero value does.
	}

	q := url.Values{}
	// Claude Code appends code=true to every authorize request, loopback and
	// manual alike. It selects the CLI code flow; the loopback redirect still
	// fires with it set.
	q.Set("code", "true")
	q.Set("client_id", ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", ScopeString)
	q.Set("code_challenge", p.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)

	return endpoint + "?" + q.Encode()
}
