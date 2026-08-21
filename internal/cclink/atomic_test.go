package cclink

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
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
	for {
		select {
		case <-stop:
			wg.Wait()
			return
		default:
		}
		got, err := os.ReadFile(path)
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
