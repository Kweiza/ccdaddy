package view

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// This file is ONE definition of what the account list is: which columns exist
// and in what order, what each cell of each column says, how the rows are
// grouped by provider, and what is printed underneath the table.
//
// It exists because there were two. `ccdad status` and the terminal dashboard
// shared the cell strings in row.go and window.go and nothing else: each owned
// its own column order, its own switch from a column to a cell, and its own
// list of trailer lines. So `status` alone showed TIER and the legend, the
// dashboard alone showed STATE and AUTO, and the two disagreed about what an
// account list even is -- not because either was wrong, but because there was
// no place for the answer to live.
//
// Nothing here reads the clock, the filesystem, the environment or the
// terminal, which is the rule the package doc states and the reason the width
// ladder is NOT here: that arithmetic starts from a border, and a border is a
// fact about a machine. What this file publishes instead is the ladder's
// INPUT -- a header and a content width per column -- so the ladder can read
// them rather than restate constants back-computed from a width table.

// ColumnKind is what a column IS.
//
// It is separate from ListColumn below for the reason the dashboard's own
// ColKind was: a window column cannot be named by a constant, because how many
// there are and which windows they stand for is a fact about the fleet.
type ColumnKind int

const (
	// ColumnIdx is the live-account marker and the store index, together, in
	// the one cell that carries both.
	//
	// The index is BARE here -- the number and not store.Account.Ref -- and
	// that is a fact about this table rather than about the index. The index is
	// numbered per provider, so it repeats across a mixed fleet; what makes it
	// unambiguous in this column is that every table drawing it is grouped into
	// provider sections, and the heading over the rows is the prefix Ref would
	// have printed on each of them. A surface that lists accounts WITHOUT that
	// grouping -- `ccdad runway` is the one -- prints Ref instead.
	ColumnIdx ColumnKind = iota
	ColumnAccount
	ColumnType
	ColumnTier
	// ColumnWindow is one window's cell, and ListColumn.Index indexes
	// Columns.Windows.
	ColumnWindow
	// ColumnReset is one rollover's countdown, and ListColumn.Index indexes
	// Columns.Resets.
	ColumnReset
	// ColumnWorst is the whole window block collapsed into one cell, for a
	// table too narrow to carry the block itself. It is never in ListColumns:
	// CollapseWindows is what puts it there.
	ColumnWorst
	ColumnState
	ColumnAuto
	ColumnAge
	// ColumnBlank is a column that holds nothing: the room a section's quota
	// block does not need and a wider section's does.
	//
	// It exists so that the columns AFTER the block -- STATE, AUTO, AGE -- land
	// at the same index in every section, which is what keeps them under one
	// another across the seam in a table whose two halves draw different
	// windows. Its cell is the empty string and never NoQuantity: "-" is a claim
	// that the quantity does not exist for this account, and this column carries
	// no claim at all.
	//
	// Appended LAST. Nothing persists these values, but they are switched on in
	// four packages, and a renumbering is a diff nobody can review.
	ColumnBlank
)

// String names a kind, for a test failure that would otherwise print an
// integer nobody can read back to a column.
func (k ColumnKind) String() string {
	switch k {
	case ColumnIdx:
		return "IDX"
	case ColumnAccount:
		return "ACCOUNT"
	case ColumnType:
		return "TYPE"
	case ColumnTier:
		return "TIER"
	case ColumnWindow:
		return "WINDOW"
	case ColumnReset:
		return "RESET"
	case ColumnWorst:
		return "WORST"
	case ColumnState:
		return "STATE"
	case ColumnAuto:
		return "AUTO"
	case ColumnAge:
		return "AGE"
	case ColumnBlank:
		return "BLANK"
	}
	return fmt.Sprintf("ColumnKind(%d)", int(k))
}

// Each column's CONTENT width: the widest cell it renders, in display columns.
//
// Nothing here pads to these and no surface has to use them. They are what a
// width ladder reserves room with, published so that the ladder can read a
// column instead of holding a second copy of these numbers.
//
// EVERY VALUE IS TODAY'S DASHBOARD FOOTPRINT MINUS TWO -- the standard gap
// that follows a column -- so a caller computing
//
//	max(display width of Header, Content) + 2
//
// reserves exactly what internal/tui reserves today, column for column, and
// adopting these moves no page. The derivation, footprint by footprint:
//
//	IDX      4   the ladder reserves 6, and "IDX" is 3 wide
//	ACCOUNT 20   the ladder's own accountComfort; it reserves that plus 2
//	TYPE    12   typeFootprint is 14, stated there as 12 content + 2 gap
//	TIER     6   tierFootprint is 8, stated there as 6 content + 2 gap
//	WINDOW   4   windowFootprint is max(header, 4) + 2; "100%" is the widest
//	RESET    5   resetFootprint is max(header, 5) + 2; "1d16h" is the widest
//	WORST   15   worstFootprint is HeaderBudget + 7, held as "100% " plus a
//	             window header of HeaderBudget
//	STATE   13   stateFootprint is 15, stated there as 13 content + 2 gap
//	AUTO     4   autoFootprint is 6, stated there as 4 content + 2 gap
//	AGE      6   ageFootprint is 8, stated there as 6 content + 2 gap
//
// Four of them -- IDX, TYPE, AUTO and STATE -- are back-computed from a width
// table rather than measured against what they hold, and they are carried over
// AS THEY ARE on purpose. Re-deriving one honestly changes what fits at some
// width, which is a change to the page and belongs in a commit that says so.
const (
	idxContent     = 4
	accountContent = 20
	typeContent    = 12
	tierContent    = 6
	windowContent  = 4
	resetContent   = 5
	worstContent   = HeaderBudget + 5
	stateContent   = 13
	autoContent    = 4
	ageContent     = 6
)

// The heading each fixed column carries. The window and reset headings are the
// fleet's own and come off Columns.
const (
	IdxHeader     = "IDX"
	AccountHeader = "ACCOUNT"
	TypeHeader    = "TYPE"
	TierHeader    = "TIER"
	WorstHeader   = "WORST"
	StateHeader   = "STATE"
	AutoHeader    = "AUTO"
	AgeHeader     = "AGE"
)

// ListColumn is one column of the account table.
//
// It is a comparable struct, so a drop list is still `==` and a caller can ask
// whether a column survived without writing a predicate for every kind.
type ListColumn struct {
	Kind ColumnKind
	// Index indexes Columns.Windows for ColumnWindow and Columns.Resets for
	// ColumnReset. It is -1 for every kind that indexes nothing -- and for the
	// one ColumnWindow that indexes nothing either, the placeholder quota
	// column a fleet with no readable window gets.
	Index int
	// Header is the heading, and Content the widest cell. See the block above
	// for what Content is and is not.
	Header  string
	Content int
}

// ListColumns is the full fixed column order, once, for every surface that
// draws an account list.
//
// It is the UNION of what the two surfaces show today and it holds each of
// them as a subsequence, which is what lets either adopt it without reordering
// anything a reader has learned: `ccdad status` draws IDX, ACCOUNT, TYPE,
// TIER, the quota block and AGE, and the dashboard draws IDX, ACCOUNT, TYPE,
// the quota block, STATE, AUTO and AGE. A surface that cannot fit all of them
// drops from ListDrops rather than picking its own set.
//
// The quota block is never empty. With no visible row carrying a readable
// window there is one placeholder column under PlaceholderHeader whose cells
// are all Unreadable, which is the same answer Columns.Headers and Row.Cells
// have given since they were written: a table that simply stopped having quota
// columns reads as a build with no quota feature rather than as a fleet nobody
// could read.
func ListColumns(c Columns) []ListColumn {
	out := make([]ListColumn, 0, 8+len(c.Windows)+len(c.Resets))
	out = append(out,
		ListColumn{Kind: ColumnIdx, Index: -1, Header: IdxHeader, Content: idxContent},
		ListColumn{Kind: ColumnAccount, Index: -1, Header: AccountHeader, Content: accountContent},
		ListColumn{Kind: ColumnType, Index: -1, Header: TypeHeader, Content: typeContent},
		ListColumn{Kind: ColumnTier, Index: -1, Header: TierHeader, Content: tierContent},
	)
	if len(c.Windows) == 0 {
		out = append(out, ListColumn{
			Kind: ColumnWindow, Index: -1, Header: PlaceholderHeader, Content: windowContent,
		})
	}
	for i, w := range c.Windows {
		out = append(out, ListColumn{
			Kind: ColumnWindow, Index: i, Header: w.Header, Content: windowContent,
		})
	}
	for i, r := range c.Resets {
		out = append(out, ListColumn{
			Kind: ColumnReset, Index: i, Header: r.Header, Content: resetContent,
		})
	}
	return append(out,
		ListColumn{Kind: ColumnState, Index: -1, Header: StateHeader, Content: stateContent},
		ListColumn{Kind: ColumnAuto, Index: -1, Header: AutoHeader, Content: autoContent},
		ListColumn{Kind: ColumnAge, Index: -1, Header: AgeHeader, Content: ageContent},
	)
}

// SectionColumns is one section's column list, laid out in the slots a PLANNED
// list reserved.
//
// It is the piece that lets two halves of one table draw different windows and
// still line up. The planned list is the widest section's, which is what a width
// ladder measured and what every column's width was reserved for; this swaps the
// quota block for the section's own and pads out the difference with blanks, so
// STATE, AUTO and AGE land at the same index in every section.
//
// The columns in FRONT of the block are untouched, and that is what makes the
// swap safe rather than lucky: IDX, ACCOUNT, TYPE and TIER are the same columns
// in every section, so the block always starts at the same slot.
//
// A COLLAPSED plan keeps its one ColumnWorst and still takes the section's own
// rollovers. The worst cell reads the block it is handed, so it is already this
// section's answer; the reset columns are the half that has to be re-indexed,
// because ListColumn.Index points into Columns.Resets and the planned list's
// indices are the widest section's. Handing that list back whole is what put a
// Claude rollover's index into a Codex block and read past the end of it.
//
// A section needing more slots than the plan reserved is cut to fit, keeping the
// soonest rollovers -- which is the direction ListDrops takes them in anyway. It
// is reachable only from a caller that planned against some other list, and
// shearing every column after the block would be the alternative.
func SectionColumns(planned []ListColumn, c Columns) []ListColumn {
	slots, collapsed := 0, false
	for _, p := range planned {
		switch p.Kind {
		case ColumnWorst:
			collapsed = true
			slots++
		case ColumnWindow, ColumnReset, ColumnBlank:
			slots++
		}
	}
	own := make([]ListColumn, 0, slots)
	if collapsed {
		own = append(own, ListColumn{
			Kind: ColumnWorst, Index: -1, Header: WorstHeader, Content: worstContent,
		})
		for i, r := range c.Resets {
			own = append(own, ListColumn{
				Kind: ColumnReset, Index: i, Header: r.Header, Content: resetContent,
			})
		}
	} else {
		for _, l := range ListColumns(c) {
			if l.Kind == ColumnWindow || l.Kind == ColumnReset {
				own = append(own, l)
			}
		}
	}
	// A section wider than the plan cannot happen -- the plan is the widest
	// section's -- but a caller that passed some other list would otherwise
	// silently lengthen the row and shear every column after the block.
	if len(own) > slots {
		own = own[:slots]
	}
	for len(own) < slots {
		own = append(own, ListColumn{Kind: ColumnBlank, Index: -1, Content: windowContent})
	}

	out := make([]ListColumn, 0, len(planned))
	placed := false
	for _, p := range planned {
		switch p.Kind {
		case ColumnWindow, ColumnReset, ColumnBlank:
			if !placed {
				out = append(out, own...)
				placed = true
			}
		default:
			out = append(out, p)
		}
	}
	return out
}

// ListDrops is the drop priority, LOWEST FIRST: a caller too narrow for every
// column walks this list from the front and removes each entry until what is
// left fits.
//
// It is a fixed priority list and never a greedy packer, because greedy is
// non-monotone: a column can vanish when the terminal WIDENS. The price is
// that at some widths a table holds slack it cannot spend, and that is stated
// rather than hidden.
//
// IDX, ACCOUNT and the window columns are absent from it, and that absence IS
// the argument. IDX and ACCOUNT say WHICH account. Every window column says
// how much of one limit is gone, and the one most likely to matter is exactly
// the one that is spent, because that is the row a reader came to look at. A
// table too narrow for the block collapses it with CollapseWindows instead,
// which is safe where dropping is not: with every cell reading percentage
// USED, the worst window is the MAX, so nothing the collapsed cell hides is
// worse than what it shows.
//
// TIER goes first because it is the only column here that never changes: a
// plan name is a fact about the subscription and not about today's fleet, and
// a reader who needs it can ask for one account. The four after it keep the
// order the dashboard has always dropped them in, and the reset columns go
// from the LAST plan-order one back, so the rollover a reader is most likely
// to be waiting on -- the soonest, which sorts first -- is the last to go.
func ListDrops(c Columns) []ListColumn {
	drops := []ListColumn{
		{Kind: ColumnTier, Index: -1, Header: TierHeader, Content: tierContent},
		{Kind: ColumnAuto, Index: -1, Header: AutoHeader, Content: autoContent},
		{Kind: ColumnType, Index: -1, Header: TypeHeader, Content: typeContent},
		{Kind: ColumnAge, Index: -1, Header: AgeHeader, Content: ageContent},
		{Kind: ColumnState, Index: -1, Header: StateHeader, Content: stateContent},
	}
	for i := len(c.Resets) - 1; i >= 0; i-- {
		drops = append(drops, ListColumn{
			Kind: ColumnReset, Index: i, Header: c.Resets[i].Header, Content: resetContent,
		})
	}
	return drops
}

// CollapseWindows swaps the window columns for one ColumnWorst, in the place
// the first of them held, and leaves everything else where it was.
//
// It declines on a fleet whose only quota column is the placeholder, and that
// is arithmetic rather than taste: the placeholder is windowContent wide under
// a five-column heading and ColumnWorst is fifteen wide, so collapsing there
// would WIDEN the table a caller is collapsing because it is too narrow --
// and both cells say the same Unreadable either way.
func CollapseWindows(cols []ListColumn) []ListColumn {
	named := false
	for _, c := range cols {
		if c.Kind == ColumnWindow && c.Index >= 0 {
			named = true
			break
		}
	}
	if !named {
		return cols
	}
	out := make([]ListColumn, 0, len(cols))
	placed := false
	for _, c := range cols {
		if c.Kind != ColumnWindow {
			out = append(out, c)
			continue
		}
		if placed {
			continue
		}
		out = append(out, ListColumn{
			Kind: ColumnWorst, Index: -1, Header: WorstHeader, Content: worstContent,
		})
		placed = true
	}
	return out
}

// ListCell is one field of one row: the switch every surface drawing an
// account list used to own half of.
//
// hover selects the quota cells' form -- the bare percentage, or used against
// the threshold the row was measured with -- and nothing else. It is a
// parameter and not a field on Row because it is a fact about the REPORT being
// drawn rather than about the account.
//
// Three cells are deliberately shorter than what a surface finally prints, and
// in each case the missing half is something this package may not know:
//
//   - IDX carries Row.Marker, which answers "which login would a session get".
//     A surface with a CURSOR draws its own glyph in that column on the row the
//     cursor is on; the marker is what it falls back to, and where the two meet
//     the live account wins.
//   - ACCOUNT is the whole address-and-handle label, uncut and unpadded. What
//     it is cut to is a column width, which comes off a terminal.
//   - STATE is the WORD alone. The dashboard prefixes a glyph, and which glyph
//     set a console can carry is the machine fact this package never reads.
//
// AGE is the age and only the age. `ccdad status` rides Row.StatusFlags on
// this cell as a suffix, and that is right where it is: the flags belong to
// the ACCOUNT rather than to the column, and a suffix reads better beside that
// account's own figure than at a fixed offset far to its right.
func (r Row) ListCell(c ListColumn, block Columns, now time.Time, hover bool) string {
	switch c.Kind {
	case ColumnIdx:
		// See ColumnIdx: bare, because the section heading over this row is
		// already the provider half of the reference.
		return fmt.Sprintf("%s %d", r.Marker(), r.Account.Idx)
	case ColumnAccount:
		return r.ListLabel()
	case ColumnType:
		return r.TypeLabel()
	case ColumnTier:
		return r.TierLabel()
	case ColumnWindow:
		// The placeholder column stands for no window at all, so there is
		// nothing to look up and nothing was read.
		if c.Index < 0 {
			return Unreadable
		}
		n := block.Windows[c.Index].Name
		if hover {
			return r.hoverWindowCell(n)
		}
		return r.WindowCell(n)
	case ColumnReset:
		return r.ResetCell(block.Resets[c.Index], now)
	case ColumnWorst:
		return r.WorstCell(block)
	case ColumnState:
		return StateLabel(r.Engine.State)
	case ColumnAuto:
		return r.AutoLabel()
	case ColumnAge:
		return r.AgeLabel(now)
	case ColumnBlank:
		return ""
	}
	return ""
}

// hoverWindowCell is one quota cell under hover: used against the threshold
// this row was actually measured with.
//
// It is HoverCells' own body, lifted so the pair is spelled once. A second
// spelling of "%.0f%%/%.0f%%" would agree with the first until the day one of
// them changed.
func (r Row) hoverWindowCell(n usage.WindowName) string {
	pct, state := r.WindowPct(n)
	switch state {
	case WindowAbsent:
		return NoQuantity
	case WindowUnreadable:
		return Unreadable
	}
	return fmt.Sprintf("%.0f%%/%.0f%%", pct, r.WindowThreshold(n))
}

// WorstCell is the whole quota block in one cell, for a table too narrow to
// carry the block itself.
//
// It names the window as well as the number, because a percentage with no
// window beside it is precisely what these tables stopped doing.
//
// The trailing "+" is a claim about the claim: one window could not be read,
// so the max is a lower bound. Saying so costs one character and stops the
// cell asserting more than it knows.
func (r Row) WorstCell(c Columns) string {
	header, any, unread := "", false, false
	best := -1.0
	for _, w := range c.Windows {
		pct, state := r.WindowPct(w.Name)
		switch state {
		case WindowUnreadable:
			unread = true
		case WindowRead:
			if pct > best {
				best, header, any = pct, w.Header, true
			}
		}
	}
	if !any {
		return Unreadable
	}
	worst := fmt.Sprintf("%.0f%% %s", best, header)
	if unread {
		worst += "+"
	}
	return worst
}

// StateLabel is the word the STATE column prints for one engine state.
//
// The default arm is mandatory and is not defensive tidiness. AccountState is
// a string type and the status document is additive by contract, so a newer
// daemon may publish a state this binary has never heard of -- which happens
// on the day somebody upgrades one half of a machine. Carry the value through
// and render it.
//
// The empty string is its own arm and it is not an error either:
// AccountStatus.State is omitempty and is filled from a map lookup that
// answers the zero value on a miss, so an account no daemon has ever published
// carries "". It renders NoQuantity -- there is no state here, as against a
// state somebody tried to read and could not.
func StateLabel(s daemon.AccountState) string {
	switch s {
	case daemon.StateActive:
		return "active"
	case daemon.StateCandidate:
		return "candidate"
	case daemon.StateExhausted:
		return "exhausted"
	case daemon.StateEmpty:
		return "empty"
	case daemon.StateQuarantined:
		return "quarantined"
	case daemon.StateServing:
		return "serving"
	case daemon.StateNeedsRelogin:
		return "needs-relogin"
	case daemon.StateDisabled:
		return "disabled"
	case daemon.StateUnknown:
		return "unknown"
	case "":
		return NoQuantity
	}
	return string(s)
}

// AutoLabel is whether the engine may rotate to this account. It is a rotation
// policy and not a lock: an explicit `ccdad switch` still activates a disabled
// account.
func (r Row) AutoLabel() string {
	if r.Account.Disabled {
		return "no"
	}
	return "yes"
}

// The heading over each provider's rows.
//
// Bare, all-caps and 7-bit, six display columns and five, which is what makes
// them safe in the one place they are drawn: a section heading is a table row
// carrying its text in the ACCOUNT cell, and the narrowest ACCOUNT is ever
// squeezed to is twelve columns. Both fit inside twelve at every width, so a
// heading can never be cut -- and a cut heading would read as a provider name
// that is not one.
//
// Neither shares a substring with the `Codex:` that `ccdad status` prints in
// front of the codex half of the Active line, which three tests grep for
// case-sensitively to assert its ABSENCE. "CODEX" does not contain "Codex:"
// and "Codex:" does not contain "CODEX", so a sectioned table cannot make one
// of those greps pass or fail for a reason that has nothing to do with the
// line it is about.
const (
	ClaudeSection = "CLAUDE"
	CodexSection  = "CODEX"
)

// Section is one provider's rows under one heading.
type Section struct {
	Provider provider.ID
	Header   string
	// Rows are this section's account rows, each remembering where it came
	// from -- see ListRow.At for why that index has to survive the grouping.
	Rows []ListRow
}

// SectionLegend is the legend for one section, prefixed with the provider it
// belongs to.
//
// The prefix is what makes two legends readable as two: under a sectioned table
// with per-section columns there are now two "windows:" lines and nothing in the
// text of either says which half of the table it explains. It is the section's
// own Header, so the word a reader matches on is the word they just read over
// the rows.
//
// The provider is LOWERCASED here, and that is not a style choice. ClaudeSection
// and CodexSection are all-caps and 7-bit precisely so that a grep for one of
// them finds a heading and nothing else -- three tests assert the ABSENCE of a
// string by searching for it, and a legend carrying "CLAUDE" would answer those
// greps from under the table. Lowercase is the same word to a reader and a
// different string to a search.
//
// Empty when the section has no windows, which covers both the empty section and
// the one nobody could read -- there is no key to publish either way, and a
// legend naming the placeholder would be a mapping from QUOTA to nothing.
func SectionLegend(header string, c Columns) string {
	legend := c.Legend()
	if legend == "" {
		return ""
	}
	return "windows " + strings.ToLower(header) + ": " + strings.TrimPrefix(legend, "windows:  ")
}

// ListRow is one line of a rendered account list: an account row, or the
// heading over a section.
//
// The two are ONE type and one slice on purpose. A table draws its rows by
// integer and styles them by the same integer, so a heading that lived outside
// the row slice would have to be counted twice -- once by the renderer and
// once by the style function -- and the two counts would agree until the day a
// section became empty.
type ListRow struct {
	Row Row
	// Header is the section heading this line carries, and empty on every
	// account row. It is what tells the two apart.
	Header string
	// ColumnHeader marks the line of column names that follows a section
	// heading. It is a line of the table rather than the table's own header row
	// because there is now ONE PER SECTION: each provider's half draws its own
	// quota block, so each needs its own names over it, and a table library has
	// exactly one header row to give.
	//
	// It is a third kind of line rather than a second flag on Header for the
	// reason Header is not a nested list: every surface draws these by integer
	// and styles them by the same integer, so a line the renderer counts and the
	// style function does not is a line whose colour belongs to its neighbour.
	ColumnHeader bool
	// At is this account's index in the slice Sections was given, and -1 on a
	// heading.
	//
	// It has to be carried rather than derived because grouping REORDERS: a
	// codex account listed first in the store is drawn below every Claude one,
	// so a display position no longer names the same row. A cursor, a
	// selection or a per-account lookup that used the display position would
	// point at one account and act on another.
	At int
}

// Sections groups rows by provider: Claude first, then Codex.
//
// BOTH sections are always returned, including an empty one. A caller can then
// ask each for its rows without asking first whether it exists, and -- more to
// the point -- the total is the input by construction, so no grouping can lose
// a row.
//
// A row is Codex IF AND ONLY IF its account names the Codex provider.
// Everything else is Claude, and the zero provider is the case that matters: a
// row read out of a version-1 document carries no provider at all, and a
// grouping that treated the zero value as Codex would file every one of those
// accounts under the wrong heading, while one that gave them a third bucket
// would leave them out of both sections and off the page entirely. The store
// fills the zero value in as Claude when it loads, and this agrees with it
// rather than depending on it.
//
// Order WITHIN a section is the input's own, untouched. That order is the
// store's, which is what the IDX column numbers, so grouping changes which
// rows are adjacent and never which comes before which.
func Sections(rows []Row) []Section {
	claude := Section{Provider: provider.Claude, Header: ClaudeSection}
	codex := Section{Provider: provider.Codex, Header: CodexSection}
	for i, r := range rows {
		line := ListRow{Row: r, At: i}
		if r.Account.Provider == provider.Codex {
			codex.Rows = append(codex.Rows, line)
			continue
		}
		claude.Rows = append(claude.Rows, line)
	}
	return []Section{claude, codex}
}

// Columns is the quota block this section's own rows need.
//
// It is ColumnsOf over this section alone, which is the whole of the change: the
// union over a mixed fleet gives every Claude row a CX 1 cell it can only fill
// with "-" and every Codex row a 5H cell it can only fill the same way. Measured
// on a nine-account fleet: nine quota columns of which four were dead in each
// section, forty-two characters of table per row saying nothing.
//
// A section with no rows has no windows, and its block is the placeholder -- one
// column headed QUOTA. That is the same answer the whole-fleet block gives for a
// fleet nobody could read, and it is right here for the same reason: a heading
// over an empty section says the provider exists, and a heading over nothing at
// all with no columns under it would read as a table that forgot a provider.
func (s Section) Columns() Columns {
	rows := make([]Row, 0, len(s.Rows))
	for _, line := range s.Rows {
		rows = append(rows, line.Row)
	}
	return ColumnsOf(rows)
}

// ListRows is the sections as one drawable slice: each heading, then that
// section's accounts, in section order.
//
// EVERY heading is drawn, including the one over a section with no rows in it,
// and that is a decision rather than a fallout of the loop being simple. A
// heading over nothing says the provider exists and this machine has no account
// on it -- which is the question a reader of a one-provider fleet is most
// likely to be asking, and the answer they would otherwise have to know ccdad's
// history to infer. The alternative, drawing a heading only where there are
// rows under it, makes the page silent about exactly the case the sections were
// added for: a machine with four Claude accounts and no Codex one renders
// identically to a build that has never heard of Codex.
//
// Each section carries TWO lines of its own: the provider's name, and the column
// names under it. The second is what per-section quota blocks cost -- each half
// of the table draws its own windows, so each needs its own names over them --
// and it is a line of the table rather than the table's own header row because a
// table library has exactly one of those to give.
//
// The cost is four rows on a page that is short of them, and a surface short of
// rows gives them up like any other block rather than being handed a shorter
// list: the count is FIXED at two per section, so it can be budgeted, spent and
// handed back without knowing anything about the fleet.
func ListRows(secs []Section) []ListRow {
	n := 0
	for _, s := range secs {
		n += len(s.Rows) + 2
	}
	out := make([]ListRow, 0, n)
	for _, s := range secs {
		out = append(out, ListRow{Header: s.Header, At: -1})
		out = append(out, ListRow{ColumnHeader: true, At: -1})
		out = append(out, s.Rows...)
	}
	return out
}

// HoverNote is the sentence a hover table prints under itself, because the
// quota cells stop being a bare percentage there and nothing else says so.
const HoverNote = "hover:    quota cells show used/threshold; thresholds are derived per account and window"

// TrailerLines is everything printed UNDER an account table that is not a
// legend, in order: the hover sentences, the unranked note, and one credit line
// per credit-metered seat.
//
// The legend left this list when the table gained per-section columns. There is
// no longer ONE legend to print -- each provider's half of the table carries its
// own windows, so each carries its own mapping back to the wire keys -- and a
// list that still held a whole-fleet legend would print a third one naming
// columns no section draws. SectionLegend is where that lives now, and the
// surface emits one per section immediately under the table.
//
// The Columns handed in is still the WHOLE FLEET's, because the unranked note is
// a statement about the fleet rather than about a section: a window nothing
// ranks is not something a reader should have to notice twice.
//
// One ordered slice rather than a sequence of prints in each surface, because
// the order is a fact about the TABLE and not about the surface drawing it.
// Each line explains a column the reader is already looking at, and the two
// that describe the quota block come before the ones that describe a row.
//
// stranded is the second hover sentence, and it is passed rather than derived
// because it is a fact about the RANKING and not about these rows: it says
// some account's share is wider than the pool's slice, which is what lets two
// accounts at the same point of the same window carry different thresholds.
// Empty when no account is in that position. It rides inside the hover gate
// with the sentence it qualifies -- a table drawing bare percentages would
// otherwise explain a threshold none of its cells show.
//
// The lines arrive UNWRAPPED. Folding one onto a width is the caller's, and
// has to be: the width is a terminal's, and the label the fold hangs under is
// found in the line rather than passed, so a wrap belongs where the terminal
// is known.
// burn is Snapshot.BurnNote, passed for the same reason stranded is: it is a
// fact about the RANKING rather than about these rows, and it is empty on every
// fleet with no measurement. It comes second because the reader meets the
// thresholds first and the rate is what those thresholds were priced in.
func TrailerLines(rows []Row, c Columns, hover bool, stranded, burn string) []string {
	var out []string
	if hover {
		out = append(out, HoverNote)
		if burn != "" {
			out = append(out, burn)
		}
		if stranded != "" {
			out = append(out, "hover:    "+stranded)
		}
	}
	if note := c.UnrankedNote(); note != "" {
		out = append(out, note)
	}
	// A balance beats a percentage for a seat metered in money, and no
	// percentage column can carry one, so it goes under the table with the
	// account named beside it.
	for _, r := range rows {
		line, ok := r.CreditLine()
		if !ok {
			continue
		}
		out = append(out, "credit:   "+r.StatusLabel()+"  "+line)
	}
	return out
}

// The labels over the two active facts. They are separate lines rather than
// one, because a long account label may be cut and must not be able to consume
// the other provider beside it.
const (
	activeClaudeLabel = "Active (Claude): "
	activeCodexLabel  = "Active (Codex): "
)

// SummaryLine is one labelled fact from the block above an account table, kept
// as the PAIR rather than as a joined string.
//
// The pair is what lets a surface paint the label without painting the value,
// which is the rule the table one block down already pays: the column headings
// carry the heading role and the cells underneath carry their own. Painting
// the whole line as one span would make the fleet's answers the loudest thing
// on the page and would make "Active:" and the address it names read as one
// word. A caller that wants the line back joins them.
type SummaryLine struct {
	Label string
	Value string
}

// SummaryLines is who is live, what the engine is set to, and what it decided.
//
// Every fact owns one line, and Claude and Codex are separate facts: the codex
// line is present only when a codex account is actually being served, so a
// machine with one provider renders exactly what it rendered before there were
// two.
//
// The Strategy line is StrategyLabel and never Strategy: under hover the
// configured strategy has stopped being read, and naming it here made a page
// under a fully automatic mode look exactly like one that was not.
//
// The Current line is present only when the pass Decided. A zero Plan does not
// stringify to nothing -- it stringifies to plausible values, and the zero Mode
// is "headroom" -- so a line built from a pass that never ran would print a
// real answer nobody computed.
//
// Strategy and Current are built by CALLING StrategyLine and CurrentLine and
// splitting the result, rather than by spelling a label here and a value
// there. That is the whole point of the shape: those two sentences have one
// author, the surfaces that draw them get the pair for free, and a label that
// changed in one place cannot go on being the old one in another.
func (s Snapshot) SummaryLines() []SummaryLine {
	lines := []string{activeClaudeLabel + s.ActiveLabel}
	if s.CodexServingLabel != "" {
		lines = append(lines, activeCodexLabel+s.CodexServingLabel)
	}
	lines = append(lines, StrategyLine(s.StrategyLabel()))
	if s.HasMode {
		lines = append(lines, CurrentLine(s.Mode))
	}
	out := make([]SummaryLine, 0, len(lines))
	for _, line := range lines {
		label, value := splitLabel(line)
		out = append(out, SummaryLine{Label: label, Value: value})
	}
	return out
}
