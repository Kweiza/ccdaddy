package view

import (
	"fmt"
	"strings"
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
	// account: `ccdad which` prints the bare Claude label, and the block
	// SummaryLines builds draws one Active line rather than two.
	CodexServingLabel string
	// CodexServingUUID is the same account's key, for the --json payloads. A
	// label is for a person and a uuid is what a script keys on, and deriving
	// one from the other at the payload builder would be a second lookup of a
	// pointer this snapshot has already read.
	CodexServingUUID string
	Strategy         string // the selected policy: hover, manual, headroom or consume-first
	Hover            bool   // compatibility storage for the hover policy
	// HoverAccounts is the per-account half of hover's derivation, empty when
	// hover is off or when no pass ran.
	//
	// The rows already carry the thresholds; this carries the SECOND TERM every
	// one of them was built from. It used to be one number for the whole pool
	// and a reader could hold it in their head; it is now per account, so a
	// table that did not publish it would be asking a reader to accept a
	// threshold they cannot reconstruct. StrandedNote is what turns it into a
	// sentence.
	HoverAccounts []strategy.HoverAccount
	// Manual is compatibility storage for the manual policy. The selected
	// strategy remains explicit so a healthy ranking under manual cannot look
	// like a broken engine.
	Manual      bool
	Mode        strategy.Mode
	HasMode     bool
	Version     string   // buildinfo.String()'s first field, or a test constant
	Notices     []string // everything cli would have written to stderr
	UnknownKeys []string // unrecognised top-level credential keys retained by the store

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

// StrandedNote names every account whose share is wider than the pool's flat
// slice, and says what is expiring to make it so.
//
// Empty when nothing strands, which is the ordinary case and the reason this is
// a note rather than a column: a fleet keeping up with its weeks would carry a
// column of identical numbers on every row forever.
//
// It exists because the hover footer's promise -- "thresholds are derived per
// account and window" -- stopped being enough the moment the share varied. A
// reader looking at two accounts whose windows elapsed identically and whose
// thresholds differ by forty points has no way to ask why, and the answer is
// not in any cell on the table.
func (s Snapshot) StrandedNote() string {
	if !s.Hover {
		return ""
	}
	label := map[string]string{}
	for _, r := range s.Rows {
		label[r.Account.UUID] = r.ListLabel()
	}
	parts := make([]string, 0, len(s.HoverAccounts))
	for _, a := range s.HoverAccounts {
		if a.Stranded <= 0 || a.Share <= a.PoolShare {
			continue
		}
		who := label[a.UUID]
		if who == "" {
			who = a.UUID
		}
		parts = append(parts, fmt.Sprintf("%s share %.0f (pool %.0f): %.0f pts of %s expire %s",
			who, a.Share, a.PoolShare, a.Stranded, a.Window, a.ResetsAt.In(s.zone()).Format("Mon 15:04")))
	}
	if len(parts) == 0 {
		return ""
	}
	return "running wide of pace on purpose -- " + strings.Join(parts, "; ")
}

// zone is the location the note renders instants in, which is the reader's own.
func (s Snapshot) zone() *time.Location {
	if s.Now.Location() != nil {
		return s.Now.Location()
	}
	return time.Local
}

// StrategyLabel is the single user-facing switching policy. New snapshots
// already carry that value in Strategy; the boolean fallback keeps snapshots
// built by older callers and tests truthful during an in-process upgrade.
func (s Snapshot) StrategyLabel() string {
	if s.Manual {
		return "manual"
	}
	if s.Hover {
		return "hover"
	}
	return s.Strategy
}

// ActiveLine is who this machine is spending, in ONE line, for `ccdad which`
// and for nothing else.
//
// One line is what `which` is: a command written to be read by a shell, whose
// whole output is the answer. Every surface with room for a block prints one
// line PER PROVIDER instead -- Snapshot.SummaryLines is where that lives -- so
// that a long account label cannot be cut in a way that takes the other
// provider beside it along.
//
// It is a free function and no longer also a method on Snapshot. It was both
// while `ccdad status` printed this same joined sentence; status prints the
// two-line block now, so the method had no caller left, and an exported method
// that puts two accounts on one line is exactly the third spelling of "who is
// live" this package exists not to have.
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
