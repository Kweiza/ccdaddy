package tui

import (
	"reflect"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/view"
)

func TestReloadKeepsTheSelectedAccountWhenAutomaticSortingMovesIt(t *testing.T) {
	snap := fixtureSnapshot(daemon.Report{})
	m := newModel(snap, 113, 34, paletteFor(fixtureOptions().Theme), glyphsFor(fixtureOptions()))
	m.Cursor = 1
	uuid := m.Snap.Rows[1].Account.UUID
	next := snap
	next.Rows = append([]view.Row(nil), snap.Rows...)
	next.Rows[0], next.Rows[1] = next.Rows[1], next.Rows[0]
	m = m.AfterLoad(next, nil)
	if m.Cursor != 0 || m.Snap.Rows[m.Cursor].Account.UUID != uuid {
		t.Fatalf("cursor moved to another account: %d", m.Cursor)
	}
}

func TestAutoSortKeyPersistsBothToggleDirections(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var ran [][]string
		o := fixtureOptions()
		o.Exec = recorder(&ran)
		a := newApp(o)
		a.m.Snap.AutoSort = enabled
		_, cmd, handled := a.key(keyPress("o"))
		if !handled || cmd == nil {
			t.Fatal("sort key did not produce a command")
		}
		cmd()
		value := "true"
		if enabled {
			value = "false"
		}
		if want := [][]string{{"config", "set", "auto_sort", value}}; !reflect.DeepEqual(ran, want) {
			t.Fatalf("commands = %v, want %v", ran, want)
		}
	}
}
