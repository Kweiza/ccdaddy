package tui

import (
	"slices"
	"strconv"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// This file is the dashboard's half of `ccdad move`: pick a row up with m,
// carry it with the arrow keys, put it down with enter.
//
// It is a MODE on the page and not a sixth screen, and that is the decision
// everything else here follows from. A reorder is judged against the rows it
// lands between -- which account is now above this one, which quota block it
// sits in -- so a picker listing the same accounts a second time would take
// away the only thing the reader needs to see. The page draws itself with one
// row in hand instead.
//
// Nothing here writes to the store. Snap.Rows is reordered as a PREVIEW, and
// the store learns about it exactly once, from the same `move` command a user
// could have typed, released through the same executor every other key on this
// page releases its command through. That is what keeps the dashboard a reader
// of the store rather than a second writer to it -- and it is why cancelling
// costs nothing to undo: nothing was done.

// grabRun is the half-open range of Snap.Rows a row may be carried within: the
// run of adjacent rows that carry its own provider.
//
// A move is bounded by the provider because the ordinal it sets is per
// provider, and because the table is drawn in provider sections -- a row that
// could be carried across the boundary would have to change section, quota
// block and number all at once, and the position the user had chosen on screen
// would not be the position the command asked for.
//
// It is found by walking OUT from the row rather than by filtering the slice
// for a provider, and the difference is the invariant it rests on: the store
// groups its accounts, so one provider's rows are one contiguous run. Walking
// answers with the run that is actually adjacent on the page, which is the run
// the arrow keys can reach; a filter would answer with rows the cursor cannot
// walk to if that grouping ever failed, and the swap below would then move a
// row past one it never passed on screen.
func (m Model) grabRun(at int) (start, end int) {
	rows := m.Snap.Rows
	if at < 0 || at >= len(rows) {
		return 0, 0
	}
	p := rows[at].Account.Provider
	start, end = at, at+1
	for start > 0 && rows[start-1].Account.Provider == p {
		start--
	}
	for end < len(rows) && rows[end].Account.Provider == p {
		end++
	}
	return start, end
}

// nothingToReorder is what m says on a provider holding one account.
//
// A sentence rather than a mode that opens and answers nothing. Entering a
// reorder where both arrow keys are no-ops leaves the reader pressing them at
// a page that cannot move, with the keybar promising it can -- and this is the
// same answer `s` gives on the row that is already live, for the same reason.
const nothingToReorder = "That is the only account under its provider, so there is nothing to reorder. " +
	"Move the cursor to a provider with two or more accounts and press m again."

// renumbered renumbers a previewed order the way the store would, so the IDX
// column agrees with the rows the reader is looking at.
//
// Without it the preview shows the order it is about to ask for and the numbers
// it has not asked for yet: carrying the first Claude row down one draws it
// second with a 1 still in its IDX cell, above a row numbered 2. That is a
// table stating two different orders at once, in the one moment a user is
// deciding between them.
//
// It is the same rule the store applies -- count from 1 within each provider,
// in slice order -- restated here because the store is not in this loop. The
// two cannot drift far: what this draws is checked against what `ccdad move`
// then stores, on the very next refresh.
func renumbered(rows []view.Row) {
	next := make(map[provider.ID]int, 2)
	for i := range rows {
		p := rows[i].Account.Provider
		next[p]++
		rows[i].Account.Idx = next[p]
	}
}

// grabbing is the m key: it picks the row under the cursor up.
//
// The PREVIEW is the clone and the backup is the snapshot's own slice, rather
// than the other way round. Everything from here on reorders and renumbers rows
// in place, and Options.Load is not promised to hand back a fresh slice every
// time -- so mutating the one it gave us would edit whatever the caller still
// holds, and a cancelled move would leave the store's own order permanently
// changed in memory.
func (a App) grabbing() App {
	uuid := a.m.cursorUUID()
	if uuid == "" {
		// The cursor is on no account -- an empty store, or a page whose rows
		// have not arrived. Nothing to reorder, and nothing to say that the
		// empty table is not already saying. Switch answers the same way.
		return a
	}
	start, end := a.m.grabRun(a.m.Cursor)
	if end-start < 2 {
		return a.saying(nothingToReorder)
	}
	a.moveBackup = a.m.Snap.Rows
	a.m.Snap.Rows = slices.Clone(a.m.Snap.Rows)
	a.moveFrom = a.m.Cursor
	a.m.Moving = true
	return a
}

// carrying is one arrow key while a row is in hand: the row swaps with its
// neighbour and the cursor goes with it.
//
// The cursor MOVES WITH THE ROW rather than staying at the index, which is the
// whole difference between carrying a row and walking a list. A cursor left
// behind would put the marker on the account that was displaced, and the next
// press would carry that one instead.
//
// A press at either end of the provider's run is answered by doing nothing.
// The alternative -- wrapping, or crossing into the other provider -- would
// move the row somewhere the user did not point at.
func (a App) carrying(delta int) App {
	start, end := a.m.grabRun(a.m.Cursor)
	to := a.m.Cursor + delta
	if to < start || to >= end {
		return a
	}
	rows := a.m.Snap.Rows
	rows[a.m.Cursor], rows[to] = rows[to], rows[a.m.Cursor]
	renumbered(rows)
	a.m.Cursor = to
	a.m = scrolled(a.m)
	return a
}

// releasing is esc: the preview is thrown away and the page goes back to the
// order the store still holds.
func (a App) releasing() App {
	a.m.Snap.Rows = a.moveBackup
	a.m.Cursor = a.moveFrom
	a.moveBackup, a.moveFrom = nil, 0
	a.m.Moving = false
	a.m = scrolled(a.m)
	return a
}

// placing is enter: the row is put down and the store is asked to agree.
//
// The account travels BY UUID and the destination as a position, which is the
// same split `s` makes and for the same reason: a display position names a
// different account on either side of the grouping, and the argv has to name
// the account the reader was looking at. The position is counted from the
// start of the provider's own run, because that is what `ccdad move` counts.
//
// A row put back where it started releases NOTHING. `ccdad move` answers that
// with exit 3 and a sentence, and spending a command -- and a panel over the
// page -- to be told that nothing happened is worse than the mode simply
// ending, which the keybar and the marker both say has happened.
func (a App) placing() (App, tea.Cmd) {
	at, from := a.m.Cursor, a.moveFrom
	uuid := a.m.cursorUUID()
	start, _ := a.m.grabRun(at)
	a.m.Moving = false
	a.moveBackup, a.moveFrom = nil, 0
	if at == from || uuid == "" {
		return a, nil
	}
	return a.starting([]string{"move", uuid, strconv.Itoa(at - start + 1)})
}

// movingKey is every key answered while a row is in hand, and it answers ALL of
// them.
//
// Nothing falls through to the page's own switch, and that is the point rather
// than an omission. The keys underneath include `s`, which moves a credential,
// and `a`, which releases the terminal to a login -- both against a list whose
// order is a preview of something the store has never been told about. A
// keystroke that reached them would act on a page that is not the page the
// store holds.
//
// The four it does answer are exactly the four MovingHelp advertises, and the
// keybar is what tells the reader so: the bar is replaced for the duration, so
// no key it names is missing here and no key missing here is named there.
func (a App) movingKey(msg tea.KeyPressMsg, k KeyMap) (App, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, k.Up):
		return a.carrying(-1), nil, true
	case key.Matches(msg, k.Down):
		return a.carrying(1), nil, true
	case key.Matches(msg, k.Enter):
		next, cmd := a.placing()
		return next, cmd, true
	case key.Matches(msg, k.Esc):
		return a.releasing(), nil, true
	}
	// Swallowed, and reported as handled so that nothing below this point sees
	// a key pressed against a previewed order.
	return a, nil, true
}
