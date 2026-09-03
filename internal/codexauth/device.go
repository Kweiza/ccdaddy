package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The OAuth surface, measured against codex-cli 0.151.0 and its matching
// source. Every one of these is a public value: the client id is a PUBLIC
// client with no secret, and the three URLs are the ones codex itself posts to.
//
// ccdad uses the DEVICE-CODE flow and never the browser one. The authorize page
// is Cloudflare-challenged — a GET with the full parameter set answers 403 and
// a "Just a moment..." interstitial under every user agent tried — so a Go
// client must not fetch it, while the device endpoints answer a bare client
// with no cookies and no challenge.
//
// Three endpoints ccdad deliberately does NOT have:
//
//   - /oauth/revoke. codex's own login and logout both revoke the stored
//     refresh token server-side before doing anything else, with no undo. ccdad
//     never calls it and never execs those two commands, so a managed account
//     cannot be destroyed by a switch.
//   - the RFC-8693 api-key exchange codex runs after a browser login. Its
//     result is stored as OPENAI_API_KEY, and a credential carrying that key
//     infers api-key mode, where refresh is a no-op forever — a subscription
//     account silently converted to a different meter.
//   - anything that acquires, stores or replays an anti-bot clearance token,
//     cookie or TLS fingerprint. If an endpoint starts challenging, this
//     feature degrades; it does not evade.
const (
	// AuthBaseURL is the issuer. Every URL below is built from it, so the set
	// cannot drift a path at a time.
	AuthBaseURL = "https://auth.openai.com"

	deviceAuthPath     = "/api/accounts/deviceauth/usercode"
	deviceTokenPath    = "/api/accounts/deviceauth/token"
	deviceCallbackPath = "/deviceauth/callback"
	deviceVerifyPath   = "/codex/device"
	tokenPath          = "/oauth/token"
)

const (
	// ClientID is the public client codex ships. There is no third-party client
	// registration for this API, so ccdad presents the same one rather than
	// inventing an identity the issuer has never seen.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// DeviceAuthURL mints a user code.
	DeviceAuthURL = AuthBaseURL + deviceAuthPath
	// DeviceTokenURL is polled until the user approves the code.
	DeviceTokenURL = AuthBaseURL + deviceTokenPath
	// DeviceVerifyURL is the page the user opens to type the code.
	DeviceVerifyURL = AuthBaseURL + deviceVerifyPath
	// DeviceRedirectURI is what the authorization-code exchange must declare.
	// It is not a listener ccdad binds — no device-code login has one — but the
	// issuer matches it against the grant, so it has to be exactly this.
	DeviceRedirectURI = AuthBaseURL + deviceCallbackPath
	// TokenURL serves both the authorization-code exchange and the refresh.
	// They are not the same request: the exchange is form-encoded and the
	// refresh is JSON, and sending either encoding to the other is refused.
	TokenURL = AuthBaseURL + tokenPath
)

const (
	// DeviceLoginWindow is how long a user code lives. The prompt codex prints
	// says fifteen minutes and its own poll loop caps at the same figure.
	//
	// It is ccdad's own clock rather than a field off the wire. The user-code
	// response does carry an expiry, and its TYPE is not determinable: codex's
	// response struct ignores the field entirely, so nothing in the source says
	// whether it is an ISO string, epoch seconds or a duration. Guessing wrong
	// is either a login that gives up in seconds or one that never gives up, so
	// ccdad uses the documented window instead of reading a field it cannot
	// type.
	DeviceLoginWindow = 15 * time.Minute

	// DefaultDeviceInterval is the wait when the response advertised none, or
	// advertised one that does not parse.
	//
	// Zero is what the wire's own default decodes to, and zero is a hot loop
	// against an endpoint that has no advertised budget — which would burn the
	// code before the user finished typing it. A floor is the only safe reading
	// of "the server did not say".
	DefaultDeviceInterval = 5 * time.Second
)

// authBase is the issuer requests are actually built against.
//
// It is a package var so a test can point the whole set at an httptest server;
// production never reassigns it. The exported constants above stay absolute so
// they remain the value the rest of ccdad and its help text name, and
// TestTheOAuthConstantsAreTheMeasuredOnes pins them against a rebasing that
// moved one of them by accident.
var authBase = AuthBaseURL

// DeviceStart is a device-code login waiting on the user.
type DeviceStart struct {
	// DeviceAuthID and UserCode are the pair every poll carries. UserCode is
	// also what the user types on the verification page.
	DeviceAuthID string
	UserCode     string
	// Interval is how long to wait between polls, as the server asked.
	Interval time.Duration
	// ExpiresAt is when the code stops being approvable.
	ExpiresAt time.Time
}

type userCodeRequest struct {
	ClientID string `json:"client_id"`
}

type userCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	// UserCodeAlt is the second accepted spelling of the same field. Both are
	// read because the server has been observed to use either and a login that
	// silently had no code to print would leave the user staring at a blank
	// prompt.
	UserCodeAlt string `json:"usercode"`
	// Interval is a STRING on the wire, not a number. Typed as a number the
	// whole response fails to unmarshal and the login dies at step one.
	Interval string `json:"interval"`
}

type devicePollRequest struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

type devicePollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// maxAuthBody caps every response this file reads. These are small JSON
// documents from an endpoint ccdad does not control, and an unbounded ReadAll
// is an unbounded allocation in a process that may be a long-lived daemon.
const maxAuthBody = 1 << 20

// StartDeviceLogin asks the issuer for a user code.
//
// It is the whole of ccdad's contact with the login surface before the user
// acts: nothing is stored, nothing is revoked, and no browser is opened.
func StartDeviceLogin(ctx context.Context, client *http.Client) (DeviceStart, error) {
	body, err := json.Marshal(userCodeRequest{ClientID: ClientID})
	if err != nil {
		return DeviceStart{}, fmt.Errorf("building the device-code request: %w", err)
	}
	raw, err := postJSON(ctx, client, authBase+deviceAuthPath, body)
	if err != nil {
		return DeviceStart{}, err
	}

	var res userCodeResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return DeviceStart{}, fmt.Errorf("the device-code endpoint returned a body ccdad could not read")
	}
	code := res.UserCode
	if code == "" {
		code = res.UserCodeAlt
	}
	if res.DeviceAuthID == "" || code == "" {
		return DeviceStart{}, fmt.Errorf("the device-code endpoint returned no code to enter")
	}

	return DeviceStart{
		DeviceAuthID: res.DeviceAuthID,
		UserCode:     code,
		Interval:     parseInterval(res.Interval),
		ExpiresAt:    time.Now().Add(DeviceLoginWindow),
	}, nil
}

// parseInterval reads the string form and floors it. See DefaultDeviceInterval
// for why an unreadable value is never zero.
func parseInterval(s string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return DefaultDeviceInterval
	}
	return time.Duration(n) * time.Second
}

// PollDeviceLogin waits for the user to approve start, then exchanges the
// authorization code for tokens.
//
// sleep is an argument rather than a call to time.Sleep so a test can drive the
// wait loop without waiting. Production passes time.Sleep.
//
// The PKCE verifier is NOT generated here: the poll response carries the pair
// the server already bound to the code, and ccdad forwards the verifier it was
// given. Minting one would produce a challenge the grant was not issued
// against.
func PollDeviceLogin(ctx context.Context, client *http.Client, start DeviceStart, sleep func(time.Duration)) (Credential, error) {
	poll, err := waitForApproval(ctx, client, start, sleep)
	if err != nil {
		return Credential{}, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", poll.AuthorizationCode)
	// The ABSOLUTE constant, not authBase+deviceCallbackPath. This value is
	// never fetched: the issuer matches it against the grant it minted, so it
	// has to be the issuer's own callback whatever host the request is being
	// sent to. Building it from the test seam would send a loopback URL the
	// grant was not issued against.
	form.Set("redirect_uri", DeviceRedirectURI)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", poll.CodeVerifier)

	raw, err := postForm(ctx, client, authBase+tokenPath, form)
	if err != nil {
		return Credential{}, err
	}
	var tokens tokenResponse
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return Credential{}, fmt.Errorf("the token endpoint returned a body ccdad could not read")
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return Credential{}, fmt.Errorf("the token endpoint returned an incomplete grant")
	}

	// The identity comes out of the id_token rather than out of a second
	// network call: the claim set carries the user, the workspace and the tier
	// already.
	claims, err := DecodeClaims(tokens.IDToken)
	if err != nil {
		return Credential{}, fmt.Errorf("the login succeeded and its id_token could not be read: %w", err)
	}

	return Credential{
		IDToken:      tokens.IDToken,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		AccountID:    claims.AccountID,
		UserID:       claims.UserID,
		LastRefresh:  time.Now().UTC(),
	}, nil
}

// waitForApproval is PollDeviceLogin's loop.
//
// 403 and 404 are the two "not yet" statuses, and everything else is a failure
// rather than a reason to keep asking: a 500 answered by more polling is a
// client hammering an endpoint that already said it could not help.
func waitForApproval(ctx context.Context, client *http.Client, start DeviceStart, sleep func(time.Duration)) (devicePollResponse, error) {
	body, err := json.Marshal(devicePollRequest{
		DeviceAuthID: start.DeviceAuthID,
		UserCode:     start.UserCode,
	})
	if err != nil {
		return devicePollResponse{}, fmt.Errorf("building the device poll request: %w", err)
	}

	for {
		if !time.Now().Before(start.ExpiresAt) {
			return devicePollResponse{}, fmt.Errorf("the device code expired before it was approved; run the command again for a new one")
		}
		raw, status, err := post(ctx, client, authBase+deviceTokenPath, "application/json", bytes.NewReader(body))
		if err != nil {
			return devicePollResponse{}, err
		}
		switch status {
		case http.StatusOK:
			var res devicePollResponse
			if err := json.Unmarshal(raw, &res); err != nil {
				return devicePollResponse{}, fmt.Errorf("the device poll returned a body ccdad could not read")
			}
			if res.AuthorizationCode == "" || res.CodeVerifier == "" {
				return devicePollResponse{}, fmt.Errorf("the device poll returned no grant to exchange")
			}
			return res, nil
		case http.StatusForbidden, http.StatusNotFound:
			if !time.Now().Add(start.Interval).Before(start.ExpiresAt) {
				return devicePollResponse{}, fmt.Errorf("the device code expired before it was approved; run the command again for a new one")
			}
			sleep(start.Interval)
		default:
			return devicePollResponse{}, fmt.Errorf("the device login failed (HTTP %d)", status)
		}
	}
}

// postJSON posts a JSON body and refuses a non-200.
func postJSON(ctx context.Context, client *http.Client, endpoint string, body []byte) ([]byte, error) {
	raw, status, err := post(ctx, client, endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the login endpoint refused the request (HTTP %d)", status)
	}
	return raw, nil
}

// postForm posts the form encoding the authorization-code exchange requires.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) ([]byte, error) {
	raw, status, err := post(ctx, client, endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the token endpoint refused the exchange (HTTP %d)", status)
	}
	return raw, nil
}

// post is the one request shape this file makes. It returns the status rather
// than judging it, because the poll loop treats two statuses as a wait and the
// other two callers treat everything but 200 as a failure.
//
// No error here ever carries the body: an error envelope from this endpoint can
// echo a value ccdad sent.
func post(ctx context.Context, client *http.Client, endpoint, contentType string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, 0, fmt.Errorf("building the login request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("the login was cancelled: %w", ctx.Err())
		}
		return nil, 0, fmt.Errorf("could not reach the login endpoint: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAuthBody))
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("the login was cancelled: %w", ctx.Err())
		}
		return nil, 0, fmt.Errorf("reading the login response: %w", err)
	}
	return raw, res.StatusCode, nil
}
