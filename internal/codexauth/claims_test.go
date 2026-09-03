package codexauth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// jwt builds a token the way the issuer writes one: three dot-separated
// base64url segments with no padding. Nothing here or in production checks the
// signature, so the third segment is a constant.
func jwt(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc([]byte(payload)) + ".c2ln"
}

// paddedJWT is the same token with the payload segment PADDED.
// base64.RawURLEncoding refuses a '=' outright, so a decoder that does not
// strip padding first reads a perfectly good token as corrupt.
func paddedJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		base64.URLEncoding.EncodeToString([]byte(payload)) + ".c2ln"
}

// The claim set is namespaced: the identity, the workspace and the tier all
// live under one URL-shaped key, and only `email` and `exp` are top level.
const fullPayload = `{
  "exp": 1790688319,
  "email": "person@example.com",
  "https://api.openai.com/auth": {
    "chatgpt_user_id": "user-abc",
    "chatgpt_account_id": "ws-1",
    "chatgpt_plan_type": "pro",
    "organizations": [
      {"id": "ws-1", "title": "Personal", "role": "owner", "is_default": true},
      {"id": "ws-2", "title": "Acme", "role": "member", "is_default": false}
    ]
  }
}`

func TestDecodeClaimsReadsTheNamespacedObject(t *testing.T) {
	// The namespace constant and the claimsWire.Auth struct tag spell the same
	// literal twice, because a struct tag cannot reference a constant. Pinning
	// it here is what makes the constant a checked fact rather than dead prose
	// beside a tag that could drift away from it.
	if authClaimNamespace != "https://api.openai.com/auth" {
		t.Fatalf("authClaimNamespace = %q; the claimsWire.Auth tag spells the same literal", authClaimNamespace)
	}

	got, err := DecodeClaims(jwt(fullPayload))
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if got.UserID != "user-abc" {
		t.Errorf("UserID = %q, want user-abc", got.UserID)
	}
	if got.AccountID != "ws-1" {
		t.Errorf("AccountID = %q, want ws-1 — the workspace, not the user", got.AccountID)
	}
	if got.PlanType != "pro" {
		t.Errorf("PlanType = %q, want pro", got.PlanType)
	}
	if got.Email != "person@example.com" {
		t.Errorf("Email = %q, want person@example.com", got.Email)
	}
	if len(got.Organizations) != 2 {
		t.Fatalf("Organizations = %+v, want two", got.Organizations)
	}
	if o := got.Organizations[0]; o.ID != "ws-1" || o.Title != "Personal" || o.Role != "owner" || !o.IsDefault {
		t.Errorf("Organizations[0] = %+v", o)
	}
	if o := got.Organizations[1]; o.ID != "ws-2" || o.Title != "Acme" || o.Role != "member" || o.IsDefault {
		t.Errorf("Organizations[1] = %+v", o)
	}
}

// exp is epoch SECONDS. Read as milliseconds it lands in the year 58000 and
// every expiry check answers "plenty of time left" forever.
func TestDecodeClaimsReadsExpAsEpochSeconds(t *testing.T) {
	got, err := DecodeClaims(jwt(fullPayload))
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if want := time.Unix(1790688319, 0).UTC(); !got.Exp.Equal(want) {
		t.Errorf("Exp = %s, want %s", got.Exp, want)
	}
}

func TestDecodeClaimsAcceptsAPaddedPayloadSegment(t *testing.T) {
	token := paddedJWT(fullPayload)
	if !strings.Contains(token, "=") {
		t.Fatal("this fixture's payload needs no padding, so it cannot exercise the padded path")
	}
	got, err := DecodeClaims(token)
	if err != nil {
		t.Fatalf("DecodeClaims() on a padded segment = %v; padding is legal and must not read as corrupt", err)
	}
	if got.UserID != "user-abc" {
		t.Errorf("UserID = %q, want user-abc", got.UserID)
	}
}

// An access token that carries no namespaced object still carries a top-level
// user_id, and that is the documented fallback.
func TestDecodeClaimsFallsBackToTheTopLevelUserID(t *testing.T) {
	got, err := DecodeClaims(jwt(`{"user_id":"user-top","exp":10}`))
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if got.UserID != "user-top" {
		t.Errorf("UserID = %q, want user-top", got.UserID)
	}
}

// A missing claim is an empty field, never an error: the token is legitimate
// and one field ccdad wanted is absent, which is a narrower answer rather than
// a broken token.
func TestDecodeClaimsLeavesAMissingClaimEmpty(t *testing.T) {
	got, err := DecodeClaims(jwt(`{}`))
	if err != nil {
		t.Fatalf("DecodeClaims() error = %v", err)
	}
	if got.UserID != "" || got.AccountID != "" || got.PlanType != "" || got.Email != "" {
		t.Errorf("claims = %+v, want every field empty", got)
	}
	if !got.Exp.IsZero() {
		t.Errorf("Exp = %s, want the zero time — an absent exp is not an expiry in 1970", got.Exp)
	}
	if got.Organizations != nil {
		t.Errorf("Organizations = %+v, want nil", got.Organizations)
	}
}

func TestDecodeClaimsRefusesWhatIsNotAThreePartToken(t *testing.T) {
	for _, bad := range []string{"", "onlyone", "two.parts", "a.b.c.d", "a..c"} {
		if _, err := DecodeClaims(bad); err == nil {
			t.Errorf("DecodeClaims(%q) = nil, want an error", bad)
		}
	}
}

func TestDecodeClaimsRefusesAPayloadThatIsNotJSON(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte("hdr")) + "." + enc([]byte("not json")) + ".c2ln"
	if _, err := DecodeClaims(token); err == nil {
		t.Error("DecodeClaims() = nil for a payload that is not JSON")
	}
}

// The ONLY exp that ever drives anything is the ACCESS token's. The id_token's
// is an hour long and reading it for expiry would report every stored account
// as expired within the hour.
func TestAccessExpiryReadsTheAccessTokenAndNotTheIDToken(t *testing.T) {
	c := Credential{
		IDToken:     jwt(`{"exp":1}`),
		AccessToken: jwt(`{"exp":1790688319}`),
	}
	at, ok := AccessExpiry(c)
	if !ok {
		t.Fatal("AccessExpiry() reported absent for a token carrying an exp")
	}
	if want := time.Unix(1790688319, 0).UTC(); !at.Equal(want) {
		t.Errorf("AccessExpiry() = %s, want %s", at, want)
	}
}

func TestAccessExpiryIsUnknownRatherThanZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Credential
	}{
		{"no access token at all", Credential{}},
		{"an access token that is not a JWT", Credential{AccessToken: "opaque"}},
		{"a payload with no exp", Credential{AccessToken: jwt(`{"email":"a@b"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if at, ok := AccessExpiry(tc.c); ok {
				t.Errorf("AccessExpiry() = %s, true; an unreadable expiry must be unknown, never a moment in 1970", at)
			}
		})
	}
}
