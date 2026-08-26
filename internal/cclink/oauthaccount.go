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

// AccountIdentity is what ResetOAuthAccountIdentity writes. It is a struct
// rather than a run of positional strings because the fields are all strings
// and three of them are organization-shaped: a caller that transposed SeatTier
// and RateLimitTier would compile silently and write a literal Claude Code
// compares by value into a file it trusts, which is the exact class of fault
// this object exists to prevent.
type AccountIdentity struct {
	// UUID is accountUuid. Everything else is optional; this is not.
	UUID string
	// Email is emailAddress.
	Email string
	// OrganizationUUID is organizationUuid.
	OrganizationUUID string
	// OrganizationType is organizationType, and it is the RAW wire value
	// (claude_max, claude_enterprise), not the short name. Claude Code writes
	// the raw value here itself; the short name lives in the credentials
	// file's subscriptionType, which is a different field in a different file.
	OrganizationType string
	// RateLimitTier is organizationRateLimitTier.
	RateLimitTier string
	// SeatTier is seat_tier: "enterprise_usage_based" for a money-metered
	// seat, empty for everything else. A pro or max account answers
	// seat_tier null, so empty here means "the wire reported none" and not
	// "ccdad did not look" -- both are written as an ABSENT key, which is
	// what Claude Code's own `??null` already reads as unknown.
	SeatTier string
}

// ResetOAuthAccountIdentity replaces oauthAccount with a MINIMAL object
// naming only who the account is -- accountUuid, emailAddress, the
// organization fields ccdad already has from the profile lookup it made when
// the account was added, and the seat tier.
//
// SEATTIER IS THE ONE FIELD HERE CLAUDE CODE READS TO DECIDE BEHAVIOUR rather
// than to display. `Zu(){return Xe()==="enterprise"&&dO()==="enterprise_usage_based"}`
// is its test for a money-metered enterprise seat, and the seat half of it
// reads this file: `dO(){return Dn()?.seatTier??null}` over
// `Dn(){return Zt()?k().oauthAccount:void 0}` (2.1.246). claudeAiOauth has no
// seatTier key -- its eight keys are fixed, see token.go -- so oauthAccount is
// the only place a switch can put one. Omitting it drops such a seat out of
// the Opus tier `cE()` grants it beside max and team-5x until Claude Code's
// own next token refresh repairs the field, which happens no earlier than five
// minutes before the access token expires.
//
// It deliberately OMITS billingType, accountCreatedAt, subscriptionCreatedAt,
// and ccOnboardingFlags rather than leaving them at a previous account's
// values or guessing new ones. Claude Code's own token-refresh handler
// treats their combined presence as "the cached profile is already
// complete" and skips re-fetching it entirely -- in 2.1.246 that gate is
// `oauthAccount.billingType!==void 0 && .accountCreatedAt!==void 0 &&
// .subscriptionCreatedAt!==void 0 && .ccOnboardingFlags!==void 0` together
// with the credentials file's subscriptionType and rateLimitTier being
// non-null. Omitting the four is what makes Claude Code fetch the real values
// itself on its next refresh instead of ccdad forging them. SEATTIER IS NOT IN
// THAT GATE, so writing it does not suppress the re-fetch -- and the re-fetch,
// when it comes, sets seatTier from the profile it just read, so ccdad's value
// is a stand-in until Claude Code measures it again rather than a claim that
// outlives the evidence.
func ResetOAuthAccountIdentity(g *GlobalConfig, id AccountIdentity) error {
	if id.UUID == "" {
		return errors.New("refusing to write an oauthAccount identity with no account uuid")
	}
	identity := struct {
		AccountUUID               string `json:"accountUuid"`
		EmailAddress              string `json:"emailAddress,omitempty"`
		OrganizationUUID          string `json:"organizationUuid,omitempty"`
		OrganizationType          string `json:"organizationType,omitempty"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier,omitempty"`
		SeatTier                  string `json:"seatTier,omitempty"`
	}{
		AccountUUID:               id.UUID,
		EmailAddress:              id.Email,
		OrganizationUUID:          id.OrganizationUUID,
		OrganizationType:          id.OrganizationType,
		OrganizationRateLimitTier: id.RateLimitTier,
		SeatTier:                  id.SeatTier,
	}
	encoded, err := marshalNoEscape(identity)
	if err != nil {
		return err
	}
	g.Set(oauthAccountKey, encoded)
	return nil
}
