package view

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// Exec runs one ccdad command through a FRESH cobra root and reports what it
// wrote and what it exited with. It is a function value declared in this leaf
// package because internal/cli must import internal/tui to register a command,
// so internal/tui must never import internal/cli. internal/mcpsrv takes the
// identical seam.
type Exec func(argv []string) (code int, stdout, stderr string)

// Snapshot is one complete read of the documents a dashboard renders. Package
// cli builds it — it owns the attribution probe and the notice stream — and
// package tui only reads it, so no number is derived in two places.
type Snapshot struct {
	Now         time.Time
	Rows        []Row
	Report      daemon.Report
	ActiveLabel string // "work@example.com (work)", or "none of the managed accounts"
	Strategy    string // config.Config.Strategy.String(), as CONFIGURED -- see StrategyLabel
	Hover       bool   // config.Config.Hover
	Mode        strategy.Mode
	HasMode     bool
	Version     string   // buildinfo.String()'s first field, or a test constant
	Notices     []string // everything cli would have written to stderr
}

// StrategyLabel is the strategy in FORCE, which under hover is not the one in
// the file.
//
// Hover derives its own: strategy.Options' withHover pass overrides the key with
// headroom, because a window close to its reset already carries a high threshold
// and ordering by reset instant on top of that would rank on a quantity none of
// hover's numbers came from. config.HoverOverrides answers true for the key and
// `ccdad config list` marks it, so a header naming the file's value was the one
// surface left spelling a setting nothing applies -- which reads to a user as
// hover not being on at all.
//
// Strategy itself keeps the configured value rather than being overwritten,
// because the terminal dashboard's strategy picker marks its current entry from
// it. Setting the key while hover is on is a legitimate "for later"; it is only
// naming it as the one in force that is false.
func (s Snapshot) StrategyLabel() string {
	if s.Hover {
		return "hover"
	}
	return s.Strategy
}
