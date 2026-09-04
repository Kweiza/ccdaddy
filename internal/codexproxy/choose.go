package codexproxy

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// threadIDHeader is the header codex puts a thread's identity in.
const threadIDHeader = "thread-id"

// threadIDOf names the thread a request belongs to, or "" for a request that
// does not say.
func threadIDOf(r *http.Request) string { return r.Header.Get(threadIDHeader) }

// chooseOrder decides which accounts a request may be served from, best first,
// and whether the choice is a launch pin.
//
// The precedence is not arbitrary. The HTTP path is stateless: every request
// carries the whole history, including the encrypted reasoning items produced
// by whichever account served the earlier turns of the thread. So an account
// that has already answered inside a thread is the one that can read what that
// thread carries, and moving a live thread is a decision with a cost rather
// than a free rotation.
//
//  1. the launch pin, when the launcher bound this codex to one account. It
//     never falls through: a pin that billed a second account would make
//     `ccdad run <acct>` a suggestion.
//  2. the thread pin, from the first successful response of this thread.
//  3. the serving pointer, read from the file on EVERY request. That is what
//     makes repointing apply to new threads and leave live ones alone.
//
// The pin and the pointer are not filtered by eligibility. Disabled is a
// rotation policy and not a per-request gate: an account the user pointed at
// keeps serving, and the lane rotates away on its next decision.
func (s *Server) chooseOrder(rec codexlaunch.Record, threadID string) ([]string, bool) {
	if rec.Pin != "" {
		return []string{rec.Pin}, true
	}
	ranked := s.rankedEligible()
	if threadID != "" {
		if uuid, ok := s.threadAccount(threadID); ok {
			return lead(uuid, ranked), false
		}
	}
	if uuid, ok := codexswitch.ReadServing(s.cfg.Root); ok && s.stored(uuid) {
		return lead(uuid, ranked), false
	}
	return ranked, false
}

// lead puts uuid at the front and keeps the rest of the ranking behind it.
func lead(uuid string, ranked []string) []string {
	out := make([]string, 0, len(ranked)+1)
	out = append(out, uuid)
	for _, candidate := range ranked {
		if candidate != uuid {
			out = append(out, candidate)
		}
	}
	return out
}

// stored reports whether the account store still has this uuid. A pointer
// naming an account somebody removed reads as no pointer at all.
func (s *Server) stored(uuid string) bool {
	rows, err := s.accounts()
	if err != nil {
		return false
	}
	for _, a := range rows {
		if a.UUID == uuid {
			return true
		}
	}
	return false
}

// rankedEligible is the lane's ranking when there is one.
//
// The fallback is not a nicety: a daemon that has just started has run no lane
// tick yet, and a proxy that answered "no accounts" for the first fifteen
// minutes of every daemon's life would be indistinguishable from a broken one.
func (s *Server) rankedEligible() []string {
	if s.cfg.RankedEligible != nil {
		if order := s.cfg.RankedEligible(); len(order) > 0 {
			return order
		}
	}
	rows := s.eligible()
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.UUID)
	}
	return out
}

// eligible is the store-derived candidate set: a Codex account this machine
// drives, that the user has not held out, that has a stored Codex credential,
// and whose credential is not the one a refusal already named.
func (s *Server) eligible() []store.Account {
	rows, err := s.accounts()
	if err != nil {
		s.logf("the codex proxy could not read the account store: %v", err)
		return nil
	}
	var out []store.Account
	for _, a := range rows {
		if a.Provider != provider.Codex || a.Disabled || a.Elsewhere {
			continue
		}
		cred, err := s.credential(a.UUID)
		if err != nil {
			continue
		}
		if a.NeedsRelogin(codexauth.RefreshTokenHash(cred.RefreshToken)) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (s *Server) accounts() ([]store.Account, error) {
	if s.cfg.Accounts == nil {
		return nil, errors.New("the codex proxy was given no account reader")
	}
	return s.cfg.Accounts()
}

// credential reads one account's Codex credential.
func (s *Server) credential(uuid string) (codexauth.Credential, error) {
	if s.cfg.Credentials == nil {
		return codexauth.Credential{}, errors.New("the codex proxy was given no credential reader")
	}
	blob, err := s.cfg.Credentials(uuid)
	if err != nil {
		return codexauth.Credential{}, err
	}
	cred, ok, err := codexauth.FromBlob(blob)
	if err != nil {
		return codexauth.Credential{}, err
	}
	if !ok {
		return codexauth.Credential{}, fmt.Errorf("account %s has no stored codex credential", short(uuid))
	}
	return cred, nil
}

func (s *Server) threadAccount(threadID string) (string, bool) {
	if threadID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	uuid, ok := s.threads[threadID]
	return uuid, ok
}

// rememberThread binds a thread to the account that produced its first
// response.
func (s *Server) rememberThread(threadID, uuid string) {
	if threadID == "" || uuid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[threadID] = uuid
}

// short is the uuid prefix the log uses. A full uuid is not a secret, but the
// daemon log is read by eye and eight characters is what every other line here
// carries.
func short(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8]
	}
	return uuid
}
