package cli

import (
	"context"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/tokens"
)

// freshenWith is the refresher every attended switch hands switcher.Execute.
//
// A swap must not install a login Claude Code would refresh the moment it saw
// it — cclink.WouldSelfRefresh says what that costs. The store normally holds a
// fresh grant because the poller keeps it that way, so this is the repair for
// the account that has sat unpolled: `ccdad switch work` on a laptop that was
// closed all night, where refusing outright would be correct and useless.
//
// One Source per call rather than a package-level one: it holds a clock and an
// HTTP client, and a CLI process performs exactly one switch.
func freshenWith(ctx context.Context) func(string) (cclink.Blob, error) {
	src := tokens.New()
	return func(uuid string) (cclink.Blob, error) {
		return src.Freshen(ctx, uuid)
	}
}
