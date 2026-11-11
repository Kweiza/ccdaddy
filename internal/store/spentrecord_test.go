package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// A retry of an adoption that could not read the account's stored login knows
// nothing about which login was spent. It must not erase what an earlier
// attempt recorded: "" lifts the hold as soon as a stored login can be read
// again, and the one that can then be read may be the very login that is
// spent.
func TestAnUnknownSpentLoginDoesNotEraseAKnownOne(t *testing.T) {
	r := &SpentRecord{}
	r.RecordSpentStoredLogin("u-1", "hash-g0", "hash-g1")
	r.RecordSpentStoredLogin("u-1", "", "hash-g2")
	spent, newer, ok := r.SpentStoredLogin("u-1")
	if !ok || spent != "hash-g0" {
		t.Fatalf("spent = %q (%v), want the known hash-g0 kept", spent, ok)
	}
	if newer != "hash-g2" {
		t.Fatalf("newer = %q, want the latest newer login, hash-g2", newer)
	}
	// A KNOWN spent login is news, and replaces the old one.
	r.RecordSpentStoredLogin("u-1", "hash-g5", "hash-g6")
	if spent, _, _ := r.SpentStoredLogin("u-1"); spent != "hash-g5" {
		t.Fatalf("spent = %q, want the newly known hash-g5", spent)
	}
}

// A WRITE NEVER REPLACES A RECORD IT COULD NOT READ: that would put an empty
// record in its place and release every hold it kept, in every process. The
// file is left for a person, and the write is refused.
func TestAWriteNeverReplacesARecordItCouldNotRead(t *testing.T) {
	dir := withStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, SpentRecordFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WithSpentRecord(time.Second, func(r *SpentRecord) error {
		r.NameLogin("hash", "u-1")
		return nil
	})
	if err == nil {
		t.Fatal("WithSpentRecord wrote over a record it could not parse")
	}
	if got, _ := os.ReadFile(path); string(got) != "{not json" {
		t.Fatalf("the unreadable record was replaced:\n%s", got)
	}
}

// THE RECORD IS A FILE OF ITS OWN. A binary that predates it rewrites the
// engine state without the record's fields, and the record is still there; a
// record that cannot be read leaves the engine state -- cooldowns,
// quarantines, intents, the Codex lane -- readable and writable as before.
func TestTheRecordAndTheEngineStateDoNotShareAFile(t *testing.T) {
	dir := withStore(t)
	if err := WithSpentRecord(time.Second, func(r *SpentRecord) error {
		r.RecordSpentStoredLogin("u-1", "hash-g0", "hash-g1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An older binary writes the whole engine state, knowing nothing of the
	// record.
	if err := os.WriteFile(filepath.Join(dir, strategy.StateFileName), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadSpentRecord()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := r.SpentStoredLogin("u-1"); !ok {
		t.Fatal("rewriting the engine state dropped the record of spent stored logins")
	}

	if err := os.WriteFile(filepath.Join(dir, SpentRecordFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.WithState(time.Second, func(s *strategy.State) error {
		s.RecordSwitch("u-1", time.Now(), strategy.Departure{})
		return nil
	}); err != nil {
		t.Fatalf("a cooldown could not be stamped while the record was unreadable: %v", err)
	}
	st, err := strategy.LoadState()
	if err != nil || st.LoadError() != nil {
		t.Fatalf("the engine state reads as unreadable with the record: %v %v", err, st.LoadError())
	}
}
