package view

import "time"

// Timestamp is the one absolute-time rendering in this repository, and the zone
// is always in it. The two human-facing absolute formats that came before it
// carry neither a date nor a zone: internal/cli/hover.go's clock is "15:04",
// and internal/switcher/evaluate.go's Explain uses time.Kitchen. Both are fine
// for a moment inside the hour and useless for a runway that names one days
// out. Every remaining .Format in the tree is RFC 3339 or the daemon log's
// millisecond variant of it, which are machine formats and not an answer here.
// It lives in internal/view so the runway command, status, list and the
// dashboard spell one layout once rather than four times.
//
// The zone is not optional, and it is not read here. The arithmetic that
// produces these moments must not touch the environment, so the location
// arrives as a parameter. A nil location falls back to UTC rather than to
// time.Local: a caller that passed none has not told us its zone, and
// time.Local would print a confidently wrong hour on any machine whose TZ is
// not the reader's -- including CI, where nothing sets it.
func Timestamp(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}
