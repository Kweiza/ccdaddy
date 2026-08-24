package daemon

import (
	"context"
	"encoding/json"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// liveVerdict is what resolving an unnameable live login established.
//
// The three values are the three answers, and the reason this type exists is
// that the middle one used to be missing. A credentials file that matches no
// stored snapshot was read as "nobody is live", which is the answer for an
// EMPTY file and the opposite of the answer for a managed account whose refresh
// token Claude Code has just rotated.
type liveVerdict uint8

const (
	// liveUnresolved: the owner could not be established. Indistinguishable
	// from a managed account mid-rotation, so it is treated as one.
	liveUnresolved liveVerdict = iota
	// liveAdopted: the owner is a managed account, and the rotated pair has
	// been taken back into its stored snapshot. Attribution matches again.
	liveAdopted
	// liveForeign: the owner is an account this store does not manage. There
	// is nothing of ours under the file, so a swap may proceed.
	liveForeign
)

// resolveLive answers who owns the login in the credentials file when the file
// itself cannot say, and repairs the store when the answer is one of ours.
//
// This is the identity ORACLE, and it is a network call because nothing local
// can answer. The refresh token is the anchor attribution compares on, the
// server rotates it whenever anything refreshes, and after a rotation the file
// and the store hold two generations of one lineage that share no field. The
// endpoint is what turns "these bytes match nothing" back into a name.
//
// Adoption is identity-GUARDED, and that guard is the whole safety argument.
// Writing the live blob into whichever account ccdad last switched to would be
// the cheap version and it is the one that destroys an account: if the user ran
// /login inside Claude Code, the live file holds somebody else's grant, and
// storing it over a managed account's snapshot overwrites that account's only
// refresh token with a stranger's. Only a uuid the endpoint itself returned may
// name the account being written.
//
// A failure to reach the endpoint answers liveUnresolved, never liveForeign.
// Offline is not evidence that the login belongs to somebody else, and the
// direction to fail in is the one that leaves a running session alone.
func (e *Engine) resolveLive(ctx context.Context, s *store.Store) (store.Account, liveVerdict) {
	if e.ResolveOwner == nil {
		return store.Account{}, liveUnresolved
	}
	live, err := cclink.Load()
	if err != nil {
		return store.Account{}, liveUnresolved
	}
	token, ok := liveAccessToken(live)
	if !ok {
		// A login with no access token cannot be resolved and cannot be used.
		// Standing down on it costs nothing: Claude Code cannot authenticate
		// with it either, so nothing is running underneath.
		return store.Account{}, liveUnresolved
	}

	ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
	defer cancel()
	uuid, err := e.ResolveOwner(ctx, token)
	if err != nil || uuid == "" {
		return store.Account{}, liveUnresolved
	}

	owner, managed := s.Get(uuid)
	if !managed {
		return store.Account{}, liveForeign
	}
	// Overlaid onto what the account already holds, not written in its place.
	// store.Add replaces the credential file WHOLESALE -- `ccdad add` carries
	// the same warning for the same reason -- so handing it Extract(live)
	// alone would delete this account's stored api-key record along with any
	// account-scoped key the live file happens not to carry, as the price of
	// adopting a refreshed token.
	//
	// Extract rather than the live blob whole for the other half of it: a
	// stored snapshot holds the account-scoped keys and nothing else, and
	// carrying this machine's device identity into one is what Extract's own
	// doc comment forbids.
	next, err := s.Credentials(owner.UUID)
	if err != nil {
		e.logf("could not read %s's stored login to adopt into it: %v", owner.Label(), err)
		return store.Account{}, liveUnresolved
	}
	adopted := cclink.Blob{}
	for k, v := range next {
		adopted[k] = v
	}
	// The live file wins on every account-scoped key, and that is the point:
	// the oracle has just established these are this account's, and they are a
	// generation ahead of what the store holds.
	for k, v := range cclink.Extract(live) {
		adopted[k] = v
	}
	if err := s.Add(owner, adopted); err != nil {
		// The rotated pair is still in the live file and the account is still
		// live, so nothing is lost but the repair. Reported as unresolved
		// because that is what it leaves behind: a file this store still
		// cannot name.
		e.logf("could not adopt the login Claude Code rotated for %s: %v", owner.Label(), err)
		return store.Account{}, liveUnresolved
	}
	return owner, liveAdopted
}

// liveAccessToken pulls the bearer the oracle needs out of the live file.
//
// It is the one place in this package that handles a raw access token, it hands
// it straight to the resolver, and it keeps no copy — the credentials rule this
// tree holds is about tokens that travel, not about tokens that are used once
// at the site that read them.
func liveAccessToken(live cclink.Blob) (string, bool) {
	raw, ok := live["claudeAiOauth"]
	if !ok {
		return "", false
	}
	var wire struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.AccessToken == "" {
		return "", false
	}
	return wire.AccessToken, true
}

// ownerResolver adapts the profile client to the oracle's signature.
//
// It returns the uuid and nothing else. The profile endpoint answers with the
// email, the organization and the tier as well, and none of that may travel
// from here: the uuid is the primary key attribution needs, and a resolver that
// handed back a whole profile would invite a caller to trust the email — which
// is the field a recycled address can make wrong.
func ownerResolver(c *identity.Client) func(context.Context, string) (string, error) {
	return func(ctx context.Context, accessToken string) (string, error) {
		p, err := c.FetchProfile(ctx, accessToken)
		if err != nil {
			return "", err
		}
		return p.AccountUUID, nil
	}
}
