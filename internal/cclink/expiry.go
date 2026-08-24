package cclink

import (
	"encoding/json"
	"time"
)

// SelfRefreshThreshold is how long before expiresAt Claude Code stops using an
// access token and spends its refresh token instead.
//
// Read out of the 2.1.241 bundle rather than guessed. The whole decision is one
// function:
//
//	function Yfe(e){if(e===null)return!1;let t=300000;return Date.now()+t>=e}
//
// and the refresh entry point is gated on it twice — once before taking Claude
// Code's `.oauth_refresh.lock` and again on the re-read under it, both
// returning "not_needed" when it answers false. So a credential further out
// than this is one Claude Code will simply USE, and a credential inside it is
// one Claude Code will rotate the moment it looks.
//
// That makes this the threshold a SWAP has to respect, which is why the
// constant lives here rather than in the token machinery. Installing a
// credential inside this window hands Claude Code a rotation it did not ask
// for, and the rotation moves the refresh token out from under whatever copy
// ccdad still holds.
const SelfRefreshThreshold = 5 * time.Minute

// WouldSelfRefresh reports whether Claude Code, handed this credential right
// now, would spend its refresh token rather than use the access token it
// carries.
//
// A record with no readable expiry answers FALSE, matching Yfe's own null
// case. Claude Code does not refresh on a missing expiresAt, so neither may
// this: answering true would make ccdad refuse to install a hand-written or
// very old credential that Claude Code is perfectly willing to use, and there
// is no expiry to refresh it against anyway.
//
// A blob with no claudeAiOauth answers false for the same reason it is not a
// login: an api-key account has no access token and no expiry, and the swap
// path that installs one must not be gated on a field it will never have.
func WouldSelfRefresh(b Blob, now time.Time) bool {
	at, ok := oauthExpiresAt(b)
	if !ok {
		return false
	}
	// Yfe compares `now + threshold >= expiresAt`, so the boundary itself
	// refreshes. !Before is that comparison without inverting it twice.
	return !now.Add(SelfRefreshThreshold).Before(at)
}

// oauthExpiresAt reads the one field WouldSelfRefresh turns on.
//
// Deliberately its own tiny parse rather than a shared credential struct: this
// package must be able to answer the question for a blob it has only just read
// off disk, without depending on the token machinery that imports it.
func oauthExpiresAt(b Blob) (time.Time, bool) {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return time.Time{}, false
	}
	var wire struct {
		// Milliseconds, as Claude Code writes it. See tokens.parseRecord for
		// what comparing it against a seconds-based expiry costs.
		ExpiresAt *int64 `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.ExpiresAt == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*wire.ExpiresAt), true
}
