package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A retry of an adoption that could not read the account's stored login knows
// nothing about which login was spent. It must not erase what an earlier
// attempt recorded: "" lifts the hold as soon as a stored login can be read
// again, and the one that can then be read may be the very login that is
// spent.
func TestAnUnknownSpentLoginDoesNotEraseAKnownOne(t *testing.T) {
	st := NewState()
	st.RecordSpentStoredLogin("u-1", "hash-g0", "hash-g1")
	st.RecordSpentStoredLogin("u-1", "", "hash-g2")
	spent, newer, ok := st.SpentStoredLogin("u-1")
	if !ok || spent != "hash-g0" {
		t.Fatalf("spent = %q (%v), want the known hash-g0 kept", spent, ok)
	}
	if newer != "hash-g2" {
		t.Fatalf("newer = %q, want the latest newer login, hash-g2", newer)
	}
	// A KNOWN spent login is news, and replaces the old one.
	st.RecordSpentStoredLogin("u-1", "hash-g5", "hash-g6")
	if spent, _, _ := st.SpentStoredLogin("u-1"); spent != "hash-g5" {
		t.Fatalf("spent = %q, want the newly known hash-g5", spent)
	}
}

// A WRITE NEVER REPLACES A STATE IT COULD NOT READ. The state holds which
// stored logins are spent, and a write that took an unreadable file for an
// empty one would put an empty record in its place -- every hold in it gone,
// in every process, with nothing to say so. The file is left for a person to
// repair or remove (`ccdad doctor` names it), and the write is refused.
func TestAWriteNeverReplacesAStateItCouldNotRead(t *testing.T) {
	root := isolate(t)
	path := filepath.Join(root, StateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("a", now, Departure{})
		return nil
	})
	if err == nil {
		t.Fatal("WithState wrote over a state it could not parse")
	}
	if got, _ := os.ReadFile(path); string(got) != "{not json" {
		t.Fatalf("the unreadable state was replaced:\n%s", got)
	}
}
