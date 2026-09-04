package view

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Columns turns a set of rows into the set of quota columns that describes them.
//
// There is ONE constructor and every surface calls it, which is what makes
// `ccdad list`, `ccdad status`, `ccdad hover status` and the dashboard show the
// same windows in the same order under the same headers. Four tables each
// deciding for themselves is how the two that existed before came to name
// different windows without either saying which.

// HeaderBudget is the widest a window header may be, in display columns.
//
// A scoped cap's display name is arbitrary wire text and the whole name is
// worse: `weekly_scoped:model:Fable` is 25 columns against a dashboard cell
// that reserves 20, and nothing between the cell and the terminal cut it, so
// the overflow came off the right where other columns live.
const HeaderBudget = 10

// resetEpsilon is how far apart two rollovers may be and still be one column.
//
// Measured on a live fleet: every account's `weekly_scoped:model:Fable` cap and
// its `seven_day` window are the same server-side instant, and the two
// timestamps arrive 158 to 320 MICROSECONDS apart because each is derived
// separately at microsecond resolution. Exact equality draws three reset
// columns where the fleet has two rollovers.
//
// A TOLERANCE and not a truncation, and that is the difference that matters.
// Truncating to the second puts a cliff at the second boundary, and two of the
// four `seven_day` resets measured landed 48ms from one -- so a skew that grew
// past 47ms would split the column with nothing on the screen saying why.
// |a-b| <= e has no boundary. One second is the coarsest unit HumanDuration
// prints below a minute, so it is the largest tolerance that cannot merge two
// spans this package would render as different numbers.
const resetEpsilon = time.Second

// WindowColumn is one quota column.
type WindowColumn struct {
	// Name is the wire key. It is the key `ccdad config` takes a per-window
	// threshold on and the key `--json` files the window under, so a reader who
	// moves between the table and either of those reads one vocabulary.
	Name usage.WindowName
	// Header is short, and unique within its Columns by construction.
	Header string
	// Reset indexes Columns.Resets, or -1 when no visible row reported a
	// rollover for this window.
	Reset int
	// Ranked is whether the engine binds on this window without being asked.
	// A window it does not rank can sit at 100% with nothing switching away
	// from it, and a column that did not say so would be a lie by omission.
	Ranked bool
}

// ResetColumn is one rollover, shared by every window that rolls with it.
type ResetColumn struct {
	Header  string
	Windows []usage.WindowName
}

// Columns is the whole quota block: the window columns and the reset columns
// they point into.
type Columns struct {
	Windows []WindowColumn
	Resets  []ResetColumn
}

// ColumnsOf builds the block for a set of rows.
//
// MEMBERSHIP is the union, over the visible rows, of every window each row
// actually carries. That is exactly the set `ccdad status --json` has published
// since it was written -- it walks the same windows and skips the absent ones --
// so the human tables gain no new source here. They stop a derivation that threw
// away all but one of a set they already had.
//
// cinder_cove can never appear and needs no filter: it is left out of the
// snapshot's own window list on purpose, because its reset is an EXPIRY rather
// than a rollover, so a countdown under a header meaning "rolls over in" would
// be counting down to a deletion.
func ColumnsOf(rows []Row) Columns {
	seen := map[usage.WindowName]bool{}
	var fixed, model, surface, unknown []usage.WindowName
	credit := false
	for _, r := range rows {
		for _, w := range r.CarriedWindows() {
			if seen[w.Name] {
				continue
			}
			seen[w.Name] = true
			switch {
			case w.Name == strategy.CreditWindow:
				credit = true
			case !w.Name.Scoped():
				fixed = append(fixed, w.Name)
			default:
				scope, _, ok := usage.ScopeOf(w.Name)
				switch {
				case !ok:
					unknown = append(unknown, w.Name)
				case scope == usage.ScopeModel:
					model = append(model, w.Name)
				case scope == usage.ScopeSurface:
					surface = append(surface, w.Name)
				default:
					unknown = append(unknown, w.Name)
				}
			}
		}
	}

	// ORDER. The fixed keys take the wire's own order rather than an
	// alphabetical one, so the reading order of the table is the reading order
	// of the document behind it. Everything after them sorts by name.
	//
	// Lexicographic and NOT first-seen, and the reason is decisive: rows arrive
	// in STORE order, which moves when an account is added, removed or hidden
	// behind --all. First-seen would slide a column sideways between two runs
	// of the same command. Sorted, the header row is a function of which
	// windows exist and of nothing else.
	names := make([]usage.WindowName, 0, len(seen)+1)
	for _, n := range usage.RateLimitWindowNames() {
		if seen[n] {
			names = append(names, n)
		}
	}
	sortNames(model)
	sortNames(surface)
	sortNames(unknown)
	names = append(names, model...)
	names = append(names, surface...)
	names = append(names, unknown...)
	if credit {
		names = append(names, strategy.CreditWindow)
	}
	_ = fixed

	cols := make([]WindowColumn, 0, len(names))
	for _, n := range names {
		cols = append(cols, WindowColumn{Name: n, Header: WindowHeader(n), Reset: -1, Ranked: rankedWindow(n)})
	}
	uniqueHeaders(cols)

	c := Columns{Windows: cols}
	c.groupResets(rows)
	return c
}

// ColumnsOfNames is ColumnsOf for a caller that already knows which windows it
// is describing and has no rows to derive them from.
//
// `ccdad hover status` is that caller: its table comes from a ranking pass
// rather than from a cache read, and loading the cache a second time to
// rediscover a set it is already holding would be two reads that agree until
// the day one of them moves. It goes through the same order, the same headers
// and the same uniqueness passes, so the column a reader sees here is the
// column they saw on the other three surfaces.
//
// It builds no reset columns: this caller has no rollovers to group, and a
// countdown it invented would be a second answer to a question `ccdad status`
// already answers.
func ColumnsOfNames(names []usage.WindowName) Columns {
	seen := map[usage.WindowName]bool{}
	rows := []Row{}
	_ = rows
	var model, surface, unknown []usage.WindowName
	credit := false
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		switch {
		case n == strategy.CreditWindow:
			credit = true
		case !n.Scoped():
		default:
			scope, _, ok := usage.ScopeOf(n)
			switch {
			case !ok:
				unknown = append(unknown, n)
			case scope == usage.ScopeModel:
				model = append(model, n)
			case scope == usage.ScopeSurface:
				surface = append(surface, n)
			default:
				unknown = append(unknown, n)
			}
		}
	}
	ordered := make([]usage.WindowName, 0, len(seen))
	for _, n := range usage.RateLimitWindowNames() {
		if seen[n] {
			ordered = append(ordered, n)
		}
	}
	sortNames(model)
	sortNames(surface)
	sortNames(unknown)
	ordered = append(ordered, model...)
	ordered = append(ordered, surface...)
	ordered = append(ordered, unknown...)
	if credit {
		ordered = append(ordered, strategy.CreditWindow)
	}
	cols := make([]WindowColumn, 0, len(ordered))
	for _, n := range ordered {
		cols = append(cols, WindowColumn{Name: n, Header: WindowHeader(n), Reset: -1, Ranked: rankedWindow(n)})
	}
	uniqueHeaders(cols)
	return Columns{Windows: cols}
}

func sortNames(n []usage.WindowName) {
	sort.Slice(n, func(i, j int) bool { return n[i] < n[j] })
}

// rankedWindow is whether the engine binds on this window unasked.
//
// The test is the NAME, and it is the same one the config layer uses to tell
// "not yet" from "no": a well-formed scoped name under a scope this build does
// not recognize ranks only once a threshold naming it opts it in, and until
// then it is real quota nothing switches away from.
func rankedWindow(n usage.WindowName) bool {
	if n == strategy.CreditWindow {
		return true
	}
	return usage.ValidWindowName(n) == nil
}

// WindowHeader is the short name a column carries.
//
// The wire key stays the vocabulary -- it is what `ccdad config` and `--json`
// use -- and the legend under the table maps one to the other, so shortening
// here costs a reader nothing they cannot look up two lines down.
func WindowHeader(n usage.WindowName) string {
	switch n {
	case usage.WindowFiveHour:
		return "5H"
	case usage.WindowSevenDay:
		return "7D"
	case usage.WindowSevenDayOAuthApps:
		return "7D APPS"
	case usage.WindowSevenDayOpus:
		return "7D OPUS"
	case usage.WindowSevenDaySonnet:
		return "7D SONNET"
	case usage.WindowCodexPrimary:
		return "CX 1"
	case usage.WindowCodexSecondary:
		return "CX 2"
	case strategy.CreditWindow:
		return "CREDIT"
	}
	if _, display, ok := usage.ScopeOf(n); ok {
		return fitHeader(strings.ToUpper(display))
	}
	return fitHeader(strings.ToUpper(string(n)))
}

// fitHeader cuts a header to the budget with a one-column cue.
//
// The cue is an ASCII '+' rather than an ellipsis because width here is DISPLAY
// columns: U+2026 is one column in an ordinary terminal and two under an
// east-asian width rule, so a budget met with it is met only on some terminals.
func fitHeader(s string) string {
	if ansi.StringWidth(s) <= HeaderBudget {
		return s
	}
	out := ""
	for _, r := range s {
		if ansi.StringWidth(out+string(r)) > HeaderBudget-1 {
			break
		}
		out += string(r)
	}
	return out + "+"
}

// uniqueHeaders makes every header unique in place, in two passes.
//
// It runs AFTER fitHeader, so a cut can only create a collision and never merge
// two windows behind one header without the second pass seeing it. The first
// pass qualifies a scoped name with its scope's initial, which is the
// distinction a reader can act on -- a model cap and a surface cap sharing a
// display name are two different limits. The second pass numbers, and it
// terminates unconditionally, which is what makes "two windows never share a
// header" a guarantee rather than an argument about the shape of names.
func uniqueHeaders(cols []WindowColumn) {
	count := map[string]int{}
	for _, c := range cols {
		count[c.Header]++
	}
	for i := range cols {
		if count[cols[i].Header] < 2 {
			continue
		}
		if scope, _, ok := usage.ScopeOf(cols[i].Name); ok && scope != "" {
			cols[i].Header = fitHeader(strings.ToUpper(scope[:1]) + ":" + cols[i].Header)
		}
	}
	used := map[string]int{}
	for i := range cols {
		h := cols[i].Header
		used[h]++
		if used[h] > 1 {
			cols[i].Header = fitHeader(fmt.Sprintf("%s#%d", h, used[h]))
		}
	}
}

// groupResets folds windows that roll over together into one countdown column.
//
// The relation is greedy against each group's FIRST member, so the answer does
// not depend on map order and the leftmost window always names its group. Two
// windows join only when every row carrying both readably agrees within
// resetEpsilon AND at least one row actually carried both -- the WITNESS. On a
// fleet nobody could read, vacuous agreement would otherwise collapse every
// window into one countdown belonging to none of them. A row carrying both
// where only one reported a rollover is a DISAGREEMENT, not an abstention:
// folding there would hide a stopped clock behind a neighbour's.
func (c *Columns) groupResets(rows []Row) {
	type group struct {
		lead    usage.WindowName
		members []usage.WindowName
	}
	var groups []group
	for i := range c.Windows {
		n := c.Windows[i].Name
		if !anyReset(rows, n) {
			continue
		}
		placed := false
		for g := range groups {
			if resetsAgree(rows, groups[g].lead, n) {
				groups[g].members = append(groups[g].members, n)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, group{lead: n, members: []usage.WindowName{n}})
		}
	}
	for _, g := range groups {
		c.Resets = append(c.Resets, ResetColumn{
			Header:  WindowHeader(g.lead) + " IN",
			Windows: g.members,
		})
		idx := len(c.Resets) - 1
		for _, m := range g.members {
			for i := range c.Windows {
				if c.Windows[i].Name == m {
					c.Windows[i].Reset = idx
				}
			}
		}
	}
}

func anyReset(rows []Row, n usage.WindowName) bool {
	for _, r := range rows {
		if _, ok := r.WindowReset(n); ok {
			return true
		}
	}
	return false
}

func resetsAgree(rows []Row, a, b usage.WindowName) bool {
	witness := false
	for _, r := range rows {
		_, carriedA := r.window(a)
		_, carriedB := r.window(b)
		if !carriedA || !carriedB {
			continue
		}
		ra, okA := r.WindowReset(a)
		rb, okB := r.WindowReset(b)
		if okA != okB {
			return false
		}
		if !okA {
			continue
		}
		d := ra.Sub(rb)
		if d < 0 {
			d = -d
		}
		if d > resetEpsilon {
			return false
		}
		witness = true
	}
	return witness
}

// Names is every window column's wire key, in column order.
func (c Columns) Names() []usage.WindowName {
	out := make([]usage.WindowName, 0, len(c.Windows))
	for _, w := range c.Windows {
		out = append(out, w.Name)
	}
	return out
}

// Unranked is the columns the engine does not bind on.
func (c Columns) Unranked() []usage.WindowName {
	var out []usage.WindowName
	for _, w := range c.Windows {
		if !w.Ranked {
			out = append(out, w.Name)
		}
	}
	return out
}

// UnrankedNote is the sentence for a column carrying real quota nothing
// switches away from, or empty when every column ranks.
//
// It names the hand edit and NOT `ccdad config set`, deliberately: the setter
// refuses exactly these names while the loader carries them, so advice to run
// it would send the reader into the binary's own refusal.
func (c Columns) UnrankedNote() string {
	un := c.Unranked()
	if len(un) == 0 {
		return ""
	}
	names := make([]string, 0, len(un))
	for _, n := range un {
		names = append(names, string(n))
	}
	return fmt.Sprintf("note: %s %s scoped to a key this ccdad does not name, so nothing ranks or switches away from %s. "+
		"Adding a [window_threshold] entry naming it by hand in config.toml opts it in.",
		strings.Join(names, ", "), plural(len(un), "is", "are"), plural(len(un), "it", "them"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Legend maps every header back to the wire key it stands for.
//
// It is the only place a human table still shows the key `ccdad config` takes a
// threshold on, which is what a full-name window column used to buy -- at the
// price of being one column naming one window per row.
func (c Columns) Legend() string {
	if len(c.Windows) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c.Windows))
	for _, w := range c.Windows {
		parts = append(parts, fmt.Sprintf("%s = %s", w.Header, w.Name))
	}
	return "windows:  " + strings.Join(parts, "   ")
}

// PlaceholderHeader is the one quota column a table shows when no visible row
// carried a readable window.
//
// The block is never omitted. A table that simply stopped having quota columns
// would look like a build with no quota feature rather than like a fleet nobody
// could read, and the cells under this header are all Unreadable, which is the
// honest statement.
const PlaceholderHeader = "QUOTA"

// Headers is the quota block's header row: one per window, then one per
// rollover. Both CLI tables and the dashboard call this rather than each
// assembling the same slice, which is what stops one of them growing a column
// the others do not have.
func (c Columns) Headers() []string {
	if len(c.Windows) == 0 {
		return []string{PlaceholderHeader}
	}
	out := make([]string, 0, len(c.Windows)+len(c.Resets))
	for _, w := range c.Windows {
		out = append(out, w.Header)
	}
	for _, r := range c.Resets {
		out = append(out, r.Header)
	}
	return out
}

// Cells is one row's quota block, in Headers' own order and always the same
// length, so a caller can append it between its own fixed columns.
func (r Row) Cells(c Columns, now time.Time) []string {
	if len(c.Windows) == 0 {
		return []string{Unreadable}
	}
	out := make([]string, 0, len(c.Windows)+len(c.Resets))
	for _, w := range c.Windows {
		out = append(out, r.WindowCell(w.Name))
	}
	for _, rc := range c.Resets {
		out = append(out, r.ResetCell(rc, now))
	}
	return out
}

// ResetCell is one countdown, for the group a column points into.
func (r Row) ResetCell(c ResetColumn, now time.Time) string {
	if !r.HasEntry {
		// Nothing about this account was read. "-" would claim it has no such
		// window, which nobody established.
		return Unreadable
	}
	for _, n := range c.Windows {
		if at, ok := r.WindowReset(n); ok {
			return HumanDuration(at.Sub(now))
		}
	}
	return Unreadable
}
