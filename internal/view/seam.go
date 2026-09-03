package view

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/forecast"
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
	// CodexServingLabel is the account ccdad's codex proxy serves new threads
	// from, or "" when there is no pointer or it names no stored account.
	//
	// EMPTY IS LOAD-BEARING. It is what keeps every surface rendering the exact
	// bytes it rendered before codex existed on a machine that has no codex
	// account -- seven byte-compared dashboard pages among them.
	CodexServingLabel string
	Strategy          string // config.Config.Strategy.String(), as CONFIGURED -- see StrategyLabel
	Hover             bool   // config.Config.Hover
	Mode              strategy.Mode
	HasMode           bool
	Version           string   // buildinfo.String()'s first field, or a test constant
	Notices           []string // everything cli would have written to stderr

	// Forecast is the measured burn and what it implies, and HasForecast is
	// whether one could be produced at all.
	//
	// The pair is a tri-state and not a nilable pointer, for the reason the
	// fields above it are: a dashboard reads this struct and must never be able
	// to dereference its way into a panic on a machine that has been recording
	// for ten minutes. HasForecast false is "no measurement", never "a fleet
	// burning nothing" -- the zero Fleet's verdicts are all VerdictUnknown, so
	// a renderer that ignored the flag would still print no promise.
	//
	// Package cli computes it, as it computes everything else here, so the same
	// value reaches the dashboard and the command line and no number is derived
	// in two places.
	Forecast    forecast.Fleet
	HasForecast bool
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

// ActiveLine is who this machine is spending, in one sentence.
//
// It is a free function as well as a method because `ccdad which` has no
// Snapshot to build and must produce the identical line: two spellings of one
// sentence is how two commands come to describe one machine two ways, which is
// the failure this whole package exists to prevent.
//
// With no codex account it is the Claude label ALONE, unlabelled. That is not a
// degradation -- it is the answer every reader and every script had before
// there was a second provider, and prefixing it with "Claude:" on a machine
// with one provider would be labelling a distinction that does not exist there.
func ActiveLine(claude, codex string) string {
	if codex == "" {
		return claude
	}
	return "Claude: " + claude + " · Codex: " + codex
}

// ActiveLine is this snapshot's.
func (s Snapshot) ActiveLine() string { return ActiveLine(s.ActiveLabel, s.CodexServingLabel) }
