// Package store persists the accounts ccdad manages.
//
// Identity has four axes and three roles. uuid is the durable primary key: it
// survives an email change and a reordering, and it is what --json and exports
// carry. idx is a display ordinal for typing and for TUI hotkeys. email is the
// human label and may legitimately repeat across organizations. alias is an
// optional user handle.
//
// idx is deliberately NOT a key. cswap keys its slots by number and has to move
// backups, aliases and session directories every time two accounts swap places;
// keying on uuid removes that whole class of migration work.
package store

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

// Account is one managed Claude Code account.
type Account struct {
	// UUID is account.uuid from the profile endpoint. The primary key, and the
	// name of the account's credential file, so it is charset-checked on the
	// way in — see ValidateUUID.
	UUID string `toml:"uuid"`
	// Email labels the account for humans.
	Email string `toml:"email"`
	// Alias is an optional short handle, unique across accounts. Stored in its
	// normalized form; see NormalizeAlias.
	Alias string `toml:"alias,omitempty"`
	// Idx is the 1-based display ordinal. Stored rather than derived so it is
	// stable across restarts.
	Idx int `toml:"idx"`
	// Kind is how the account is metered. Callers set this one.
	Kind identity.Kind `toml:"-"`
	// KindName is Kind's serialized form, because TOML has no enum. It is
	// maintained by Save, which overwrites it from Kind on every write — a
	// caller that sets only KindName silently loses it.
	KindName string `toml:"kind"`
	// Tier is organization_type, e.g. claude_max.
	Tier string `toml:"tier,omitempty"`
	// RateLimitTier is rate_limit_tier, e.g. default_claude_max_20x.
	RateLimitTier string `toml:"rate_limit_tier,omitempty"`
	// OrganizationUUID is the owning organization, when the profile reported
	// one. The ambiguous-email error names it, so a user with the same address
	// in two organizations can tell the candidates apart.
	OrganizationUUID string `toml:"organization_uuid,omitempty"`
	// Disabled holds an account out of auto-rotation while keeping it a valid
	// explicit switch target.
	Disabled bool `toml:"disabled,omitempty"`
	// AddedAt is when ccdad first stored this account.
	AddedAt time.Time `toml:"added_at"`
	// Credit is the credit balance kept alongside the classification. An
	// account that is both — a subscription with overage enabled — classifies
	// as Subscription and keeps its credit balance as a SECONDARY axis,
	// because credits are not spent while a subscription window still has
	// room — but the engine still has to know what is there when that window
	// runs out.
	Credit CreditBalance `toml:"credit,omitempty"`
}

// CreditBalance is what a usage reading last said about an account's overage
// credits.
//
// The two figures are pointers because the credit gate branches on used_credits
// being nil and fails closed on money: a balance that could not be read must
// never persist as "$0 spent, full cap available". State is a string rather
// than a typed enum so that reading an accounts.toml stays free of the usage
// package; callers turn it back into one with usage.ParseExtraUsageState, which
// reads anything it does not recognize as unknown.
type CreditBalance struct {
	// State is usage.ExtraUsageState's name: enabled, disabled, blocked, or
	// empty for an account no reading has covered yet.
	State string `toml:"state,omitempty"`
	// DisabledReason is the organization's own word for a refusal, kept
	// verbatim because it is what a notification says out loud.
	DisabledReason string `toml:"disabled_reason,omitempty"`
	// Currency is the ISO code the figures below are denominated in, so that a
	// stored 150 is never ambiguous between $150 and ¥150.
	Currency string `toml:"currency,omitempty"`
	// MonthlyLimit is the account's own spend cap, in the MAJOR unit — dollars
	// for USD, the same unit max_auto_spend is written in. The endpoint reports
	// it in the minor unit; usage.ExtraUsage converts, and that conversion
	// happens in exactly one place. Nil means the account sets no cap —
	// unlimited — which is not the same as a cap of zero.
	MonthlyLimit *float64 `toml:"monthly_limit,omitempty"`
	// UsedCredits is what has already been spent, in the same major unit. Nil
	// means it could not be read, and that refuses a switch rather than reading
	// as zero.
	UsedCredits *float64 `toml:"used_credits,omitempty"`
	// ObservedAt is when the reading behind these figures was taken. The zero
	// time means no reading ever has been.
	ObservedAt time.Time `toml:"observed_at,omitempty"`
}

// Label is the account's best human name: the alias when it has one, else the
// email, else a short uuid.
func (a Account) Label() string {
	if a.Alias != "" {
		return a.Alias
	}
	if a.Email != "" {
		return a.Email
	}
	if len(a.UUID) > 8 {
		return a.UUID[:8]
	}
	return a.UUID
}
