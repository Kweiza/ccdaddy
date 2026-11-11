package strategy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A state file that cannot be read is written over by the next write, as it
// was before the record of spent stored logins existed: the cooldown, the
// quarantines and the intents degrade towards more switching and recover with
// the next stamp. That record keeps a file of its own, which fails closed
// (store.SpentRecord), so nothing here has a reason to refuse.
func TestAnUnreadableStateIsWrittenOverByTheNextWrite(t *testing.T) {
	root := isolate(t)
	path := filepath.Join(root, StateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WithState(time.Second, func(s *State) error {
		s.RecordSwitch("a", now, Departure{})
		return nil
	}); err != nil {
		t.Fatalf("WithState = %v, want the unreadable state written over", err)
	}
	st, err := LoadState()
	if err != nil || st.LoadError() != nil {
		t.Fatalf("LoadState = %v, %v after the write", err, st.LoadError())
	}
	if at, to := st.LastSwitch(); to != "a" || !at.Equal(now) {
		t.Fatalf("the cooldown stamp did not land: %v %q", at, to)
	}
}
