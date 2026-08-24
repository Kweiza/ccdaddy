package cclink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// oauthAccountKey is the ~/.claude.json key Claude Code caches the live
// OAuth login's profile under. See globalconfig.go's file comment for why
// ccdad has to touch it at all.
const oauthAccountKey = "oauthAccount"

// oauthAccountIdentity is just enough of Claude Code's oauthAccount shape to
// answer "whose is this" -- accountUuid is the only field ccdad's own switch
// decision depends on, and decoding the rest would tie this package to a
// shape only Claude Code owns.
type oauthAccountIdentity struct {
	AccountUUID string `json:"accountUuid"`
}

// OAuthAccountSnapshot returns the raw oauthAccount value, if the config has
// one. A present-but-empty or JSON-null value reads as absent: both are
// states Claude Code's own refresh handler treats as "no cached profile" --
// its merge is gated on `S && y.oauthAccount` (2.1.241).
func OAuthAccountSnapshot(g *GlobalConfig) (json.RawMessage, bool) {
	raw, ok := g.Get(oauthAccountKey)
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false
	}
	return raw, true
}

// OAuthAccountUUID reads accountUuid out of a raw oauthAccount value.
func OAuthAccountUUID(raw json.RawMessage) (string, bool) {
	var id oauthAccountIdentity
	if err := json.Unmarshal(raw, &id); err != nil || id.AccountUUID == "" {
		return "", false
	}
	return id.AccountUUID, true
}

// RestoreOAuthAccountSnapshot installs a previously captured oauthAccount
// object verbatim -- the exact object Claude Code itself last wrote while
// this account was live, byte-for-byte including every cosmetic field ccdad
// never asked for (displayName, billingType, the trial and onboarding
// flags). Nothing ccdad could construct is safer than what Claude Code
// already wrote.
func RestoreOAuthAccountSnapshot(g *GlobalConfig, snapshot json.RawMessage) error {
	if len(bytes.TrimSpace(snapshot)) == 0 {
		return errors.New("refusing to restore an empty oauthAccount snapshot")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, snapshot); err != nil {
		return fmt.Errorf("the stored oauthAccount snapshot is not valid JSON: %w", err)
	}
	g.Set(oauthAccountKey, json.RawMessage(append([]byte(nil), compact.Bytes()...)))
	return nil
}

// ResetOAuthAccountIdentity replaces oauthAccount with a MINIMAL object
// naming only who the account is -- accountUuid, emailAddress, and the
// organization fields ccdad already has from the profile lookup it made when
// the account was added.
//
// It deliberately OMITS billingType, accountCreatedAt, subscriptionCreatedAt,
// and ccOnboardingFlags rather than leaving them at a previous account's
// values or guessing new ones. Claude Code's own token-refresh handler
// treats their combined presence as "the cached profile is already
// complete" and skips re-fetching it entirely (2.1.241's
// oauth_token_refresh_success handler, the boolean it names `b`) --
// omitting them is what makes Claude Code fetch the real values itself on
// its next refresh instead of ccdad forging them.
func ResetOAuthAccountIdentity(g *GlobalConfig, uuid, email, organizationUUID, organizationType, organizationRateLimitTier string) error {
	if uuid == "" {
		return errors.New("refusing to write an oauthAccount identity with no account uuid")
	}
	identity := struct {
		AccountUUID               string `json:"accountUuid"`
		EmailAddress              string `json:"emailAddress,omitempty"`
		OrganizationUUID          string `json:"organizationUuid,omitempty"`
		OrganizationType          string `json:"organizationType,omitempty"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier,omitempty"`
	}{
		AccountUUID:               uuid,
		EmailAddress:              email,
		OrganizationUUID:          organizationUUID,
		OrganizationType:          organizationType,
		OrganizationRateLimitTier: organizationRateLimitTier,
	}
	encoded, err := marshalNoEscape(identity)
	if err != nil {
		return err
	}
	g.Set(oauthAccountKey, encoded)
	return nil
}
