package view

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// ThresholdsFor is internal/cli's thresholdsFrom with its notices RETURNED
// rather than written to a cobra command, so a caller with no command can use
// it. Package cli's own wrapper prints them and is otherwise unchanged.
//
// The plan is read only when hover is on; with hover off it is ignored
// entirely, including its error.
func ThresholdsFor(cfg config.Config, now time.Time, plan strategy.Plan, planErr error) (
	resolve func(uuid string) strategy.Thresholds, notices []string) {

	o := cfg.RankOptions(now)
	configured := func(string) strategy.Thresholds { return o.Thresholds() }
	if !o.Hover {
		return configured, nil
	}
	if planErr != nil {
		notices = append(notices, fmt.Sprintf(
			"note: hover is on, but the thresholds it derived could not be read (%v); the rows are measured against the configured ones\n", planErr))
		return configured, notices
	}
	if plan.Hover == nil {
		// Hover is on and the engine made no pass, which is what "nothing has
		// ever been polled" looks like from here: there was no pool to divide.
		// Every row is then unread and none of them reaches a threshold at all,
		// so what this answers is only what hover answers for an account it has
		// never seen.
		var none strategy.HoverPlan
		return none.For, notices
	}
	return plan.Hover.For, notices
}
