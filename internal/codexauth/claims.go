package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claims is a decoded Codex JWT payload.
//
// NOTHING here is verified. There is no signature check, no issuer check and no
// audience check, and that is deliberate rather than a shortcut: codex itself
// parses the payload the same way, and the token ccdad decodes is one ccdad
// received over TLS from the issuer and stored itself. A signature check would
// need the issuer's key set, which is a network call on every read of a value
// that is already trusted by provenance.
//
// The one field that drives behaviour is Exp, and only ever the ACCESS token's.
// The id_token's exp is one hour, so reading it for expiry would report every
// stored account as dead within the hour of being added.
type Claims struct {
	// Exp is the token's expiry, and the zero time means the payload carried
	// none. Absent is not an expiry at the epoch.
	Exp time.Time
	// Email is the top-level `email` claim: the human label for the account.
	Email string
	// UserID is the ACCOUNT, and it is the uuid ccdad keys a Codex row on. It
	// comes from the namespaced chatgpt_user_id, falling back to a top-level
	// user_id.
	UserID string
	// AccountID is the WORKSPACE, not the user. Two seats in one team
	// workspace share it, so keying an account on it would let a colleague's
	// login overwrite the first one. It is the quota and sharing-group key and
	// the value the ChatGPT-Account-Id header carries.
	AccountID string
	// PlanType is the raw subscription tier, e.g. free, plus, pro, team,
	// enterprise. It is kept verbatim: an unrecognized tier is a tier this
	// build has not seen rather than an error.
	PlanType string
	// Organizations is the workspaces this identity belongs to, with the role
	// it holds in each. `ccdad add codex` reads it to tell an owner from a
	// member.
	Organizations []Organization
}

// Organization is one entry of the organizations claim.
type Organization struct {
	ID        string
	Title     string
	Role      string
	IsDefault bool
}

// authClaimNamespace is the URL-shaped key every identity claim hides under.
// It is a literal because it is one: it is not resolved, fetched or joined with
// anything.
const authClaimNamespace = "https://api.openai.com/auth"

type claimsWire struct {
	Exp    int64          `json:"exp"`
	Email  string         `json:"email"`
	UserID string         `json:"user_id"`
	Auth   *authClaimWire `json:"https://api.openai.com/auth"`
}

type authClaimWire struct {
	UserID        string    `json:"chatgpt_user_id"`
	AccountID     string    `json:"chatgpt_account_id"`
	PlanType      string    `json:"chatgpt_plan_type"`
	Organizations []orgWire `json:"organizations"`
}

type orgWire struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Role      string `json:"role"`
	IsDefault bool   `json:"is_default"`
}

// DecodeClaims reads a JWT's payload segment.
//
// A malformed token is an error and a missing CLAIM is not. The difference is
// the one that decides behaviour: a token ccdad cannot parse at all is a stored
// credential it must not act on, while a token that simply does not carry a
// plan type is a perfectly good login about which one field is unknown.
func DecodeClaims(jwt string) (Claims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, fmt.Errorf("the token is not a three-part JWT")
	}
	raw, err := decodeSegment(parts[1])
	if err != nil {
		// The token itself never reaches the message: it is a live credential
		// and this error is printed.
		return Claims{}, fmt.Errorf("the token's payload segment is not base64url")
	}
	var w claimsWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Claims{}, fmt.Errorf("the token's payload is not a JSON object")
	}

	out := Claims{Email: w.Email, UserID: w.UserID}
	if w.Exp != 0 {
		// SECONDS. The claim is epoch seconds by RFC 7519 and reading it as
		// milliseconds puts every expiry tens of thousands of years out, which
		// is the same as having no expiry check at all.
		out.Exp = time.Unix(w.Exp, 0).UTC()
	}
	if w.Auth != nil {
		if w.Auth.UserID != "" {
			out.UserID = w.Auth.UserID
		}
		out.AccountID = w.Auth.AccountID
		out.PlanType = w.Auth.PlanType
		for _, o := range w.Auth.Organizations {
			out.Organizations = append(out.Organizations, Organization{
				ID: o.ID, Title: o.Title, Role: o.Role, IsDefault: o.IsDefault,
			})
		}
	}
	return out, nil
}

// decodeSegment decodes one base64url segment, padded or not.
//
// The issuer writes these unpadded and the standard permits either, so
// base64.RawURLEncoding alone would refuse a legal token outright. Trimming the
// padding and decoding raw accepts both without accepting a segment whose
// padding is wrong in the middle.
func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// AccessExpiry is when c's ACCESS token expires, and whether that could be
// read at all.
//
// There is no stored expiry field anywhere in a Codex credential, so this
// decode is the only source of the figure. Unknown is never zero: a caller
// handed the zero time would read every unreadable credential as expired in
// 1970 and refresh the whole fleet on sight, spending a single-use grant per
// account for nothing.
func AccessExpiry(c Credential) (time.Time, bool) {
	claims, err := DecodeClaims(c.AccessToken)
	if err != nil || claims.Exp.IsZero() {
		return time.Time{}, false
	}
	return claims.Exp, true
}
