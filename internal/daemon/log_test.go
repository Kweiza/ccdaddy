package daemon

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// openTestLog opens the daemon log with a cap small enough to reach in a test.
// The default cap is pinned separately against a literal, so shrinking it here
// cannot shrink the assertion that it is what the design says.
func openTestLog(t *testing.T, max int64, keep int) *Logger {
	t.Helper()
	l, err := openLog(LogPath(), max, keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	l.now = func() time.Time { return at("2026-08-22T05:00:00Z") }
	return l
}

func TestLogAppendsToItsOwnFile(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 1<<20, 3)
	l.Printf("started, pid %d", 42)
	l.Printf("tick %d", 1)

	body := readFile(t, LogPath())
	if !strings.Contains(body, "started, pid 42") || !strings.Contains(body, "tick 1") {
		t.Fatalf("lines missing from the log:\n%s", body)
	}
	if !strings.Contains(body, "2026-08-22T05:00:00") {
		t.Errorf("a log line carries no timestamp:\n%s", body)
	}
	if strings.Count(body, "\n") != 2 {
		t.Errorf("want one line per Printf, got:\n%s", body)
	}
}

func TestLogReopensAnExistingFileWithoutTruncatingIt(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 1<<20, 3)
	l.Printf("before the restart")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	again := openTestLog(t, 1<<20, 3)
	again.Printf("after the restart")
	body := readFile(t, LogPath())
	if !strings.Contains(body, "before the restart") {
		t.Fatalf("reopening the log truncated it:\n%s", body)
	}
	if !strings.Contains(body, "after the restart") {
		t.Fatalf("the reopened log is not being written:\n%s", body)
	}
}

func TestLogDoesNotRotateBelowTheCap(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 1<<20, 3)
	l.Printf("small")

	rotated, err := l.RotateIfLarge()
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Fatal("a small log was rotated")
	}
	if _, err := os.Stat(LogPath() + ".1"); !os.IsNotExist(err) {
		t.Error("a rotated copy exists although nothing was rotated")
	}
}

func TestLogRotatesAtTheCap(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 64, 3)
	l.Printf("%s", strings.Repeat("x", 200))

	rotated, err := l.RotateIfLarge()
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("a log over the cap was not rotated")
	}
	if !strings.Contains(readFile(t, LogPath()+".1"), "xxx") {
		t.Error("the rotated copy does not hold what the log held")
	}
	if body := readFile(t, LogPath()); body != "" {
		t.Errorf("the fresh log is not empty:\n%s", body)
	}
}

// The trap the brief names. Rotation by rename leaves an already-open
// descriptor pointing at the RENAMED inode, so a daemon that does not reopen
// keeps writing into daemon.log.1 forever while daemon.log stays 0 bytes —
// silently discarding every line it will ever log again.
func TestPostRotationWritesLandInTheNewFile(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 64, 3)
	l.Printf("%s", strings.Repeat("x", 200))
	if _, err := l.RotateIfLarge(); err != nil {
		t.Fatal(err)
	}

	l.Printf("this line is after the rotation")

	if got := readFile(t, LogPath()); !strings.Contains(got, "after the rotation") {
		t.Fatalf("the post-rotation line did not land in the new daemon.log:\n%s", got)
	}
	if got := readFile(t, LogPath()+".1"); strings.Contains(got, "after the rotation") {
		t.Fatal("the post-rotation line landed in the rotated inode; the daemon never reopened")
	}
}

func TestLogKeepsABoundedNumberOfRotations(t *testing.T) {
	isolate(t)
	const keep = 3
	l := openTestLog(t, 8, keep)
	for i := range keep + 2 {
		l.Printf("generation %d %s", i, strings.Repeat("y", 40))
		if rotated, err := l.RotateIfLarge(); err != nil || !rotated {
			t.Fatalf("rotation %d: rotated=%v err=%v", i, rotated, err)
		}
	}
	for i := 1; i <= keep; i++ {
		if _, err := os.Stat(LogPath() + "." + strconv.Itoa(i)); err != nil {
			t.Errorf("daemon.log.%d is missing: %v", i, err)
		}
	}
	if _, err := os.Stat(LogPath() + "." + strconv.Itoa(keep+1)); !os.IsNotExist(err) {
		t.Errorf("daemon.log.%d survived; the kept count is unbounded", keep+1)
	}
	// The newest rotation holds the newest generation: the shift must go from
	// the oldest end, or every copy is overwritten by its neighbour.
	if got := readFile(t, LogPath()+".1"); !strings.Contains(got, "generation 4") {
		t.Errorf("daemon.log.1 is not the most recent rotation:\n%s", got)
	}
}

// The comparison itself, not just "it rotates eventually". A cap that is only
// exercised by a log ten times over it lets the threshold drift by any factor
// without a test noticing.
func TestTheRotationThresholdIsTheCapExactly(t *testing.T) {
	isolate(t)
	measure := openTestLog(t, 1<<20, 3)
	measure.Printf("ab")
	info, err := os.Stat(LogPath())
	if err != nil {
		t.Fatal(err)
	}
	line := info.Size()
	if err := measure.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		max  int64
		want bool
	}{
		{"exactly at the cap", line, true},
		{"one byte short of it", line + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			l := openTestLog(t, tc.max, 3)
			l.Printf("ab")
			rotated, err := l.RotateIfLarge()
			if err != nil {
				t.Fatal(err)
			}
			if rotated != tc.want {
				t.Errorf("a %d-byte log against a cap of %d: rotated=%v, want %v", line, tc.max, rotated, tc.want)
			}
		})
	}
}

// "If large" needs an actual number, and an assertion written in terms of the
// constant it is checking moves with it. This one is a literal on purpose.
func TestDefaultLogCapIsTheDocumentedNumber(t *testing.T) {
	if maxLogSize != 8*1024*1024 {
		t.Errorf("maxLogSize = %d, want 8 MiB", maxLogSize)
	}
	if keepRotated != 3 {
		t.Errorf("keepRotated = %d, want 3", keepRotated)
	}
}

func TestLogFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows and nothing may depend on the mode")
	}
	isolate(t)
	l := openTestLog(t, 64, 3)
	l.Printf("%s", strings.Repeat("x", 200))
	if _, err := l.RotateIfLarge(); err != nil {
		t.Fatal(err)
	}
	l.Printf("after")

	for _, path := range []string{LogPath(), LogPath() + ".1"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, got)
		}
	}
}

// The tick loop is not the only writer: pollers run as their own goroutines
// (§8.4) and every one of them logs.
func TestLogIsSafeForConcurrentWriters(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 1<<20, 3)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				l.Printf("writer %d line %d", i, j)
			}
		}()
	}
	wg.Wait()
	if got := strings.Count(readFile(t, LogPath()), "\n"); got != 160 {
		t.Errorf("wrote 160 lines, log holds %d", got)
	}
}

func TestRotationIsSafeAgainstAConcurrentWriter(t *testing.T) {
	isolate(t)
	l := openTestLog(t, 128, 3)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			l.Printf("line")
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			if _, err := l.RotateIfLarge(); err != nil {
				t.Errorf("RotateIfLarge: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
