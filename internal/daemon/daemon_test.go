package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	if _, err := os.Stat(mustPath(PIDPath())); err != nil {
		t.Errorf("the pidfile was removed: %v", err)
	}
	if pid, ok, err := ReadPID(); ok || err != nil {
		t.Errorf("ReadPID() = (%d, %v, %v) after shutdown, want nothing to read", pid, ok, err)
	}

	// flock is per-inode: unlinking and recreating lets two daemons each hold
	// "the" lock on a different one.
	if _, err := os.Stat(mustPath(LockPath())); err != nil {
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
	orphan := filepath.Join(dir, orphanTemp(t, StatusFileName, "orphaned"))
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

	body := readFile(t, mustPath(LogPath()))
	if body == "" {
		t.Fatal("the daemon logged nothing at all")
	}
}

// A process that WORKED and then wedged is not the same evidence as one that
// never worked at all, and the recovery budget has to tell them apart. Without
// this, a daemon that runs healthily for a week and then wedges inherits the
// spent budget of whatever restarted it a week ago, and gets no replacement at
// all.
func TestNextRecoveryCountStartsOverAfterAWorkingRun(t *testing.T) {
	if got := NextRecoveryCount(2, true); got != 1 {
		t.Errorf("NextRecoveryCount(2, everPassed) = %d, want a fresh streak of 1", got)
	}
	if got := NextRecoveryCount(2, false); got != 3 {
		t.Errorf("NextRecoveryCount(2, never passed) = %d, want the streak continued", got)
	}
	if got := NextRecoveryCount(0, false); got != 1 {
		t.Errorf("NextRecoveryCount(0, never passed) = %d, want 1", got)
	}
}

// The budget is what stops a machine broken for good from spawning a successor
// every five minutes forever. It is carried in the child's environment, the
// same way CCDAD_DAEMON_CHILD is, because there is nowhere else a fact about
// THIS chain of processes can live -- the store is shared with every other
// daemon that ever ran against it.
func TestRecoveryBudgetShrinksAsItIsSpent(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", maxRecoveries},
		{"0", maxRecoveries},
		{"1", maxRecoveries - 1},
		{"3", 0},
		{"9", 0},
		{"nonsense", maxRecoveries},
		{"-4", maxRecoveries},
	} {
		t.Setenv(RecoveryEnvVar, tc.env)
		if got := recoveryBudget(); got != tc.want {
			t.Errorf("recoveryBudget() with %s=%q = %d, want %d", RecoveryEnvVar, tc.env, got, tc.want)
		}
	}
}

// Run hands the wedge back to its caller rather than replacing the process
// itself, and the ordering is the reason: the successor needs the singleton,
// and Run releases it in a defer. Only the caller runs after that.
func TestRunReportsAWedgedLoopAndNamesTheSuccessorsCount(t *testing.T) {
	isolate(t)
	t.Setenv(RecoveryEnvVar, "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Run(ctx, Options{
		Interval:    time.Millisecond,
		WedgedAfter: 20 * time.Millisecond,
		Tick:        func(context.Context) error { return errors.New("boom") },
	})

	var wedged *WedgedError
	if !errors.As(err, &wedged) {
		t.Fatalf("Run = %v, want a *WedgedError", err)
	}
	if !errors.Is(err, ErrWedged) {
		t.Fatalf("Run = %v, want it to unwrap to ErrWedged", err)
	}
	if wedged.NextRecovery != 2 {
		t.Fatalf("NextRecovery = %d, want the second of three", wedged.NextRecovery)
	}
	// The singleton is what the successor is about to need.
	held, herr := SingletonHeld()
	if herr != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) after Run returned; the successor cannot start", held, herr)
	}
}

// A spent budget must not end the process. No daemon at all is worse than one
// that keeps trying, so the loop runs on and the alarm moves to the document.
func TestRunKeepsRunningWhenTheRecoveryBudgetIsSpent(t *testing.T) {
	isolate(t)
	t.Setenv(RecoveryEnvVar, strconv.Itoa(maxRecoveries))
	stop := runInBackground(t, Options{
		Interval:    time.Millisecond,
		WedgedAfter: 20 * time.Millisecond,
		Tick:        func(context.Context) error { return errors.New("boom") },
	})

	waitForStatus(t, 10*time.Second, func(s Status) bool { return s.TickFailures > 5 })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want a clean stop rather than a wedge it cannot act on", err)
	}
}

// The three facts that were nowhere on disk while a daemon span for three
// hours with every doctor row reading ok.
func TestRunPublishesTickHealth(t *testing.T) {
	isolate(t)
	stop := runInBackground(t, Options{
		Interval: time.Millisecond,
		Tick: func(context.Context) error {
			return errors.New("security find-generic-password: said-nothing (exit 60)")
		},
	})

	s := waitForStatus(t, 10*time.Second, func(s Status) bool { return s.TickFailures > 0 })
	if s.TickFailingSince.IsZero() {
		t.Error("tickFailingSince was never stamped, so nothing can say how old the run is")
	}
	if !strings.Contains(s.LastTickError, "exit 60") {
		t.Errorf("lastTickError = %q, want the tick's error", s.LastTickError)
	}
	stop()

	// And a healthy daemon publishes none of it, so a reader can tell the two
	// apart without knowing which fields a daemon of this vintage writes.
	isolate(t)
	stop2 := runInBackground(t, Options{Interval: time.Millisecond})
	healthy := waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if healthy.TickFailures != 0 || healthy.LastTickError != "" {
		t.Errorf("a healthy daemon published %+v, want the fields absent", healthy)
	}
	stop2()
}

// orderLog records what happened in which order across the daemon's goroutines.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (o *orderLog) add(event string) {
	o.mu.Lock()
	o.events = append(o.events, event)
	o.mu.Unlock()
}

func (o *orderLog) all() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func firstIndex(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

func lastIndex(events []string, want string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i] == want {
			return i
		}
	}
	return -1
}

// fakeProxy stands in for the real listener: this package's job is the ORDER
// the process does things in, not what the proxy does once it is up.
type fakeProxy struct {
	port     int
	fellBack bool
	events   *orderLog
	linger   time.Duration
}

func (p *fakeProxy) Port() int      { return p.port }
func (p *fakeProxy) FellBack() bool { return p.fellBack }

func (p *fakeProxy) Serve(ctx context.Context) error {
	<-ctx.Done()
	// A request still being forwarded when the daemon is asked to stop.
	time.Sleep(p.linger)
	p.events.add("proxy")
	return nil
}

// The launcher reads the port out of status.json and then asks that port for
// health, so a document published before the bind would name a port nothing
// answers.
func TestTheProxyBindsBeforeTheFirstStatusIsPublished(t *testing.T) {
	isolate(t)
	events := &orderLog{}
	stop := runInBackground(t, Options{
		Interval: 5 * time.Millisecond,
		Snapshot: func() Status { events.add("snapshot"); return Status{} },
		StartProxy: func(ctx context.Context) (Proxy, error) {
			events.add("bind")
			return &fakeProxy{port: 24242, fellBack: true, events: events}, nil
		},
	})

	s := waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if s.CodexProxyPort != 24242 {
		t.Errorf("codexProxyPort = %d, want 24242", s.CodexProxyPort)
	}
	if !s.CodexProxyFellBack {
		t.Error("codexProxyFellBack = false, want true — a session started before this daemon is pointed at the old port")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := events.all()
	if len(got) == 0 || got[0] != "bind" {
		t.Fatalf("events = %v, want the bind first", got)
	}
}

func TestAProxyThatCannotBindStopsTheDaemon(t *testing.T) {
	isolate(t)
	refusal := errors.New("the codex proxy cannot bind the configured port 24242: address already in use")
	err := Run(context.Background(), Options{
		Interval:   5 * time.Millisecond,
		StartProxy: func(context.Context) (Proxy, error) { return nil, refusal },
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("Run() = %v, want the bind refusal", err)
	}
	// And it says so where somebody will find it.
	path, perr := LogPath()
	if perr != nil {
		t.Fatal(perr)
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), "not starting") {
		t.Fatalf("the daemon log does not say why it refused:\n%s", data)
	}
	// The singleton must be free again, or nothing can replace this daemon.
	held, herr := SingletonHeld()
	if herr != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) after a refused start", held, herr)
	}
}

// A request the proxy is still forwarding harvests a usage reading when it
// finishes. That reading must not land after the document saying the daemon
// stopped, which is the same rule the engine drain has always had.
func TestTheProxyIsDrainedBeforeTheFinalDocument(t *testing.T) {
	isolate(t)
	events := &orderLog{}
	stop := runInBackground(t, Options{
		Interval: 5 * time.Millisecond,
		Snapshot: func() Status { events.add("snapshot"); return Status{} },
		Drain:    func() { events.add("drain") },
		StartProxy: func(ctx context.Context) (Proxy, error) {
			return &fakeProxy{port: 24242, events: events, linger: 50 * time.Millisecond}, nil
		},
	})
	waitForStatus(t, 10*time.Second, func(s Status) bool { return s.PID != 0 })
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := events.all()
	proxyAt, drainAt := firstIndex(got, "proxy"), firstIndex(got, "drain")
	if proxyAt < 0 || drainAt < 0 {
		t.Fatalf("events = %v, want both a proxy stop and a drain", got)
	}
	if proxyAt > drainAt {
		t.Fatalf("events = %v, want the proxy drained before the engine", got)
	}
	if lastIndex(got, "snapshot") < drainAt {
		t.Fatalf("events = %v, want the final document published after both drains", got)
	}
}

// A wedged loop is the shutdown that nobody asked for: loop.Run gives up on its
// own and returns with the context still live, so the proxy goroutine is still
// parked on <-ctx.Done() when Run reaches the drain that waits for it. The
// explicit stop() before that drain is the only thing that ever tells it.
// Leaving the job to the deferred cancel cannot work, because the deferred one
// does not run until Run returns and Run cannot return until the drain does.
//
// Nothing paired the two cases until this test. The other wedged-loop test
// passes no StartProxy, so proxyDone is nil there and the drain is skipped
// entirely; the three proxy tests all stop through a cancelled parent context,
// which is exactly the case the explicit stop() is NOT for. Deleting the
// stop() line left the whole package green.
//
// The failure is a hang rather than a wrong value, so this is written around a
// deadline: Run is started on a context nobody cancels, and a daemon that
// never returns never runs the defer that releases the singleton -- leaving the
// machine with one hung daemon and a recovery successor that can never claim
// the lock to replace it.
func TestAWedgedLoopStillTellsTheProxyToStop(t *testing.T) {
	isolate(t)
	t.Setenv(RecoveryEnvVar, "1")
	// Nothing cancels this context. Wedging is the only way out.
	ctx, cancel := context.WithCancel(context.Background())
	events := &orderLog{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Interval:    time.Millisecond,
			WedgedAfter: 20 * time.Millisecond,
			Tick:        func(context.Context) error { return errors.New("boom") },
			StartProxy: func(context.Context) (Proxy, error) {
				return &fakeProxy{port: 24242, events: events}, nil
			},
		})
	}()
	// The cleanup matters only when this test FAILS: a Run left hanging still
	// holds the singleton and its temporary home, and every later test in this
	// binary would then fail for a reason that is not its own.
	returned := false
	t.Cleanup(func() {
		cancel()
		if returned {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	select {
	case err := <-done:
		returned = true
		if !errors.Is(err, ErrWedged) {
			t.Fatalf("Run = %v, want it to unwrap to ErrWedged", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned: a wedged loop left the proxy waiting on a context nobody cancelled")
	}
	// And the proxy was told, rather than merely outlived: fakeProxy records
	// itself only after its own context is done. Without this the deadline
	// above would still pass on a Run that skipped the drain altogether.
	if got := events.all(); firstIndex(got, "proxy") < 0 {
		t.Fatalf("events = %v, want the proxy to have seen its context cancelled", got)
	}
	// The singleton is what the recovery successor is about to need.
	held, herr := SingletonHeld()
	if herr != nil || held {
		t.Fatalf("SingletonHeld() = (%v, %v) after Run returned; the successor cannot start", held, herr)
	}
}

func TestTheSweepRunsOnEveryTick(t *testing.T) {
	isolate(t)
	var sweeps atomic.Int64
	stop := runInBackground(t, Options{
		Interval: 5 * time.Millisecond,
		Sweep:    func() { sweeps.Add(1) },
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && sweeps.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sweeps.Load(); got < 2 {
		t.Fatalf("the sweep ran %d times over many ticks, want it on every tick", got)
	}
}
