package codexlaunch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUnroutedCountIsZeroOnAMachineThatHasNeverHadOne(t *testing.T) {
	if got := UnroutedCount(t.TempDir()); got != 0 {
		t.Errorf("UnroutedCount() = %d on a store with no counter file, want 0", got)
	}
}

func TestNoteUnroutedCounts(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 3; i++ {
		if err := NoteUnrouted(root); err != nil {
			t.Fatalf("NoteUnrouted() = %v", err)
		}
		if got := UnroutedCount(root); got != i {
			t.Errorf("after %d launches UnroutedCount() = %d", i, got)
		}
	}
}

// Two launchers can start at the same moment, and a read-add-write from each
// would lose one of the two -- which is the whole reason this is an appended
// byte rather than a number that is parsed and rewritten.
func TestNoteUnroutedLosesNothingUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := NoteUnrouted(root); err != nil {
				t.Errorf("NoteUnrouted() = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := UnroutedCount(root); got != n {
		t.Errorf("UnroutedCount() = %d after %d concurrent launches", got, n)
	}
}

// The file sits beside the launch records, under the same `codex` directory, so
// `ccdad uninstall` takes it with the one name already in storeMarkers.
func TestTheUnroutedCounterSitsBesideTheLaunchRecords(t *testing.T) {
	root := t.TempDir()
	if got, want := filepath.Dir(UnroutedPath(root)), filepath.Dir(Dir(root)); got != want {
		t.Errorf("UnroutedPath is in %s, want it beside the launch records in %s", got, want)
	}
}

// A damaged file is a wrong count and never an error: the only consumer is a
// status document, and refusing to publish one because of a byte in a tally
// would take the whole document down with it.
func TestUnroutedCountToleratesRubbish(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(UnroutedPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UnroutedPath(root), []byte("not a number at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := UnroutedCount(root); got != 0 {
		t.Errorf("UnroutedCount() = %d for a file with no newline in it, want 0", got)
	}
}
