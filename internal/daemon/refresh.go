package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// RefreshState is what one account's hand-held refresh did.
type RefreshState int

const (
	// RefreshFetched: the endpoint answered and the reading is in the cache.
	RefreshFetched RefreshState = iota
	// RefreshCached: a reading younger than serveTTL was served as it stood.
	RefreshCached
	// RefreshHeld: a 429's floor is still in force.
	RefreshHeld
	// RefreshUnpollable: there is no OAuth grant to poll with.
	RefreshUnpollable
	// RefreshFailed: the attempt was made and did not produce a reading.
	RefreshFailed
)

func (s RefreshState) String() string {
	switch s {
	case RefreshFetched:
		return "fetched"
	case RefreshCached:
		return "cached"
	case RefreshHeld:
		return "held"
	case RefreshUnpollable:
		return "unpollable"
	case RefreshFailed:
		return "failed"
	}
	return "unknown"
}

// RefreshResult is one account's outcome.
type RefreshResult struct {
	Account store.Account
	State   RefreshState
	// At is when the endpoint may next be reached for this account. It is set
	// for the two states that are a WAIT rather than an answer — RefreshCached
	// and RefreshHeld — so the caller can say how long, instead of only that.
	At  time.Time
	Err error
}

// Refresh is `ccdad list --refresh`: one pass over `want`, taking a reading for
// every account allowed to have one taken, and returning when they are all in.
//
// It is the SAME poller the daemon's tick dispatches — the same token source,
// the same commit, the same poll-policy cadence written back into the same
// cache — and that is the point rather than an economy. Two implementations of
// "record a poll" would compute two schedules, and the promise that `list` and
// `status --json` can never disagree only survives while one number has one
// writer.
//
// What differs from the tick is the GATE, and only the gate. A tick polls what
// its own cadence says is due; a hand pressing a button is not on a cadence, so
// the poll policy holds it to serveTTL and to whatever floor a 429 has earned,
// and to nothing else. Honouring nextPollAt as well would make the flag useless
// in the one situation it exists for — no daemon running, a reading four
// minutes old, and a candidate's ten-minute cadence with nothing to advance it.
//
// The caller's Engine must not be ticking. Nothing here takes the in-flight
// claim, because the CLI's Engine only ever runs this: a second poller inside
// ONE process is a case the CLI cannot produce, and a branch nothing can reach
// is worse than no branch. Across processes there is no claim to take anyway —
// the daemon is a different process, serveTTL keeps the overlap to a sliver,
// and the cache's own cross-process lock is what makes the two writes safe.
func (e *Engine) Refresh(ctx context.Context, s *store.Store, want []store.Account,
	cfg config.Config, active string) []RefreshResult {

	out := make([]RefreshResult, len(want))
	for i, a := range want {
		out[i].Account = a
	}

	cache, err := usage.LoadCache()
	if err != nil {
		// Unreachable from a machine whose store opened at all — this is the
		// same root store.Open resolved — but every row genuinely did fail, and
		// silently reporting them as cached would be a lie about a fetch.
		for i := range out {
			out[i].State, out[i].Err = RefreshFailed, err
		}
		return out
	}

	now := e.now()
	// Sized over EVERY account, not over `want`. The allowance belongs to the
	// identity, so an account left out of the listing still draws on it.
	members := identityMembers(s.Accounts())

	var wg sync.WaitGroup
	for i := range want {
		a, res := want[i], &out[i]

		// The Codex arm, AHEAD of the Claude gate rather than folded into it.
		// pollable asks for a claudeAiOauth key and a Codex account never has
		// one, so a Codex row reaching that line comes back unpollable forever
		// -- and this command is the one a user reaches for precisely when no
		// daemon is running to have read the account instead.
		if a.Provider == provider.Codex {
			e.refreshCodex(ctx, s, a, res, cache, cfg, now, &wg)
			continue
		}
		if !pollable(s, a) {
			res.State = RefreshUnpollable
			continue
		}
		entry, has := cache.Get(a.UUID)
		if has && entry.Fresh(now) {
			res.State, res.At = RefreshCached, entry.FetchedAt.Add(usage.ServeTTL)
			continue
		}
		// Measured from the 429 and not from the reading, which is why it is
		// asked here rather than read off the entry's age: commit deliberately
		// leaves FetchedAt alone on a failed poll (an account that could not be
		// read is unknown, not empty, so its last good reading stands), and an
		// age-based hold would therefore lapse the moment it was earned.
		if at, held := pollpolicy.RateLimitedUntil(pollStateOf(entry), now); held {
			res.State, res.At = RefreshHeld, at
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			// The configured bundle, not the tick's hoverThresholds: a
			// hand-held refresh has no switcher.Evaluation to derive hover's
			// per-account table from — asking for one would be the ranking
			// pass this button is not a substitute for — so it measures the
			// same way `ccdad auto`'s own poll does whenever it cannot ask.
			if perr := e.poll(ctx, a, cfg, configuredThresholds(cfg), members[identityOf(a)], a.UUID == active); perr != nil {
				res.State, res.Err = RefreshFailed, perr
				return
			}
			res.State = RefreshFetched
		}()
	}
	wg.Wait()
	return out
}
