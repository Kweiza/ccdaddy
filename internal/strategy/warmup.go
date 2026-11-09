package strategy

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The warm-up's own rules -- which cold window a turn would start, whether this
// account may be warmed now, and when the poll after a rollover should look --
// stated once, for the daemon, which acts on them, and for the weekly
// simulation, which has to start the same clocks at the same instants for its
// figures to be the daemon's. The account-level gates (provider, probe_unknown,
// the live account, quarantine) stay with the daemon; these are the parts that
// read only a reading, a probe record and a table.

// WarmUpDue is the window a warm-up of this account would start now, and
// whether it is due: a cold window (ColdWindow) a turn may be spent on without
// reaching credits (WarmUpWouldSpendCredits), an attempt the probe record
// allows (usage.ProbeState.MayProbe), and the account's phase after the
// rollover passed (WarmPhase).
func WarmUpDue(uuid string, s *usage.Snapshot, probe usage.ProbeState, thr Thresholds, now time.Time) (usage.WindowName, bool) {
	w, rollover, ok := ColdWindow(s, "", thr, now)
	if !ok {
		return "", false
	}
	// The one refusal that is about money rather than about clocks. A turn spent
	// on an account with nothing left in a window can be billed to metered
	// credits, and unattended overage takes two independent opt-ins that a warm
	// clock is not one of.
	if WarmUpWouldSpendCredits(s, "", thr) {
		return "", false
	}
	if !probe.MayProbe(now, w, rollover, !rollover.IsZero()) {
		return "", false
	}
	// The phase, and it is the last gate rather than the first because it is the
	// only one that says "not yet" to a warm-up every other rule has approved.
	//
	// Without it the fleet's anchors re-form into one batch every cycle: each
	// account's next rollover is set by its last warm-up, and every warm-up
	// happens within a minute of the rollover that triggered it, so a fleet
	// warmed together stays warmed together forever. Measured on this fleet on
	// 2026-09-05: six five-hour windows resetting inside a forty-minute band of a
	// three-hundred-minute cycle. The consequence is not a shortage -- `ccdad
	// runway` scored the five-hour axis "holds" with a 1.99x margin on the same
	// afternoon three accounts sat at 100% together -- it is that all 120 pp/h of
	// replenishment arrives at once and the hours between it have none.
	//
	// It is deliberately NOT done by delaying the poll. WarmTarget aims a poll AT
	// a rollover and pollpolicy.WarmClamp declines any target further out than
	// the ordinary cadence would reach, so a phase added there is silently
	// dropped and the batch survives. The probe is the thing whose timing sets
	// the anchor, so the probe is what has to wait.
	if wait := WarmPhase(uuid, s, w); wait > 0 && !rollover.IsZero() && now.Before(rollover.Add(wait)) {
		return "", false
	}
	return w, true
}

// WarmTarget is when the next of this account's clocks runs down, plus the
// margin that keeps the reading aimed at it from arriving before the rollover
// it is aimed at. Zero means there is nothing to aim at.
//
// Zero is the ordinary answer for the account this exists to serve, and that is
// not a contradiction: an account whose five-hour clock is already stopped has
// no future rollover to schedule against, and it does not need one -- it is
// warm-up-due on this very tick. The schedule matters for the account whose
// clock is RUNNING, which is the one that will go cold later and unattended.
//
// The margin is jittered across [ProbeWakeMargin, 2*ProbeWakeMargin), rnd in
// [0, 1) choosing where, because rollover instants cluster: a fleet warmed in
// one batch rolls over in one batch -- the live one warmed three accounts
// within 19 minutes -- and the anchors themselves land on ten-minute
// boundaries. An unjittered margin would put several `claude -p` children on
// the same second.
func WarmTarget(s *usage.Snapshot, thr Thresholds, now time.Time, rnd float64) time.Time {
	at, ok := NextResetAmong(s, "", thr, now)
	if !ok {
		return time.Time{}
	}
	return at.Add(usage.ProbeWakeMargin + time.Duration(rnd*float64(usage.ProbeWakeMargin)))
}

// WarmPhase is how long this account's warm-up waits after its rollover, so the
// fleet's anchors stop landing in one band.
//
// The phase is derived from the UUID rather than from a position in a list,
// because there is no list here: the warm-up is asked about one account, and the
// identity peers it could reach are a singleton on a fleet where every account
// carries its own organization. A hash is stable across restarts -- a per-process
// seed would re-phase the whole fleet on every daemon start, which is the batch
// re-forming by another route -- needs nothing but the account, and spreads a
// fleet of any size without any account having to know the size.
//
// It is capped at HALF the window, and the cap is the cost being bounded rather
// than tidiness: a delayed warm-up leaves that account's clock stopped for the
// delay, which is elapsed window time the account does not get back. Half a
// cycle turns this fleet's forty-minute band into a hundred-and-fifty-minute
// one, which is enough to break the batch, while holding the worst case to
// something an axis with a 1.99x margin can pay.
//
// A window whose length this build cannot name gets no phase. That is the codex
// window with no limit_window_seconds: with no scale to state a phase on, the
// answer is silence rather than a guess.
//
// The length comes from the COLD window itself and not from the next rollover
// among the readable ones, and that is not interchangeable: at the moment this
// is asked the window has already rolled over, so it has no future reset for a
// walk to find, and a phase derived that way would be zero on every account this
// gate exists to spread.
func WarmPhase(uuid string, s *usage.Snapshot, name usage.WindowName) time.Duration {
	length, ok := windowLengthIn(s, name)
	if !ok || length <= 0 {
		return 0
	}
	return time.Duration(uuidPhase(uuid) * float64(length) / 2)
}

// windowLengthIn is how long the named window is, read off the reading that
// carries it. A codex window states its own length on the wire; the fixed keys
// are known by name.
func windowLengthIn(s *usage.Snapshot, name usage.WindowName) (time.Duration, bool) {
	if s == nil {
		return 0, false
	}
	for _, w := range s.AllWindows() {
		if w.Name != name {
			continue
		}
		return usage.WindowLengthOf(w.Name, w.Window)
	}
	return 0, false
}

// uuidPhase is a stable number in [0, 1) for one account.
//
// FNV-1a and not maphash: this decides when a probe spends the user's quota, so
// it has to answer the same thing on every process and on every machine. A
// per-process seed would re-phase the whole fleet on every daemon restart, which
// is the batch re-forming by another route.
func uuidPhase(uuid string) float64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(uuid); i++ {
		h ^= uint64(uuid[i])
		h *= prime64
	}
	return float64(h%1_000_000) / 1_000_000
}
