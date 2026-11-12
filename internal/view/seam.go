package view

import (
	"fmt"
	"math"
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
	// CodexServingLabel is the account ccdad's proxy tries first for the next
	// request in each unpinned Codex session, or "" when there is no pointer or it names no stored account.
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
	AutoSort         bool   // account lists follow the nearest seven-day reset
	Hover            bool   // compatibility storage for the hover policy
	// HoverOrder is the main pool in the order hover's pass left it, empty when
	// hover is off or when no pass ran.
	//
	// It is carried because hover's whole decision IS the order, and no cell
	// on the table shows it: every quota cell is a raw utilization, and which
	// account the session goes to next -- or why an account with room is not
	// it -- is a fact about the ranking, not about any row. HoverLines turns it
	// into one sentence per account.
	HoverOrder []strategy.Ranked
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

// HoverLines is the hover block printed under the table: what the mode does, and
// then the order its pass left the pool in, one line per account.
//
// Nothing when hover is off. The order lines are the half no cell can carry:
// a five-hour account at 85% that the session is not on reads like a mistake
// until the line beside it says it was left at its limit and is held out until
// its window rolls, and an Ultra seat holding half its week is explained only
// by whose week ends first.
//
// In recovery mode the order is not hover's: every account is past its last
// point, and the pass ranks on emptiness, then on which comes back first. The
// lines then say so and describe each account by what that order ranked it on,
// because a rung printed beside an order the rungs did not make would explain a
// decision nobody took.
func (s Snapshot) HoverLines() []string {
	if !s.Hover {
		return nil
	}
	rows := map[string]Row{}
	for _, r := range s.Rows {
		rows[r.Account.UUID] = r
	}
	out := []string{HoverNote}
	describe := s.placeOf
	if s.HasMode && s.Mode == strategy.ModeRecovery {
		out = append(out, HoverRecoveryNote)
		describe = s.comesBack
	}
	for i, ranked := range s.HoverOrder {
		row, ok := rows[ranked.UUID]
		who := ranked.UUID
		if ok {
			who = row.ListLabel()
		}
		line := fmt.Sprintf("hover:    %d. %s  %s", i+1, who, describe(ranked, row))
		if note := s.carryNote(ranked, row); note != "" {
			line += " · " + note
		}
		if row.Active {
			line += " · live"
		}
		out = append(out, line)
	}
	return out
}

// placeOf says where hover's order filed one account and the one fact that put
// it there.
func (s Snapshot) placeOf(r strategy.Ranked, row Row) string {
	switch rung := r.Rung(); rung {
	case strategy.RungShortWindow, strategy.RungWeeklyOnly:
		// Filed by its tier with nothing spent since its windows rolled: the
		// reading names the five-hour window and no figure, so there is no
		// week to name a reset for either.
		if !r.Headroom.Known {
			return rung.String() + " · no usage reported since its windows reset"
		}
		if !r.HasWeeklyReset {
			return rung.String() + " · no weekly reset"
		}
		return rung.String() + " · week resets " + s.clock(r.WeeklyResetsAt)
	case strategy.RungHeldOut:
		// ResetFor is nil-safe, so a row with no reading names no instant.
		if at, ok := row.Entry.Snapshot.ResetFor(r.HeldOut); ok {
			return fmt.Sprintf("held out until %s rolls over at %s", r.HeldOut, s.clock(at))
		}
		return fmt.Sprintf("held out until %s rolls over", r.HeldOut)
	case strategy.RungAtLimit:
		return fmt.Sprintf("at its limit on %s", r.AtLimit())
	case strategy.RungEmpty:
		return fmt.Sprintf("nothing left on %s", r.AtLimit())
	}
	return "unreadable"
}

// carryNote is what an account the session is not on cannot do, when its levels
// say it has room and the work running now would still run it out before its
// first live reading: the hand-off passes it over, and a line that printed only
// its rung would leave that unexplained. It never says "at its limit" -- a rung
// reads levels alone -- and it is silent for the live account, whose own limit
// is its rung.
func (s Snapshot) carryNote(r strategy.Ranked, row Row) string {
	if row.Active || r.Carries || !r.HasRunway || r.RunsOutAt.IsZero() || r.AtLimit() != "" {
		return ""
	}
	minutes := int(math.Ceil(r.RunsOutAt.Sub(s.Now).Minutes()))
	return fmt.Sprintf("cannot carry the work running now (runs out in %d min)", minutes)
}

// HoverRecoveryNote is the line that says the order below it is the recovery
// order rather than hover's, and states that order's keys in the order
// strategy's lessRecovery applies them: an account with anything left before an
// empty one, then one back within the recovery horizon (an hour,
// strategy.DefaultRecoveryHorizon) before one that is not, the soonest first,
// and the rest by how little they are past their line.
const HoverRecoveryNote = "hover:    recovery: every account is past its last point; accounts with something left come " +
	"before empty ones, and within each, those back within the hour come first, soonest first, " +
	"then the rest, the least past its line first"

// comesBack describes one account in the recovery order by the fact that order
// ranked it on: emptiness, the rollover when that is inside the horizon, and how
// far past its line it is when it is not.
func (s Snapshot) comesBack(r strategy.Ranked, _ Row) string {
	back := "names no rollover"
	if r.HasRecovery {
		back = "back at " + s.clock(r.RecoversAt)
	}
	if _, empty := r.Empty(); empty {
		return fmt.Sprintf("nothing left on %s · %s", r.AtLimit(), back)
	}
	if r.ReturnsInsideHorizon {
		return back
	}
	return fmt.Sprintf("%.1f past its line · %s", -r.Headroom.Slack, back)
}

// clock renders an instant in the reader's own zone, to the minute, with the
// weekday a weekly reset needs.
func (s Snapshot) clock(at time.Time) string {
	return at.In(s.zone()).Format("Mon 15:04")
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
