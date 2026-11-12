package ccactivity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var base = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The newest transcript anywhere under projects is the answer, a subagent's
// included; files that are not transcripts do not count.
func TestTheNewestTranscriptWriteIsTheLastActivity(t *testing.T) {
	home := t.TempDir()
	touch(t, filepath.Join(home, "projects", "-a", "s1.jsonl"), base)
	touch(t, filepath.Join(home, "projects", "-b", "s2", "subagents", "x.jsonl"), base.Add(3*time.Minute))
	touch(t, filepath.Join(home, "projects", "-a", "notes.txt"), base.Add(time.Hour))
	at, ok := LastWrite(home, base.Add(time.Hour))
	if !ok || !at.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("LastWrite = %v, %v; want the subagent transcript's write", at, ok)
	}
}

// No transcript is no activity found, which the engine never reads as rest.
func TestNoTranscriptIsNothingFound(t *testing.T) {
	if _, ok := LastWrite(t.TempDir(), base); ok {
		t.Fatal("found activity in an empty config root")
	}
}

// A project directory that is a symlink is walked: a session writing there is
// as live as any other.
func TestASymlinkedProjectIsWalked(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(home, "projects", "-a", "old.jsonl"), base)
	touch(t, filepath.Join(elsewhere, "new.jsonl"), base.Add(5*time.Minute))
	if err := os.Symlink(elsewhere, filepath.Join(home, "projects", "-linked")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if at, ok := LastWrite(home, base.Add(time.Hour)); !ok || !at.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("LastWrite = %v, %v; want the write behind the link", at, ok)
	}
}

// A directory the walk cannot read may hold the newest write, so a walk that
// met one finds nothing rather than an older write it could see.
func TestAnUnreadableProjectIsNothingFound(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("mode bits do not lock this user out here")
	}
	home := t.TempDir()
	touch(t, filepath.Join(home, "projects", "-a", "old.jsonl"), base)
	locked := filepath.Join(home, "projects", "-locked")
	touch(t, filepath.Join(locked, "new.jsonl"), base.Add(5*time.Minute))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if at, ok := LastWrite(home, base.Add(time.Hour)); ok {
		t.Fatalf("LastWrite = %v, found; want nothing found past an unreadable project", at)
	}
}

// A transcript dated past the clock is a clock that moved, not a write: it is
// passed over rather than standing as the newest write until the clock catches
// up.
func TestAFutureMtimeIsPassedOver(t *testing.T) {
	home := t.TempDir()
	touch(t, filepath.Join(home, "projects", "-a", "s.jsonl"), base)
	touch(t, filepath.Join(home, "projects", "-b", "future.jsonl"), base.AddDate(1, 0, 0))
	if at, ok := LastWrite(home, base.Add(time.Hour)); !ok || !at.Equal(base) {
		t.Fatalf("LastWrite = %v, %v; want the write that is not in the future", at, ok)
	}
}

// Last reuses a walk only while it says the work is active. An answer that
// would read as rest is walked again, so a session that resumed after the
// cached walk is seen at once.
func TestLastReusesOnlyAnActiveAnswer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	const quiet = 10 * time.Minute
	path := filepath.Join(home, "projects", "-a", "s.jsonl")
	touch(t, path, base)

	now := base.Add(5 * time.Minute)
	if at, ok := Last(now, quiet); !ok || !at.Equal(base) {
		t.Fatalf("Last = %v, %v", at, ok)
	}
	touch(t, path, base.Add(6*time.Minute))
	if at, _ := Last(now.Add(time.Second), quiet); !at.Equal(base) {
		t.Fatalf("an active answer inside the window: Last = %v, want the cached %v", at, base)
	}
	// Ten minutes after the cached write the cached answer would read as rest:
	// it is walked again, and the newer write is seen.
	if at, _ := Last(base.Add(quiet), quiet); !at.Equal(base.Add(6 * time.Minute)) {
		t.Fatalf("Last = %v, want the newer write rather than a cached rest", at)
	}
}
