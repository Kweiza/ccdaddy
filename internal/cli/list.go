package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// newEngine builds the poller `status --refresh` borrows. It is a seam for the
// same reason profileBaseURL is: a test that reached the real usage endpoint
// would depend on the machine being online and would send a made-up token to a
// live service. isolate points it at one that fails the test if dialled.
var newEngine = daemon.NewEngine

// codexReadSeams is the read half of the daemon's Codex seams. A foreground
// refresh may spend a stored access token but must never rotate a grant: the
// token endpoint invalidates a refresh token used twice, and the daemon is its
// only spender.
var codexReadSeams = daemon.CodexReadSeams

// refreshUsage implements `ccdad status --refresh`.
//
// Disabled accounts stay visible in status but are deliberately absent from
// want: refreshing is an action, and disabled means the account is held out of
// automatic work. Every failure is a notice and none changes status's exit
// code; the cached dashboard still answers, while usage.fetchedAt remains the
// machine-readable freshness signal.
func refreshUsage(cmd *cobra.Command, s *store.Store, want []store.Account,
	active store.Account, hasActive bool, now time.Time) {

	errw := cmd.ErrOrStderr()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(errw, "note: %v; refreshing on the built-in defaults\n", err)
		cfg = config.Defaults()
	}

	activeUUID := ""
	if hasActive {
		activeUUID = active.UUID
	}

	var fetched, cached int
	engine := newEngine()
	engine.CodexAccessToken, engine.CodexFetchUsage = codexReadSeams(nil)
	for _, r := range engine.Refresh(cmd.Context(), s, want, cfg, activeUUID) {
		switch r.State {
		case daemon.RefreshFetched:
			fetched++
		case daemon.RefreshCached:
			cached++
		case daemon.RefreshHeld:
			fmt.Fprintf(errw, "%s: the usage endpoint rate-limited ccdad; a refresh is allowed again in %s.\n",
				r.Account.Label(), humanDuration(r.At.Sub(now)))
		case daemon.RefreshFailed:
			fmt.Fprintf(errw, "%s could not be refreshed: %v\n", r.Account.Label(), r.Err)
		case daemon.RefreshUnpollable:
			// There is no OAuth grant behind an add-token account, so this is
			// its ordinary state and the row already renders it as unreadable.
		}
	}
	if fetched == 0 && cached > 0 {
		fmt.Fprintf(errw, "Nothing needed refreshing: every reading is under %s old.\n",
			humanDuration(usage.ServeTTL))
	}
}
