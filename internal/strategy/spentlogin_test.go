package strategy

import "testing"

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
