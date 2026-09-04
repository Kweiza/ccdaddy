package codexproxy

import (
	"context"
	"net/http"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
)

// refreshTimeout bounds a grant exchange that has been detached from the
// client's request context, so a token endpoint that never answers cannot pin
// this goroutine -- and with it the refresher's per-account mutex -- forever.
//
// It is deliberately longer than the 30 s Timeout the refresher's own default
// http.Client carries: that client's timeout is what ends a stuck exchange in
// production, and this is only the backstop for a Refresher built around a
// client that has none.
const refreshTimeout = 60 * time.Second

// refreshVerdict is what one account's attempt settled.
type refreshVerdict int

const (
	// verdictAnswer: there is an attempt to answer with.
	verdictAnswer refreshVerdict = iota
	// verdictDead: this account needs a new login, and the refresher has
	// already marked it. Move on.
	verdictDead
	// verdictTransient: nothing was learned about the grant. Move on, and if
	// nothing else works say so rather than blaming the account.
	verdictTransient
)

// refresh is the single call into the daemon's shared refresher.
func (s *Server) refresh(ctx context.Context, uuid, triggeredBy string) (codexauth.Outcome, error) {
	if s.cfg.RefreshFunc != nil {
		return s.cfg.RefreshFunc(ctx, uuid, triggeredBy)
	}
	return s.cfg.Refresher.Refresh(ctx, uuid, triggeredBy)
}

func (s *Server) canRefresh() bool {
	return s.cfg.RefreshFunc != nil || s.cfg.Refresher != nil
}

// sendWithRefresh makes one attempt and, if the endpoint answers 401, repairs
// the token ONCE and replays.
//
// Once per REQUEST, not once per account, and refreshed is a pointer for that
// reason. codex answers a 401 with six requests over six seconds; if each of
// those, across each account, could spend a grant, one expired token would turn
// into a burst at the token endpoint -- and a refresh grant that is spent twice
// concurrently is how a login is lost rather than repaired.
//
// A 401 that survives the replay is passed back as an ANSWER. The endpoint has
// been asked with a token ccdad knows is current, so its refusal is a fact
// about the account rather than about ccdad's bookkeeping, and nothing here
// marks anything on the strength of it.
func (s *Server) sendWithRefresh(ctx context.Context, uuid string, in *http.Request, body []byte, stripTurnState bool, refreshed *bool) (*attempt, refreshVerdict, error) {
	a, err := s.send(ctx, uuid, in, body, stripTurnState)
	if err != nil {
		return nil, verdictAnswer, err
	}
	if a.status != http.StatusUnauthorized || *refreshed || !s.canRefresh() {
		return a, verdictAnswer, nil
	}
	*refreshed = true

	// The exchange runs on a context of this process's own, NEVER on the codex
	// client's.
	//
	// A Codex refresh token is single-use with server-side reuse detection: the
	// token endpoint burns the grant it was presented before it answers. If the
	// client hangs up while that answer is in flight -- the user presses Ctrl-C
	// or Esc, or codex abandons one of the six requests it answers a 401 with
	// over six seconds -- net/http cancels this handler's request context, the
	// POST is aborted, and the rotated pair is never read. codexauth.Classify
	// calls a cancelled context Transient, which is correct (nothing was
	// learned) and is exactly why nothing is written: the store keeps the grant
	// the issuer has just spent. The next exchange presents it, is
	// reuse-detected, goes Terminal, and the account needs `ccdad add codex` --
	// a login destroyed by a keystroke. The daemon lane never had this exposure
	// because it refreshes on the tick's context, which only a daemon stop
	// cancels.
	//
	// Only the exchange is detached. The attempts around it stay on ctx,
	// because a forwarded turn spends nothing single-use and a client that has
	// gone away should stop paying for one.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	out, rerr := s.refresh(rctx, uuid, a.token)
	if rerr != nil {
		s.logf("refreshing the codex token for %s did not complete: %v", short(uuid), rerr)
		return nil, verdictTransient, nil
	}
	switch out.Kind {
	case codexauth.Rotated, codexauth.Adopted:
		// The headers are rebuilt inside send, from the store, so the replay
		// carries the token the refresh produced and not the one that failed.
		retry, err := s.send(ctx, uuid, in, body, stripTurnState)
		if err != nil {
			return nil, verdictAnswer, err
		}
		return retry, verdictAnswer, nil
	case codexauth.Terminal:
		s.logf("codex account %s needs a new login: %s", short(uuid), out.Code)
		return nil, verdictDead, nil
	default:
		s.logf("the codex token refresh for %s settled nothing: %s", short(uuid), out.Code)
		return nil, verdictTransient, nil
	}
}
