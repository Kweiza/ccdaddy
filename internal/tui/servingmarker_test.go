package tui

import (
	"strings"
	"testing"
)

// The Codex section marks the account being served from with the "*" the
// Claude section puts on the live login, and prints each row's Ref.
func TestTheCodexSectionMarksTheServingAccount(t *testing.T) {
	m := fixtureModel(113, 26)
	m.Cursor = 0 // on the live Claude row, so no cursor glyph is drawn anywhere else
	body := m.Body()
	if !strings.Contains(body, "* x1 cx@example.com") {
		t.Errorf("the serving codex row is not drawn as %q:\n%s", "* x1 cx@example.com", body)
	}
	if !strings.Contains(body, "* c1 work@example.com") {
		t.Errorf("the live claude row is not drawn as %q:\n%s", "* c1 work@example.com", body)
	}
}

// Where the cursor meets the serving row, the mark wins, by the rule noCursor
// states for the live Claude login: it answers which account a new session
// gets, and the cursor answers where the reader left the highlight.
func TestTheServingMarkWinsOverTheCursor(t *testing.T) {
	m := fixtureModel(113, 26)
	for i, r := range m.Snap.Rows {
		if r.Serving {
			m.Cursor = i
		}
	}
	if !m.Snap.Rows[m.Cursor].Serving {
		t.Fatal("the fixture has no serving row")
	}
	if body := m.Body(); !strings.Contains(body, "* x1 cx@example.com") {
		t.Errorf("the cursor replaced the serving mark:\n%s", body)
	}
}
