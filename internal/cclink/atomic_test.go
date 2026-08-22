package cclink

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestWriteFileAtomicCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")

	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic() = %v, want nil", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("contents = %s, want {\"a\":1}", got)
	}
}

func TestWriteFileAtomicSetsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	path := filepath.Join(t.TempDir(), ".credentials.json")

	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// TestWriteFileAtomicHonoursPerm uses a mode os.CreateTemp cannot produce by
// accident (CreateTemp defaults to 0600), so it actually exercises the
// tmp.Chmod(perm) call rather than merely re-confirming CreateTemp's default.
func TestWriteFileAtomicHonoursPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := WriteFileAtomic(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"new":true}` {
		t.Fatalf("contents = %s, want the new value", got)
	}
}

// TestWriteFileAtomicRemovesItsTempFile checks that the temp file created
// during the write does not survive as a stray sibling once the rename
// succeeds. It does NOT prove the temp file was ever a sibling in the first
// place — a temp file created in os.TempDir() and cleaned up there would also
// leave this directory holding only the target file. Siblingness itself is
// checked by TestWriteFileAtomicCreatesTempBesideTarget below.
func TestWriteFileAtomicRemovesItsTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")

	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the target file", names)
	}
}

// TestWriteFileAtomicCreatesTempBesideTarget proves the temp file is a
// sibling of the target — the property the whole package exists for. A
// rename within one directory is atomic; a temp file created elsewhere (e.g.
// os.TempDir()) and moved in would degrade to copy-then-unlink across
// filesystems, where a reader can catch a half-written credential file. The
// temp file is gone by the time a test could inspect it after the fact, so
// this observes the directory createTemp is actually called with via a seam.
func TestWriteFileAtomicCreatesTempBesideTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")

	orig := createTemp
	t.Cleanup(func() { createTemp = orig })
	var gotDir string
	createTemp = func(d, pattern string) (*os.File, error) {
		gotDir = d
		return orig(d, pattern)
	}

	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if gotDir != dir {
		t.Fatalf("temp file created in %q, want the target's own directory %q", gotDir, dir)
	}
}

// TestWriteFileAtomicSyncsBeforeRename pins that the fsync actually happens,
// and that it happens before the rename. Crash durability itself is not
// observable from a Go test -- proving the sync's disk-flush guarantee would
// need OS-level fault injection -- but a refactor that drops the syncFile
// call, or moves it after the rename, is observable, and this test exists to
// catch exactly that regression.
func TestWriteFileAtomicSyncsBeforeRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")

	origSync := syncFile
	origRename := renameFile
	t.Cleanup(func() {
		syncFile = origSync
		renameFile = origRename
	})

	var events []string
	syncFile = func(f *os.File) error {
		events = append(events, "sync")
		return origSync(f)
	}
	renameFile = func(oldpath, newpath string) error {
		events = append(events, "rename")
		return origRename(oldpath, newpath)
	}

	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := []string{"sync", "rename"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v (sync must happen before the rename)", events, want)
		}
	}
}

func TestWriteFileAtomicFailsOnMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", ".credentials.json")

	if err := WriteFileAtomic(path, []byte("{}"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic() = nil, want an error for a missing directory")
	}
}

// TestWriteFileAtomicReaderNeverSeesPartialFile is the property this whole
// task exists to provide: a reader racing a writer must always see a complete
// payload — the old one or the new one — never a torn mix of the two. It
// alternates a small and a much larger payload so a non-atomic implementation
// (e.g. os.WriteFile, which writes in place without a rename) has a realistic
// chance of being caught mid-write.
func TestWriteFileAtomicReaderNeverSeesPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	small := []byte(`{"account":"a"}`)
	large := append(append([]byte(`{"account":"b","pad":"`), bytes.Repeat([]byte("x"), 1<<16)...), []byte(`"}`)...)
	if err := WriteFileAtomic(path, small, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			payload := small
			if i%2 == 1 {
				payload = large
			}
			if err := WriteFileAtomic(path, payload, 0o600); err != nil {
				t.Error(err)
				break
			}
		}
		close(stop)
	}()
	// On Windows the reader has to breathe. FILE_SHARE_DELETE stops it
	// BLOCKING the replace, but the replaced file then sits delete-pending
	// until that handle closes, and MoveFileEx onto a name in that state
	// answers ERROR_ACCESS_DENIED. A reader that reopens with no pause at all
	// keeps the name permanently in it, and the writer spends all ten of the
	// bounded retries §10.3 sizes for a TRANSIENT antivirus handle against
	// interference that never stops. Measured: this test failed on
	// windows-latest with "replacing .credentials.json: Access is denied".
	//
	// No pause off Windows: a rename there cannot be obstructed by an open
	// handle at all, and the tighter the loop the better this test is at
	// catching a torn write.
	var breathe time.Duration
	if runtime.GOOS == "windows" {
		breathe = time.Millisecond
	}
	for {
		select {
		case <-stop:
			wg.Wait()
			return
		default:
		}
		if breathe > 0 {
			time.Sleep(breathe)
		}
		got, err := readSharedDelete(path)
		if err != nil {
			t.Errorf("read: %v", err)
			break
		}
		if !bytes.Equal(got, small) && !bytes.Equal(got, large) {
			t.Errorf("reader saw a torn file: %d bytes, neither payload", len(got))
			break
		}
	}
	<-stop
	wg.Wait()
}

// TestWriteFileAtomicRetriesRetryableReplaceFailures drives the retry loop
// with a fake rename that fails twice and then delegates to the real
// os.Rename, and a fake retryable that recognizes the fake failure. This
// exercises the loop's own bookkeeping (it keeps trying, and eventually
// succeeds and writes the data) without depending on a real platform-specific
// sharing violation, so it runs on every OS.
func TestWriteFileAtomicRetriesRetryableReplaceFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")

	origRename := renameFile
	origRetryable := retryable
	t.Cleanup(func() {
		renameFile = origRename
		retryable = origRetryable
	})

	wantErr := errors.New("simulated transient replace failure")
	calls := 0
	renameFile = func(oldpath, newpath string) error {
		calls++
		if calls < 3 {
			return wantErr
		}
		return origRename(oldpath, newpath)
	}
	retryable = func(err error) bool { return errors.Is(err, wantErr) }

	if err := WriteFileAtomic(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic() = %v, want nil after retries succeed", err)
	}
	if calls != 3 {
		t.Fatalf("rename called %d times, want 3", calls)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("contents = %s, want the written value", got)
	}
}

// TestWriteFileAtomicGivesUpAfterReplaceAttempts proves the retry loop is
// bounded: a rename that always fails with a retryable error must not be
// retried forever, and must not give up after a single attempt either — both
// are real failure modes a broken loop could take.
func TestWriteFileAtomicGivesUpAfterReplaceAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")

	origRename := renameFile
	origRetryable := retryable
	t.Cleanup(func() {
		renameFile = origRename
		retryable = origRetryable
	})

	wantErr := errors.New("simulated permanent replace failure")
	calls := 0
	renameFile = func(oldpath, newpath string) error {
		calls++
		return wantErr
	}
	retryable = func(err error) bool { return errors.Is(err, wantErr) }

	err := WriteFileAtomic(path, []byte("{}"), 0o600)
	if err == nil {
		t.Fatal("WriteFileAtomic() = nil, want an error once every retry is exhausted")
	}
	if calls != replaceAttempts {
		t.Fatalf("rename called %d times, want exactly replaceAttempts (%d)", calls, replaceAttempts)
	}
}
