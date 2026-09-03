package switcher

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// EvaluateCodex is the Codex lane's own pass over the store and the usage cache.
//
// IT IS NOT A LOOP AROUND Evaluate, and the reason is engineCandidates: its
// first line skips any account whose blob is not Installable, and a codex
// credential never is -- so every Codex account would be dropped before any
// provider check ran, and the lane would silently rank an empty pool forever.
//
// Three things differ from the Claude pass, and each is a decision:
//
//   - The BASELINE is the serving pointer, never the account Claude Code is
//     logged in as. A baseline that is not among the candidates makes
//     cooldownGate's no-baseline arm fire on every pass, which turns the
//     cooldown into a no-op and the lane into a repoint on every tick.
//   - PreemptLead is forced to zero and Hover to false AFTER RankOptions, so a
//     user's Claude tuning cannot switch on for codex two mechanisms this
//     version does not implement for it: there is no forecast for a Codex
//     window and no hover table over a two-window snapshot.
//   - The cooldown comes from the codex stamp, through State.ForProvider.
//
// opts.Provider is accepted for readability at the call sites and is not read:
// this function ranks Codex accounts and nothing else.
//
// book may be nil, which means "nothing is known to be throttled". An attended
// `ccdad switch` has no proxy in its process and passes nil.
func EvaluateCodex(s *store.Store, root string, opts EvalOptions, book *codexproxy.LimitBook) (Evaluation, error) {
	var ev Evaluation
	accounts := s.CodexAccounts()
	if len(accounts) == 0 {
		return ev, nil
	}
	now := opts.now()

	serving, hasServing := codexswitch.ReadServing(root)
	if hasServing {
		// A pointer naming an account the store no longer has reads as no
		// pointer. That is the same answer the proxy gives it, and it is what
		// stops `ccdad remove` leaving a baseline nothing can rank.
		if a, ok := s.Get(serving); ok && a.Provider == provider.Codex {
			ev.Live, ev.LiveKnown = a, true
		} else {
			serving, hasServing = "", false
		}
	}
	// LiveState is deliberately left at its zero. It answers what Claude Code's
	// credentials file holds, and that question has no Codex meaning at all --
	// filling it in with something plausible would give a reader an answer
	// nobody measured.

	cache, err := usage.LoadCache()
	if err != nil {
		return ev, err
	}
	added := make(map[string]time.Time, len(accounts))
	for _, a := range accounts {
		added[a.UUID] = a.AddedAt
	}
	cache.Prune(added)

	cands := codexCandidates(s, accounts, cache, book, serving, now)
	if !anyReading(cands) {
		ev.NoReadings = true
		return ev, nil
	}

	st, err := strategy.LoadState()
	if err != nil {
		return ev, err
	}
	ev.StateErr = st.LoadError()
	codexState := st.ForProvider(provider.Codex)
	ev.LastSwitchAt, ev.LastSwitchTo = codexState.LastSwitch()

	cfg, cerr := opts.config()
	ev.ConfigErr = cerr

	o := cfg.RankOptions(now)
	// AFTER RankOptions, and that order is the point: RankOptions carries the
	// user's PreemptLead and Hover, and both are Claude-only mechanisms here.
	o.PreemptLead = 0
	o.Hover = false
	o.Model = ""
	o.Threshold = codexThreshold(cfg)

	ev.Plan, ev.Decided = strategy.Decide(cands, o, cfg.StrategyConfig(), codexState, servingBaseline(serving, hasServing)), true

	if ev.Plan.Action == strategy.ActionSwitch {
		target, ok := s.Get(ev.Plan.Target.UUID)
		if !ok {
			return ev, fmt.Errorf("the codex lane chose %q, which is no longer in the store", ev.Plan.Target.UUID)
		}
		ev.Target, ev.HasTarget = target, true
	}
	return ev, nil
}

// servingBaseline is the uuid Decide holds its margins against, or "" when
// there is no pointer -- which Decide reads as "no baseline", the state where
// moving beats waiting.
func servingBaseline(serving string, has bool) string {
	if !has {
		return ""
	}
	return serving
}

// codexThreshold is the utilization percent above which a Codex account counts
// as spent. It is its own key because the two providers meter different things:
// a ChatGPT plan's five-hour and weekly windows are not a Claude plan's, and one
// number for both would tune each by tuning the other.
func codexThreshold(cfg config.Config) float64 {
	if cfg.Codex.Threshold > 0 {
		return cfg.Codex.Threshold
	}
	return cfg.Threshold
}

// codexCandidates is the Codex lane's own candidate builder.
//
// The eligibility rule is: enabled, not driven by another machine, holding a
// codex credential, and not marked as needing a login. A fifth term is applied
// here that the rule does not name -- an account the shared limit book still
// shows throttled is not a repoint target -- because pointing at one produces a
// 429 on the very first new thread. The account that is ALREADY serving is
// exempt from that term: dropping it would take the baseline away, and losing
// the cooldown at the moment a 429 makes churn most expensive is the opposite of
// what the throttle should cause.
func codexCandidates(s *store.Store, accounts []store.Account, c *usage.Cache,
	book *codexproxy.LimitBook, serving string, now time.Time) []strategy.Candidate {

	out := make([]strategy.Candidate, 0, len(accounts))
	for _, a := range accounts {
		if a.Disabled || a.Elsewhere {
			continue
		}
		creds, err := s.Credentials(a.UUID)
		if err != nil {
			continue
		}
		if _, ok := creds[codexauth.Key]; !ok {
			continue
		}
		if codexauth.NeedsRelogin(a, creds) {
			continue
		}
		if until, limited := book.LimitedUntil(a.UUID, now); limited && a.UUID != serving {
			_ = until
			continue
		}
		cand := strategy.Candidate{UUID: a.UUID, Kind: a.Kind, Primary: a.Primary}
		if e, ok := c.Get(a.UUID); ok {
			cand.Usage = e.Snapshot
			cand.FetchedAt, cand.NextPollAt = e.FetchedAt, e.NextPollAt
			cand.LastRateLimited = e.Poll.LastRateLimited
		}
		out = append(out, cand)
	}
	return out
}
