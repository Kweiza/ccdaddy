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
	// way in — see validUUID.
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
