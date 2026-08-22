package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A kernel object name may not contain a backslash past its namespace prefix,
// and every Windows store path has several — so a name that embedded the path
// would fail to create at all, on Windows only, where nothing in this tree can
// see it happen.
func TestShutdownEventNameHasNoPathInIt(t *testing.T) {
	name := shutdownEventNameFor(`C:\Users\someone\.ccdad`)
	if !strings.HasPrefix(name, shutdownEventPrefix) {
		t.Fatalf("name = %q, want the %q prefix", name, shutdownEventPrefix)
	}
	if rest := strings.TrimPrefix(name, shutdownEventPrefix); strings.ContainsAny(rest, `\/`) {
		t.Errorf("the name carries a path separator past the namespace prefix: %q", name)
	}
	if len(name) > 128 {
		t.Errorf("name is %d characters; object names are capped at 260 and a deep store must not reach it", len(name))
	}
}

// The daemon and the process trying to stop it derive the name independently.
// If two spellings of one store produced two names, `ccdad daemon stop` would
// open an event nobody is listening on and report that no daemon is there.
func TestShutdownEventNameIsOneNamePerStore(t *testing.T) {
	want := shutdownEventNameFor(`C:\Users\someone\.ccdad`)
	for _, spelling := range []string{
		`C:\Users\someone\.ccdad\`,
		`C:\Users\someone\.ccdad\.`,
		`C:\Users\someone\other\..\.ccdad`,
		`c:\users\someone\.ccdad`,
	} {
		if got := shutdownEventNameFor(spelling); got != want {
			t.Errorf("%q gives %q, want the same name as the canonical spelling (%q)", spelling, got, want)
		}
	}
}

// The daemon was handed a resolved store by ChildEnv and the CLI may have been
// given a symlink to the same directory. If the two derived different names,
// `ccdad daemon stop` would open an event nobody is listening on and report
// that no daemon is there — while one is.
func TestShutdownEventNameResolvesTheStore(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this platform cannot make a symlink: %v", err)
	}
	t.Setenv("CCDAD_HOME", link)

	got, err := shutdownEventName()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := shutdownEventNameFor(resolved); got != want {
		t.Errorf("shutdownEventName() = %q, want the resolved store's name %q", got, want)
	}
}

func TestShutdownEventNameSeparatesDifferentStores(t *testing.T) {
	a := shutdownEventNameFor(`C:\Users\someone\.ccdad`)
	b := shutdownEventNameFor(`C:\Users\someone-else\.ccdad`)
	if a == b {
		t.Fatalf("two different stores share one event name: %q", a)
	}
}

// The cross-check, branch by branch. This is the whole of what can be verified
// off Windows, and it is the part whose failure kills an innocent process.
func TestMayTerminateFailsClosed(t *testing.T) {
	started := mustTimeAt(t, "2026-08-22T12:00:00Z")
	ok := shutdownTarget{PID: 4321, Image: `C:\ccdad\ccdad.exe`, StartedAt: started}

	cases := []struct {
		name  string
		want  shutdownTarget
		got   processFacts
		allow bool
		says  string
	}{
		{
			name:  "the same process, created just before it published",
			want:  ok,
			got:   processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started.Add(-20 * time.Millisecond)},
			allow: true,
		},
		{
			name:  "the same image by a different path, which is an upgrade in place",
			want:  ok,
			got:   processFacts{Image: `C:\Program Files\ccdad\CCDAD.EXE`, CreatedAt: started.Add(-time.Second)},
			allow: true,
		},
		{
			name:  "a creation time a moment after the published start, which is clock granularity",
			want:  ok,
			got:   processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started.Add(time.Second)},
			allow: true,
		},
		{
			name: "a recycled pid running something else",
			want: ok,
			got:  processFacts{Image: `C:\Windows\System32\notepad.exe`, CreatedAt: started.Add(-time.Second)},
			says: "not ccdad.exe",
		},
		{
			name: "a recycled pid created after the daemon published",
			want: ok,
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started.Add(time.Hour)},
			says: "the pid has been reused",
		},
		{
			name: "a process far older than the daemon's own start",
			want: ok,
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started.Add(-2 * time.Hour)},
			says: "before the daemon published",
		},
		{
			name: "a process that would not say what it is",
			want: ok,
			got:  processFacts{CreatedAt: started},
			says: "would not say",
		},
		{
			name: "a process whose creation time could not be read",
			want: ok,
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`},
			says: "creation time",
		},
		{
			name: "no published start time at all",
			want: shutdownTarget{PID: 4321, Image: `C:\ccdad\ccdad.exe`},
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started},
			says: "cannot be ruled out",
		},
		{
			name: "our own image could not be determined",
			want: shutdownTarget{PID: 4321, StartedAt: started},
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started},
			says: "could not be determined",
		},
		{
			name: "not a pid",
			want: shutdownTarget{PID: 0, Image: `C:\ccdad\ccdad.exe`, StartedAt: started},
			got:  processFacts{Image: `C:\ccdad\ccdad.exe`, CreatedAt: started},
			says: "is not a pid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, why := mayTerminate(tc.want, tc.got)
			if allowed != tc.allow {
				t.Fatalf("mayTerminate() = (%v, %q), want allowed=%v", allowed, why, tc.allow)
			}
			if tc.allow {
				if why != "" {
					t.Errorf("an allowed terminate carried a reason: %q", why)
				}
				return
			}
			if !strings.Contains(why, tc.says) {
				t.Errorf("the refusal says %q, want it to mention %q", why, tc.says)
			}
		})
	}
}

// ForceShutdown assembles the target itself so that a caller cannot hand it one
// with a check left out. Two of its refusals are reachable on every platform,
// which is the only way they get tested at all.
func TestForceShutdownRefusesAPidThatIsNotOne(t *testing.T) {
	isolate(t)
	for _, pid := range []int{0, -1} {
		err := ForceShutdown(pid)
		if err == nil {
			t.Errorf("ForceShutdown(%d) = nil, want a refusal", pid)
			continue
		}
		// Not just "an error": on Unix every path through here ends in
		// errors.ErrUnsupported, so a test that only checked for non-nil would
		// pass whether or not the guard existed. It has to be THIS refusal.
		if errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("ForceShutdown(%d) reached the platform refusal, so the pid was never checked: %v", pid, err)
		}
	}
}

// A published document describing a DIFFERENT pid is not evidence about this
// one, and its start time is the anchor the creation-time cross-check rests on.
// Terminating on another process's start time is the recycled-pid hazard with
// extra steps.
func TestForceShutdownRefusesAStatusDescribingAnotherProcess(t *testing.T) {
	isolate(t)
	if _, err := NewStatusWriter().Write(Status{PID: 9999}, time.Now()); err != nil {
		t.Fatal(err)
	}

	err := ForceShutdown(4321)
	if err == nil {
		t.Fatal("ForceShutdown() = nil, want a refusal")
	}
	if errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("the platform refusal was reached first, so the mismatch was never checked: %v", err)
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("the refusal does not name the pid the document describes: %v", err)
	}
}

func mustTimeAt(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
