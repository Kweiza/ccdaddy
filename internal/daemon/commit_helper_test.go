package daemon

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// commit is commitReading with its input spelled out, the form these tests
// state one poll's outcome in.
func (e *Engine) commit(a store.Account, snap *usage.Snapshot, now, sent time.Time,
	identity []string, thresholds func(uuid string) strategy.Thresholds, active bool,
	adjust func(pollpolicy.State) pollpolicy.State) {
	e.commitReading(commitInput{
		account: a, snap: snap, adjust: adjust, now: now, sent: sent,
		identity: identity, thresholds: thresholds, active: active,
	})
}
