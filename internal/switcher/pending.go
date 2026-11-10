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
// viewAt is when v was read. An intent recorded AFTER it is one v knows
// nothing about -- a switch that started writing after the read -- and is left
// alone; and what is cleared or completed is exactly the intent read here,
// checked again under the state lock, never a later one recorded meanwhile.
func SettlePending(s *store.Store, v LoginView, viewAt, now time.Time) (PendingSettlement, string, error) {
	st, err := strategy.LoadState()
	if err != nil {
		return PendingOpen, "", err
	}
	to, left, ok := st.Pending()
	at := st.PendingAt()
	if !ok || at.After(viewAt) || v.StoresDisagree() || v.Unresolved() {
		return PendingOpen, to, nil
	}
	lead := v.Lead()
	if lead.State != LiveManaged {
		return PendingOpen, to, nil
	}
	if lead.Account.UUID != to {
		err := strategy.WithState(stateTimeout, func(st *strategy.State) error {
			st.ClearPendingIf(to, at)
			return nil
		})
		return PendingNotTaken, to, err
	}
	if !v.StoresOn(to) {
		return PendingOpen, to, nil
	}
	completed := false
	err = strategy.WithState(stateTimeout, func(st *strategy.State) error {
		if st.IsPending(to, at) {
			st.RecordSwitch(to, now, left)
			completed = true
		}
		return nil
	})
	if err != nil {
		return PendingOpen, to, fmt.Errorf("completing the switch to %s: %w", to, err)
	}
	if !completed {
		return PendingOpen, to, nil
	}
	if err := s.SetActive(to); err != nil {
		return PendingCompleted, to, err
	}
	return PendingCompleted, to, nil
}
