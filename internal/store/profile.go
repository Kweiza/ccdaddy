package store

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

// ProfileTTL is how long a profile reading stands before a poll spends a
// request re-reading it.
//
// The number is COPIED from Claude Code rather than chosen. Its own cached
// profile is considered current while `Date.now()-profileFetchedAt < XH`, and
// `XH=86400000` in 2.1.246 -- one day. Picking a different figure here would
// mean the two programs re-read the same endpoint on different schedules for
// the same account, which is a second poller nobody asked for.
const ProfileTTL = 24 * time.Hour

// ProfileStale reports whether this account's profile-derived fields are old
// enough to be worth a request.
//
// The ZERO VALUE READS STALE, and that is the whole point: an account added
// before ProfileFetchedAt existed carries it, and that account is the one whose
// SeatTier is missing. A build that treated "never measured" as fresh would
// leave exactly the broken population unrepaired.
func (a Account) ProfileStale(now time.Time) bool {
	if a.ProfileFetchedAt.IsZero() {
		return true
	}
	// A stamp in the future is a clock that moved, not a reading from ahead of
	// time. Reading it as fresh would freeze the account until real time caught
	// up with the bad stamp, which for a badly set clock is never; reading it
	// as stale costs one request and re-stamps it correctly.
	if a.ProfileFetchedAt.After(now.Add(ProfileTTL)) {
		return true
	}
	return now.Sub(a.ProfileFetchedAt) >= ProfileTTL
}

// AdoptProfile writes the fields ONE profile lookup decides, and stamps when it
// happened. It is the single writer of those fields: `ccdad add` and the
// daemon's poll both reach them through here, so the two cannot drift into
// disagreeing about which parts of a profile are allowed to land.
//
// IT DELIBERATELY DOES NOT TOUCH Kind. Kind belongs to the USAGE axis --
// ApplyUsage revises it through identity.ReclassifyOnUsage, which calls
// Classify with a nil profile and the real UsageShape. Add-time classification
// is the mirror image: a real profile and an EMPTY shape, because no usage call
// has been made yet. Re-running that here would overwrite a classification made
// on window-and-overage evidence with the guess that preceded it, and it would
// do so on every poll. The caller that legitimately decides Kind from a profile
// is `ccdad add`, once, and it does that beside this call rather than through
// it.
//
// An EMPTY field on the wire is written as empty. That is not the same as the
// nil-profile case its callers reject: a profile that answers seat_tier null is
// evidence of none -- every pro and max account measured answers exactly that
// -- and an organization can move a seat off usage-based metering, which has to
// be able to clear the value.
func (a *Account) AdoptProfile(p *identity.Profile, at time.Time) {
	if p == nil {
		return
	}
	a.Tier = p.OrganizationType
	a.RateLimitTier = p.RateLimitTier
	a.SeatTier = p.SeatTier
	a.OrganizationUUID = p.OrganizationUUID
	a.ProfileFetchedAt = at
}

// ApplyProfile records a fresh profile reading against one stored account. It
// is the sibling of ApplyUsage and it is deliberately shaped the same way: a
// reading is required, the account is mutated in the store's own slice, and a
// missing account is ErrNotFound rather than a silent no-op.
//
// A nil profile is refused rather than ignored, for the reason ApplyUsage
// refuses a nil snapshot: the caller has a failed lookup on its hands and needs
// to be told, and writing "no evidence" over four fields that currently hold
// measured values would turn one unreachable endpoint into a fleet that forgot
// its tiers.
func (s *Store) ApplyProfile(uuid string, p *identity.Profile, observedAt time.Time) error {
	if p == nil {
		return fmt.Errorf("ApplyProfile needs a profile; a lookup that failed must leave %q's tiers as they are", uuid)
	}
	return s.mutate(func() error { return s.applyProfile(uuid, p, observedAt) })
}

func (s *Store) applyProfile(uuid string, p *identity.Profile, observedAt time.Time) error {
	// The store's OWN slice, for the reason applyUsage states: Accounts() hands
	// back a defensive copy and a change made to one never reaches the disk.
	for i := range s.data.Accounts {
		if s.data.Accounts[i].UUID == uuid {
			s.data.Accounts[i].AdoptProfile(p, observedAt)
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, uuid)
}
