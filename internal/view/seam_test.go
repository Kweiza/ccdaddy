package view

import "testing"

// Under hover the configured strategy has stopped being read. strategy.Options'
// withHover pass overrides it -- it forces headroom, because hover has already
// expressed perishability on the slack axis -- and config.HoverOverrides already
// answers true for the key. `ccdad config list` says so in its HOVER column, and
// the dashboard header was the one surface that did not: it printed a strategy
// nothing was applying, which reads exactly like hover being off.
func TestTheDashboardNamesHoverRatherThanTheStrategyHoverOverrode(t *testing.T) {
	s := Snapshot{Strategy: "consume-first", Hover: true}
	if got := s.StrategyLabel(); got != "hover" {
		t.Errorf("StrategyLabel() = %q, want %q: under hover the file's value is not the one in force", got, "hover")
	}
	// The field itself keeps the configured value, and that is deliberate: the
	// terminal dashboard's strategy picker marks its current entry from it, and
	// a Snapshot that carried "hover" there would leave the picker marking
	// nothing at all.
	if s.Strategy != "consume-first" {
		t.Errorf("Strategy = %q, want the configured value kept beside the label", s.Strategy)
	}
}

// With hover off the configured strategy IS what the engine ranks on, and
// naming hover there would be the same falsehood pointing the other way.
func TestTheDashboardNamesTheConfiguredStrategyWithHoverOff(t *testing.T) {
	if got := (Snapshot{Strategy: "consume-first"}).StrategyLabel(); got != "consume-first" {
		t.Errorf("StrategyLabel() = %q with hover off, want the configured %q", got, "consume-first")
	}
}
