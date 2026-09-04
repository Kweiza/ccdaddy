package codexproxy

import (
	"net/http"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// outOfAccounts is the answer when every account the request could have used
// has been tried and none of them served it.
//
// The candidates partition into two states that need OPPOSITE answers, and
// giving one answer for both is what made this worth writing down:
//
//   - WAITING is an account with quota that has not come back yet. codex should
//     wait, so the answer is a 429 carrying the earliest reset among them --
//     and carrying NOTHING when no endpoint stated one, because codex renders
//     an absent value as "try again later" and a zero as 1970.
//   - DEAD is an account the search marked dead: its refresh grant was refused,
//     or it has no usable Codex credential at all. Waiting will not fix either,
//     so the answer names the account and the command.
//
// An account the search could not reach for a THIRD reason -- the upstream
// refused the connection, the context was cancelled -- is in neither set. The
// caller keeps that case for itself, because "the network is down" must never
// be rendered as "log in again".
//
// Waiting wins when both are present: an account that comes back in an hour is
// a better thing to tell a user about than one that needs a login they may not
// be able to do right now.
//
// Neither status is ever 5xx, and the last arm is the reason this function
// cannot fall through: a store that answered nothing at all still has to
// produce an answer codex can act on.
func (s *Server) outOfAccounts(w http.ResponseWriter, order, dead []string) {
	now := s.now()
	rows, err := s.accounts()
	if err != nil {
		s.logf("the codex proxy could not read the account store while answering an exhausted request: %v", err)
		writeUnavailable(w)
		return
	}
	byUUID := make(map[string]store.Account, len(rows))
	for _, a := range rows {
		byUUID[a.UUID] = a
	}
	deadSet := make(map[string]bool, len(dead))
	for _, uuid := range dead {
		deadSet[uuid] = true
	}

	// WAITING is read off the whole candidate order, because any of them coming
	// back is a reason to wait.
	var (
		waiting   bool
		earliest  time.Time
		haveReset bool
	)
	for _, uuid := range order {
		if _, limited := s.book.LimitedUntil(uuid, now); limited && !deadSet[uuid] {
			waiting = true
			if at, ok := s.book.ResetsAt(uuid, now); ok && (!haveReset || at.Before(earliest)) {
				earliest, haveReset = at, true
			}
		}
	}
	// DEAD is read ONLY off what the caller marked dead. Deriving it from "not
	// rate-limited" instead would name an account every time the network was
	// down, and tell the user to re-authenticate something that is fine.
	var (
		firstDead store.Account
		haveDead  bool
	)
	for _, uuid := range dead {
		if a, ok := byUUID[uuid]; ok {
			firstDead, haveDead = a, true
			break
		}
	}

	switch {
	case waiting:
		writeWaiting(w, earliest, haveReset)
	case haveDead:
		writeNeedsRelogin(w, firstDead.Label())
	default:
		writeUnavailable(w)
	}
}
