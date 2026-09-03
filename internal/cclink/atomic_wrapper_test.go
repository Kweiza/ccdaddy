package cclink

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The writer moved to internal/atomicfile so that a package holding a switch
// can take the atomic write without taking the credentials file with it. This
// name stayed, because a dozen callers spell it and the move is not their
// business -- but a wrapper that stopped calling through would be silent: the
// callers would compile and write nothing.
func TestWriteFileAtomicStillWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic() = %v, want nil", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("file = %q, want the bytes handed to the wrapper", got)
	}
	// The mode half only, and not the whole test. What this test exists to
	// catch is a wrapper that stopped calling through -- which would compile,
	// leave every caller writing nothing, and be caught by the bytes above on
	// every platform there is. Skipping the function outright on Windows would
	// give that silence one OS to hide on for no gain, since the thing Windows
	// cannot answer is the permission and nothing else.
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// TempPattern is what daemon.SweepStatusTemps globs for. It moved with the
// writer and is re-exported here, so the two spellings cannot drift: a wrapper
// returning a pattern the writer does not produce would leave every orphaned
// temp file uncollected while the sweep reported success.
func TestTempPatternStillNamesTheWritersTempFile(t *testing.T) {
	if got, want := TempPattern("/a/b/status.json"), "status.json.tmp-*"; got != want {
		t.Fatalf("TempPattern() = %q, want %q", got, want)
	}
}
