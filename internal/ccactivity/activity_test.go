package ccactivity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	touch(t, filepath.Join(home, "projects", "-a", "s1.jsonl"), base)
	touch(t, filepath.Join(home, "projects", "-b", "s2", "subagents", "x.jsonl"), base.Add(3*time.Minute))
	touch(t, filepath.Join(home, "projects", "-a", "notes.txt"), base.Add(time.Hour))
	at, ok := LastWrite(home)
	if !ok || !at.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("LastWrite = %v, %v; want the subagent transcript's write", at, ok)
	}
}

// No transcript is no activity found, which the engine never reads as rest.
func TestNoTranscriptIsNothingFound(t *testing.T) {
	if _, ok := LastWrite(t.TempDir()); ok {
		t.Fatal("found activity in an empty config root")
	}
}

// Last reads Claude Code's own root and holds its answer for cacheFor.
func TestLastIsCachedForItsWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(home, "projects", "-a", "s.jsonl")
	touch(t, path, base)
	now := base.Add(20 * time.Minute)
	if at, ok := Last(now); !ok || !at.Equal(base) {
		t.Fatalf("Last = %v, %v", at, ok)
	}
	touch(t, path, base.Add(19*time.Minute))
	if at, _ := Last(now.Add(cacheFor - time.Second)); !at.Equal(base) {
		t.Fatalf("inside the window Last = %v, want the cached %v", at, base)
	}
	if at, _ := Last(now.Add(cacheFor)); !at.Equal(base.Add(19 * time.Minute)) {
		t.Fatalf("after the window Last = %v, want the new write", at)
	}
}
