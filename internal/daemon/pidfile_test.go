package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTheDaemonsFilesAllLiveInTheStore(t *testing.T) {
	store := isolate(t)
	for name, got := range map[string]string{
		"ccdad.lock":  mustPath(LockPath()),
		"ccdad.pid":   mustPath(PIDPath()),
		"status.json": mustPath(StatusPath()),
		"daemon.log":  mustPath(LogPath()),
	} {
		if want := filepath.Join(store, name); got != want {
			t.Errorf("%s resolved to %q, want %q", name, got, want)
		}
	}
}

func TestWritePIDAndReadPIDRoundTrip(t *testing.T) {
	isolate(t)
	if err := WritePID(4321); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	pid, ok, err := ReadPID()
	if err != nil || !ok || pid != 4321 {
		t.Fatalf("ReadPID() = (%d, %v, %v), want (4321, true, nil)", pid, ok, err)
	}
}

// The trailing newline is a commit marker, and these are the states it exists
// to tell apart. Everything a half-finished write can leave behind reads as
// "nothing to report"; a body that IS committed and still does not parse is an
// error, because folding corruption into "nothing to report" leaves a
// supervisor unable to tell a damaged store from an idle one — the same hazard
// the singleton's three-outcome contract exists to prevent, one layer down.
func TestReadPIDTellsAnInterruptedWriteFromCorruption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		absent  bool
		wantPID int
		wantOK  bool
		wantErr bool
	}{
		{name: "no file at all", absent: true},
		{name: "zero bytes, which truncation leaves first", body: ""},
		{name: "a prefix with no marker yet", body: "1234"},
		{name: "a shorter prefix, still no marker", body: "12"},
		{name: "committed and whole", body: "1234\n", wantPID: 1234, wantOK: true},
		{name: "committed but not a number", body: "12x4\n", wantErr: true},
		{name: "committed but padded", body: " 1234 \n", wantErr: true},
		{name: "committed as zero", body: "0\n", wantErr: true},
		{name: "committed as negative", body: "-1\n", wantErr: true},
		{name: "two lines", body: "1234\n5678\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if !tc.absent {
				if err := os.WriteFile(mustPath(PIDPath()), []byte(tc.body), 0o600); err != nil {
					t.Fatalf("planting the pidfile: %v", err)
				}
			}
			pid, ok, err := ReadPID()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ReadPID() = (%d, %v, nil) for %q, want an error — a committed body that "+
						"does not parse is corruption, and `ccdad doctor` has to be able to see it", pid, ok, tc.body)
				}
				if ok {
					t.Errorf("ReadPID() reported ok=true alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadPID() = %v for %q, want no error — an unfinished write is expected, not a fault", err, tc.body)
			}
			if ok != tc.wantOK || pid != tc.wantPID {
				t.Errorf("ReadPID() = (%d, %v) for %q, want (%d, %v)", pid, ok, tc.body, tc.wantPID, tc.wantOK)
			}
		})
	}
}

func TestReadPIDReportsAReadFailureRatherThanNothingToRead(t *testing.T) {
	isolate(t)
	// A directory where the pidfile should be: os.ReadFile fails with
	// something that is NOT os.ErrNotExist.
	if err := os.Mkdir(mustPath(PIDPath()), 0o700); err != nil {
		t.Fatalf("planting a directory at the pidfile: %v", err)
	}
	pid, ok, err := ReadPID()
	if err == nil {
		t.Fatalf("ReadPID() = (%d, %v, nil), want an error — an unreadable pidfile is not an absent one", pid, ok)
	}
}

func TestWritePIDRefusesToRecordANonPID(t *testing.T) {
	isolate(t)
	// Kill(0, SIGTERM) signals ccdad's own process group and Kill(-1) signals
	// every process the user may signal, so neither may ever reach the file.
	for _, pid := range []int{0, -1, -1234} {
		if err := WritePID(pid); err == nil {
			t.Errorf("WritePID(%d) was accepted", pid)
		}
	}
	if _, err := os.Stat(mustPath(PIDPath())); !os.IsNotExist(err) {
		t.Errorf("WritePID created the pidfile for a pid it refused")
	}
}

// The write is in place, not a temp file and a rename, and that is load-bearing
// rather than stylistic: a rename is atomic, so there is no torn state for a
// reader to catch, the commit marker becomes dead code, and the next reader of
// pidfile.go deletes it as noise. Both halves stand or fall together, so this
// pins the half that is otherwise invisible.
func TestWritePIDReplacesTheFileInPlace(t *testing.T) {
	isolate(t)
	if err := WritePID(1111); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	first, err := os.Stat(mustPath(PIDPath()))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := WritePID(2222); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	second, err := os.Stat(mustPath(PIDPath()))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(first, second) {
		t.Error("the second write landed on a different file — this is a temp-and-rename, " +
			"which makes the trailing-newline commit marker unreachable and therefore untestable")
	}
	entries, err := os.ReadDir(filepath.Dir(mustPath(PIDPath())))
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}
}

func TestWritePIDLeavesThePidfileReadableOnlyByItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful here")
	}
	isolate(t)
	if err := WritePID(1234); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	info, err := os.Stat(mustPath(PIDPath()))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the pidfile is mode %04o, want 0600 like every other file in the store", perm)
	}
}

// Removing the pidfile would forge the one state the daemon reads as "no daemon
// has ever run against this store". A zero-byte file says "one ran and is not
// running now", which is the truth.
func TestClearPIDEmptiesThePidfileWithoutRemovingIt(t *testing.T) {
	isolate(t)
	if err := WritePID(1234); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	if err := ClearPID(); err != nil {
		t.Fatalf("ClearPID: %v", err)
	}
	info, err := os.Stat(mustPath(PIDPath()))
	if err != nil {
		t.Fatalf("the pidfile is gone after ClearPID, which forges the never-ran state: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("the pidfile is %d bytes after ClearPID, want 0", info.Size())
	}
	pid, ok, err := ReadPID()
	if err != nil || ok {
		t.Errorf("ReadPID() = (%d, %v, %v) after ClearPID, want (0, false, nil)", pid, ok, err)
	}
}

func TestClearPIDOnAStoreThatNeverHadADaemonIsNotAFailure(t *testing.T) {
	isolate(t)
	if err := ClearPID(); err != nil {
		t.Errorf("ClearPID() = %v with no pidfile present, want nil", err)
	}
	// And it must not have created one. An absent pidfile means "no daemon has
	// ever run against this store"; a ClearPID that opened with O_CREATE would
	// forge the opposite, and the whole point of the never-remove rule above
	// is that these two states stay distinct. This is the same class as the
	// singleton's never-create-the-lock-file rule, in the other direction.
	if _, err := os.Stat(mustPath(PIDPath())); !os.IsNotExist(err) {
		t.Errorf("ClearPID created %s on a store no daemon had ever used", mustPath(PIDPath()))
	}
}

// A detached daemon's working directory differs from its parent's by design,
// so a relative store means the daemon and the CLI act on two different files
// while both believe they agree. ccpath now reports an unresolvable home rather
// than degrading to ".ccdad", so what still reaches this guard is a CCDAD_HOME
// that is itself relative — which the test sets explicitly.
func TestTheStoreMustBeAnAbsolutePath(t *testing.T) {
	// A relative store resolves against the working directory, which under
	// `go test` is the package source tree. If any of the three calls below
	// stops refusing, the files it creates must land somewhere disposable
	// rather than in internal/daemon/ — a mutation run proved that is not
	// hypothetical by leaving one there.
	t.Chdir(t.TempDir())
	t.Setenv("CCDAD_HOME", filepath.Join("relative", "store"))
	if err := WritePID(1234); err == nil {
		t.Error("WritePID accepted a relative store")
	}
	if _, err := AcquireSingleton(); err == nil {
		t.Error("AcquireSingleton accepted a relative store")
	}
	if _, err := SingletonHeld(); err == nil {
		t.Error("SingletonHeld accepted a relative store")
	}
}
