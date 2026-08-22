package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// runInBackground starts Run and returns a stop function that waits for it.
func runInBackground(t *testing.T, o Options) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			cancel()
			<-done
		}
	})
	return func() error {
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after the context was cancelled")
			return nil
		}
	}
}

// waitForStatus polls until a status document satisfies cond.
func waitForStatus(t *testing.T, within time.Duration, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s, ok, err := ReadStatus()
		if err == nil && ok && cond(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no status document matching the condition within %s", within)
	return Status{}
}

func TestRunPublishesWhileItIsUp(t *testing.T) {
	isolate(t)
	stop := runInBackground(t, Options{
		Interval: 5 * time.Millisecond,
		Snapshot: func() Status { return Status{ActiveUUID: "uuid-a"} },
	})

	s := waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if s.PID != os.Getpid() {
		t.Errorf("status pid = %d, want this process (%d)", s.PID, os.Getpid())
	}
	if s.ActiveUUID != "uuid-a" {
		t.Errorf("the snapshot did not reach the document: %+v", s)
	}
	if s.Stopped {
		t.Error("a running daemon published stopped=true")
	}
	if s.StartedAt.IsZero() {
		t.Error("startedAt was never stamped")
	}

	// A probe must see it, and the pidfile must name it.
	held, err := SingletonHeld()
	if err != nil || !held {
		t.Fatalf("SingletonHeld() = (%v, %v) while the daemon is up", held, err)
	}
	pid, ok, err := ReadPID()
	if err != nil || !ok || pid != os.Getpid() {
		t.Errorf("ReadPID() = (%d, %v, %v), want this process", pid, ok, err)
	}

	if err := stop(); err != nil {
		t.Errorf("Run returned %v, want nil on a clean stop", err)
	}
}

// The shutdown sequence, as one path: the final document is marked stopped, the
// pidfile is truncated rather than removed, and the lock file is left exactly
// where it is.
func TestRunShutsDownInOrderAndLeavesTheLockFileAlone(t *testing.T) {
	isolate(t)
	stop := runInBackground(t, Options{Interval: 5 * time.Millisecond})
	waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s, ok, err := ReadStatus()
	if err != nil || !ok {
		t.Fatalf("ReadStatus after shutdown: ok=%v err=%v", ok, err)
	}
	if !s.Stopped {
		t.Error("the final document is not marked stopped; a reader cannot tell this from a crash")
	}

	// Truncated, not removed. An absent pidfile means "no daemon has ever run
	// against this store", which is a different fact and a false one here.
	if _, err := os.Stat(PIDPath()); err != nil {
		t.Errorf("the pidfile was removed: %v", err)
	}
	if pid, ok, err := ReadPID(); ok || err != nil {
		t.Errorf("ReadPID() = (%d, %v, %v) after shutdown, want nothing to read", pid, ok, err)
	}

	// flock is per-inode: unlinking and recreating lets two daemons each hold
	// "the" lock on a different one.
	if _, err := os.Stat(LockPath()); err != nil {
		t.Errorf("the lock file was removed on shutdown: %v", err)
	}
	if held, err := SingletonHeld(); err != nil || held {
		t.Errorf("SingletonHeld() = (%v, %v) after shutdown, want (false, nil)", held, err)
	}
}

func TestRunSweepsOrphanedStatusTemps(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "status.json.tmp-orphaned")
	if err := os.WriteFile(orphan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := runInBackground(t, Options{Interval: 5 * time.Millisecond})
	waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the orphaned temp file survived startup: %v", err)
	}
	stop()
}

func TestRunRunsTheTickBody(t *testing.T) {
	isolate(t)
	ticked := make(chan struct{}, 64)
	stop := runInBackground(t, Options{
		Interval: 5 * time.Millisecond,
		Tick: func(context.Context) error {
			select {
			case ticked <- struct{}{}:
			default:
			}
			return nil
		},
	})
	for range 3 {
		select {
		case <-ticked:
		case <-time.After(10 * time.Second):
			t.Fatal("the tick body never ran")
		}
	}
	stop()
}

// A `ccdad status` racing the daemon's start must see a daemon rather than
// nothing. The interval here is an hour, so the only publication that can
// satisfy this is the one made before the loop ever ticks.
func TestRunPublishesBeforeTheFirstTick(t *testing.T) {
	isolate(t)
	stop := runInBackground(t, Options{
		Interval: time.Hour,
		Snapshot: func() Status { return Status{ActiveUUID: "uuid-a"} },
	})
	s := waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if s.ActiveUUID != "uuid-a" {
		t.Errorf("the first document is not the snapshot: %+v", s)
	}
	stop()
}

// A second daemon must lose, and must lose in a way `daemon status` and `auto`
// can tell from a filesystem that cannot lock.
func TestRunRefusesWhenAnotherProcessHoldsTheSingleton(t *testing.T) {
	store := isolate(t)
	signals := t.TempDir()
	ready := filepath.Join(signals, "ready")
	release := filepath.Join(signals, "release")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(),
		roleEnv+"="+roleHolder,
		readyEnv+"="+ready,
		releaseEnv+"="+release,
		"CCDAD_HOME="+store,
	)
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = holder.Wait()
	})
	waitFor(t, ready, 10*time.Second)

	err := Run(context.Background(), Options{Interval: 5 * time.Millisecond})
	if !errors.Is(err, ErrSingletonHeld) {
		t.Fatalf("Run() = %v, want ErrSingletonHeld", err)
	}
	// It refused before touching anything: a losing daemon that had already
	// written the pidfile would name itself as the daemon in charge.
	if pid, ok, _ := ReadPID(); ok {
		t.Errorf("the losing daemon wrote pid %d into the pidfile", pid)
	}
}

func TestRunReportsAFilesystemThatCannotLock(t *testing.T) {
	isolate(t)
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	defer restore()

	err := Run(context.Background(), Options{Interval: 5 * time.Millisecond})
	if !errors.Is(err, ErrLocksUnsupported) {
		t.Fatalf("Run() = %v, want it to carry ErrLocksUnsupported", err)
	}
	if errors.Is(err, ErrSingletonHeld) {
		t.Error("a broken lock was reported as a lost race; those are different exits")
	}
}

// The daemon writes into daemon.log, and it opens that file ITSELF. Nothing
// hands it a descriptor, because a descriptor opened elsewhere survives the
// rename that rotation performs.
func TestRunWritesItsOwnLog(t *testing.T) {
	isolate(t)
	stop := runInBackground(t, Options{Interval: 5 * time.Millisecond})
	waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	stop()

	body := readFile(t, LogPath())
	if body == "" {
		t.Fatal("the daemon logged nothing at all")
	}
}
