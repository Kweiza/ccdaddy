package switcher

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// PendingSettlement is what SettlePending found.
type PendingSettlement uint8

const (
	// PendingOpen: nothing pending, or the stores do not yet say how it went
	// -- they disagree, and the repair carries it out.
	PendingOpen PendingSettlement = iota
	// PendingCompleted: both stores hold the target. The switch took effect
	// with nothing left to write, and it has been recorded as the switch it
	// was: the account as live, the cooldown, and the departure.
	PendingCompleted
	// PendingNotTaken: both stores hold another account. The switch did not
	// take effect, and it is forgotten.
	PendingNotTaken
)

// SettlePending closes out ccdad's own switch whose keychain write could not
// be confirmed (strategy.State.Pending), once the stores agree and so no
// repair will.
//
// Agreeing on the TARGET is not "nothing to do". The plan that made the switch
// left an account behind -- possibly at a limit, which is what its departure
// records -- and the cooldown is what holds the engine off moving straight
// back. A switch whose file write landed through some other path (the file
// already held the target, say) would otherwise be recorded by nobody, and
// only the record would be corrected.
//
// The store lock is taken for the record and the state lock for the switch,
// in that order, as Execute takes them; no credential lock is involved.
func SettlePending(s *store.Store, v LoginView, now time.Time) (PendingSettlement, string, error) {
	st, err := strategy.LoadState()
	if err != nil {
		return PendingOpen, "", err
	}
	to, left, ok := st.Pending()
	if !ok || v.StoresDisagree() || v.Unresolved() {
		return PendingOpen, to, nil
	}
	lead := v.Lead()
	if lead.State != LiveManaged {
		return PendingOpen, to, nil
	}
	if lead.Account.UUID != to {
		err := strategy.WithState(stateTimeout, func(st *strategy.State) error {
			st.ClearPending()
			return nil
		})
		return PendingNotTaken, to, err
	}
	if !v.StoresOn(to) {
		return PendingOpen, to, nil
	}
	if err := s.SetActive(to); err != nil {
		return PendingOpen, to, err
	}
	if err := RecordSwitchAt(to, now, left); err != nil {
		return PendingOpen, to, fmt.Errorf("completing the switch to %s: %w", to, err)
	}
	return PendingCompleted, to, nil
}
