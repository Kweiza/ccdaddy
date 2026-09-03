package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// The two lanes switch for different reasons and at different rates, and one
// stamp for both would have a Claude rotation start the Codex cooldown -- so
// the Codex lane would sit out a five-minute hold it never earned, and a Codex
// repoint would do the same to Claude Code's login.
func TestAClaudeSwitchLeavesTheCodexStampAlone(t *testing.T) {
	st := NewState()
	st.RecordCodexSwitch("cx-1", now.Add(-time.Hour))
	st.RecordSwitch("cl-1", now)

	at, to := st.CodexLastSwitch()
	if !at.Equal(now.Add(-time.Hour)) || to != "cx-1" {
		t.Fatalf("CodexLastSwitch() = (%v, %q), want (%v, %q)", at, to, now.Add(-time.Hour), "cx-1")
	}
}

func TestACodexSwitchLeavesTheClaudeStampAlone(t *testing.T) {
	st := NewState()
	st.RecordSwitch("cl-1", now.Add(-time.Hour))
	st.RecordCodexSwitch("cx-1", now)

	at, to := st.LastSwitch()
	if !at.Equal(now.Add(-time.Hour)) || to != "cl-1" {
		t.Fatalf("LastSwitch() = (%v, %q), want (%v, %q)", at, to, now.Add(-time.Hour), "cl-1")
	}
}

// ForProvider is what lets one anti-flap implementation serve both lanes: the
// gates read LastSwitch, so the Codex view has to present the codex pair under
// that name rather than growing a second cooldown gate.
func TestForProviderPresentsTheCodexPairAsTheLastSwitch(t *testing.T) {
	st := NewState()
	st.RecordSwitch("cl-1", now.Add(-time.Hour))
	st.RecordCodexSwitch("cx-1", now)

	at, to := st.ForProvider(provider.Codex).LastSwitch()
	if !at.Equal(now) || to != "cx-1" {
		t.Fatalf("ForProvider(codex).LastSwitch() = (%v, %q), want (%v, %q)", at, to, now, "cx-1")
	}
	claudeAt, claudeTo := st.ForProvider(provider.Claude).LastSwitch()
	if !claudeAt.Equal(now.Add(-time.Hour)) || claudeTo != "cl-1" {
		t.Fatalf("ForProvider(claude).LastSwitch() = (%v, %q), want (%v, %q)",
			claudeAt, claudeTo, now.Add(-time.Hour), "cl-1")
	}
}

// The whole reason the pair exists: the cooldown gate has to see it. This is
// the same fixture TestCooldownHoldsASecondSwitchOffAndSaysWhenItLifts uses,
// with the stamp written on the codex side and read through ForProvider.
func TestTheCodexCooldownHoldsThroughForProvider(t *testing.T) {
	st := NewState()
	st.RecordCodexSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)},
		at(time.Minute), Config{}, st.ForProvider(provider.Codex), "a")
	want(t, p, ActionStay, ReasonCooldown, "")
	if got, wantAt := p.RetryAt, now.Add(DefaultCooldown); !got.Equal(wantAt) {
		t.Errorf("RetryAt = %v, want %v", got, wantAt)
	}
}

// And the negative of it, which is the mutation this pair is most likely to
// lose: a Claude stamp must not hold the Codex lane off a move.
func TestAClaudeStampDoesNotHoldTheCodexLane(t *testing.T) {
	st := NewState()
	st.RecordSwitch("b", now)

	p := Decide([]Candidate{hr("a", 20, time.Hour), hr("b", 50, time.Hour)},
		at(time.Minute), Config{}, st.ForProvider(provider.Codex), "a")
	want(t, p, ActionSwitch, ReasonBetterTarget, "b")
}

// The pair has to survive the file, or a daemon restart clears a cooldown the
// document was written to remember.
//
// isolate is this package's own helper, in antiflap_test.go: it points
// CCDAD_HOME at a temp directory so the file-backed test writes nowhere near
// the developer's store.
func TestTheCodexPairSurvivesAWriteAndAReload(t *testing.T) {
	isolate(t)

	if err := RecordCodexSwitch("cx-1"); err != nil {
		t.Fatalf("RecordCodexSwitch: %v", err)
	}
	reloaded, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	at, to := reloaded.CodexLastSwitch()
	if to != "cx-1" || at.IsZero() {
		t.Fatalf("CodexLastSwitch() = (%v, %q) after a reload, want a stamp naming cx-1", at, to)
	}
	// The Claude half is untouched on disk, which is the property a shared
	// document makes easy to lose.
	if claudeAt, claudeTo := reloaded.LastSwitch(); !claudeAt.IsZero() || claudeTo != "" {
		t.Fatalf("LastSwitch() = (%v, %q), want the zero pair", claudeAt, claudeTo)
	}
}
