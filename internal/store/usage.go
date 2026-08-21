package store

import (
	"fmt"

	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// ApplyUsage records a SUCCESSFUL usage reading against an account: it revises
// the account's Kind if the reading is evidence, and refreshes the credit
// balance §5 keeps as a secondary axis.
//
// Taking a non-nil *usage.Snapshot is the whole point of the signature. Both
// production Classify calls pass an empty UsageShape today, so the window
// evidence §5 makes primary has never once been consulted — and the fix must not
// be a function that can be handed "no reading" and quietly apply Classify's
// no-evidence default. A failed poll has nothing to pass here, and passing nil
// is refused rather than treated as an empty reading.
//
// The read-modify-write is not multi-process safe. That is a pre-existing
// property of Store, documented on the type, and the store-level cross-process
// lock is its own queue item (ccdad/task-19-store-cross-process-lock) whose file
// this deliberately does not pre-empt. Nothing here widens the window.
func (s *Store) ApplyUsage(uuid string, snap *usage.Snapshot, observedAt time.Time) error {
	if snap == nil {
		return fmt.Errorf("ApplyUsage needs a reading; a poll that failed must leave %q classified as it is", uuid)
	}

	// The account has to be mutated in the store's OWN slice. Accounts() hands
	// back a defensive copy, and Save() rewrites KindName from Kind on every
	// write, so a change made to either of those is a change that never reaches
	// the disk.
	idx := -1
	for i := range s.data.Accounts {
		if s.data.Accounts[i].UUID == uuid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, uuid)
	}

	shape := identity.UsageShape{
		HasSubscriptionWindows: snap.HasSubscriptionWindows(),
		// Only overage actually switched on is credit evidence. A subscription
		// organization with the overage switch available but off reports
		// is_enabled false, and reading that as a credit axis would file every
		// Max account on the far side of the credit gate.
		ExtraUsageEnabled: snap.ExtraUsage.State == usage.ExtraUsageEnabled,
	}
	// Unconditional on purpose: ReclassifyOnUsage hands back the CURRENT kind
	// when the reading is not evidence, so there is nothing to guard here. The
	// bool it also returns is for callers that want to announce a change; this
	// one does not.
	s.data.Accounts[idx].Kind, _ = identity.ReclassifyOnUsage(s.data.Accounts[idx].Kind, shape)
	s.data.Accounts[idx].Credit = creditBalanceOf(snap.ExtraUsage, observedAt)

	return s.Save()
}

// creditBalanceOf projects a reading's credit axis onto what the store keeps.
// The figures come out of the accessors, so they are in the currency's MAJOR
// unit — the one max_auto_spend is written in — and the currency is recorded
// alongside them so the number is never ambiguous.
// An account whose reading carried no extra_usage at all records an empty
// balance rather than a zeroed one: "no credit axis" and "a credit axis worth
// nothing" are different facts, and only one of them is safe to spend against.
func creditBalanceOf(e usage.ExtraUsage, observedAt time.Time) CreditBalance {
	if !e.Present {
		return CreditBalance{ObservedAt: observedAt}
	}
	b := CreditBalance{
		State:          e.State.String(),
		DisabledReason: e.DisabledReason,
		Currency:       e.Currency,
		ObservedAt:     observedAt,
	}
	if limit, ok := e.MonthlyLimit(); ok {
		b.MonthlyLimit = &limit
	}
	if used, ok := e.UsedCredits(); ok {
		b.UsedCredits = &used
	}
	return b
}
