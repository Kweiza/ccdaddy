package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the loop's wall clock, under the test's control. Every cadence
// the loop compares is a timestamp comparison, so this is the only way to say
// "eight hours passed between two ticks" without waiting eight hours.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: at("2026-08-22T05:00:00Z")} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness drives a Loop one tick at a time. The tick channel is unbuffered and
// the body reports completion, so a test can say "one tick happened, entirely"
// rather than sleeping and hoping.
type harness struct {
	loop     *Loop
	clock    *fakeClock
	ticks    chan time.Time
	finished chan struct{}
	looped   chan struct{}
	cancel   context.CancelFunc
	done     chan error
	// atStart is how many rotation checks the loop had already made by the time
	// it was waiting for its first tick.
	atStart int

	mu      sync.Mutex
	calls   int
	rotates int
	behave  func(ctx context.Context, call int) error
}

func newHarness(t *testing.T, behave func(ctx context.Context, call int) error) *harness {
	t.Helper()
	isolate(t)
	log := openTestLog(t, 1<<20, 3)
	h := &harness{
		clock:    newClock(),
		ticks:    make(chan time.Time),
		finished: make(chan struct{}, 64),
		looped:   make(chan struct{}, 64),
		done:     make(chan error, 1),
		behave:   behave,
	}
	h.loop = &Loop{
		Tick: func(ctx context.Context) error {
			h.mu.Lock()
			h.calls++
			call := h.calls
			behave := h.behave
			h.mu.Unlock()
			defer func() { h.finished <- struct{}{} }()
			if behave == nil {
				return nil
			}
			return behave(ctx, call)
		},
		Log:         log,
		Now:         h.clock.now,
		RotateEvery: 5 * time.Minute,
		ticks:       h.ticks,
		looped:      h.looped,
		ready:       make(chan struct{}),
		rotate: func() (bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.rotates++
			return false, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.loop.Run(ctx) }()
	select {
	case <-h.loop.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never reached its first tick")
	}
	_, h.atStart = h.counts()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after the context was cancelled")
		}
	})
	return h
}

// tick delivers one tick and waits for the ITERATION to finish, not only the
// body. A panicking body still reaches its deferred report, so this does not
// hang on the panic tests.
//
// The second wait is the one that matters and it was missing. Run does its
// rotation check after runTick returns, so a test that stopped at h.finished
// resumed while the loop was still deciding whether to rotate — and every
// assertion on a rotation count was a race the test usually won. It lost on
// windows-latest.
//
// The give-up test is why h.done is a case here rather than a timeout: on the
// tick that exhausts the panic budget, Run returns from inside the loop body
// and no iteration is ever completed. The error goes back into the channel for
// the test that is about to read it.
func (h *harness) tick(t *testing.T) {
	t.Helper()
	select {
	case h.ticks <- h.clock.now():
	case err := <-h.done:
		h.done <- err
		t.Fatalf("the loop had already returned (%v) when a tick was delivered", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never accepted a tick")
	}
	select {
	case <-h.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick body never finished")
	}
	select {
	case <-h.looped:
	case err := <-h.done:
		h.done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never came back round to wait for its next tick")
	}
}

func (h *harness) counts() (calls, rotates int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.rotates
}

// rotatesSinceStart discounts the check the loop makes before its first tick.
func (h *harness) rotatesSinceStart() int {
	_, rotates := h.counts()
	return rotates - h.atStart
}

func (h *harness) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
		return nil
	}
}

func TestLoopRunsTheTickBody(t *testing.T) {
	h := newHarness(t, nil)
	for range 3 {
		h.tick(t)
	}
	if calls, _ := h.counts(); calls != 3 {
		t.Errorf("the body ran %d times, want 3", calls)
	}
	if err := h.stop(t); err != nil {
		t.Errorf("Run returned %v, want nil on a clean stop", err)
	}
}

// Shutdown is one path and it never interrupts work in progress. A tick killed
// mid-swap abandons Claude Code's three lock directories on disk, and cclock's
// stale windows are 60 s / 60 s / 15 s — so Claude Code's own token refresh
// wedges for up to a minute over a Ctrl-C.
func TestLoopFinishesTheInFlightTickBeforeReturning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var finished bool
	var mu sync.Mutex

	h := newHarness(t, func(ctx context.Context, call int) error {
		close(started)
		<-release
		mu.Lock()
		finished = true
		mu.Unlock()
		return nil
	})

	// Deliver a tick and wait until the body has actually BEGUN. Waiting only
	// for the channel send would prove the loop received the tick, not that it
	// entered the body — and the loop deliberately abandons a tick it received
	// but had not started when the stop arrived.
	select {
	case h.ticks <- h.clock.now():
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never accepted a tick")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick body never started")
	}
	h.cancel()

	// The loop must still be inside the body: nothing has released it.
	select {
	case err := <-h.done:
		t.Fatalf("Run returned (%v) while a tick was still running", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-h.done:
		h.done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
	// The body reports completion on the harness channel too; drain it so the
	// cleanup is not left holding an unread send.
	select {
	case <-h.finished:
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if !finished {
		t.Fatal("the in-flight tick was abandoned")
	}
}

// time.Ticker DROPS ticks rather than queueing them when a body overruns, so a
// counter never measures wall-clock time. Five ticks four minutes apart is
// twenty minutes and two rotation checks; "every 300 ticks" would be none.
func TestRotationChecksFollowTheWallClockNotATickCount(t *testing.T) {
	h := newHarness(t, nil)
	for range 5 {
		h.clock.advance(4 * time.Minute)
		h.tick(t)
	}
	calls, _ := h.counts()
	if calls != 5 {
		t.Fatalf("the body ran %d times, want 5", calls)
	}
	if got := h.rotatesSinceStart(); got != 2 {
		t.Errorf("rotation was checked %d times over 20 minutes at a 5-minute cadence, want 2", got)
	}

}

// A laptop that sleeps eight hours wakes to ONE tick, not 28,800. Every deadline
// the loop compares has to be recomputed from the clock it just read, not
// advanced by one period at a time.
func TestASleepJumpProducesOneRotationCheckNotThousands(t *testing.T) {
	h := newHarness(t, nil)
	h.clock.advance(8 * time.Hour)
	h.tick(t)

	if got := h.rotatesSinceStart(); got != 1 {
		t.Errorf("rotation was checked %d times after an eight-hour jump, want exactly 1", got)
	}
	// And the next deadline is five minutes from the wake-up, not from before
	// the sleep — otherwise every subsequent tick checks again.
	h.clock.advance(time.Minute)
	h.tick(t)
	if got := h.rotatesSinceStart(); got != 1 {
		t.Errorf("rotation was checked %d times, want 1: the deadline was not recomputed from the wake-up", got)
	}
}

// A daemon inherits whatever its predecessor left in daemon.log, so waiting
// five minutes to notice an oversized one is five minutes of appending to it.
func TestRotationIsCheckedBeforeTheFirstTick(t *testing.T) {
	h := newHarness(t, nil)
	if h.atStart != 1 {
		t.Errorf("the loop made %d rotation checks before its first tick, want 1", h.atStart)
	}
}

// With a tick pending and the context cancelled, Go's select chooses at RANDOM
// between the two ready cases. That makes a single trial a coin flip — a first
// version of this test passed against the mutation that deletes the guard — so
// it is run twenty times and the total has to be zero. An unguarded loop starts
// a tick in about half of them.
func TestLoopStartsNoTickAfterCancellation(t *testing.T) {
	isolate(t)
	log := openTestLog(t, 1<<20, 3)

	const trials = 20
	var calls int
	var mu sync.Mutex
	for range trials {
		ticks := make(chan time.Time, 4)
		for range 4 {
			ticks <- at("2026-08-22T05:00:00Z")
		}
		l := &Loop{
			Tick: func(context.Context) error {
				mu.Lock()
				calls++
				mu.Unlock()
				return nil
			},
			Log:   log,
			Now:   newClock().now,
			ticks: ticks,
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan error, 1)
		go func() { done <- l.Run(ctx) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run never returned")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("over %d trials the loop started %d ticks after its context was already cancelled, want 0", trials, calls)
	}
}

func TestLoopKeepsGoingAfterATickPanics(t *testing.T) {
	h := newHarness(t, func(_ context.Context, call int) error {
		if call == 1 {
			panic("the poller fleet fell over")
		}
		return nil
	})
	h.tick(t)
	h.tick(t)

	if calls, _ := h.counts(); calls != 2 {
		t.Errorf("the body ran %d times, want 2: a panic must not stop the loop", calls)
	}
	if got := h.loop.Panics(); got != 1 {
		t.Errorf("Panics() = %d, want 1: a panic that is not counted is a panic nobody will ever see", got)
	}
	if body := readFile(t, mustPath(LogPath())); !strings.Contains(body, "the poller fleet fell over") {
		t.Errorf("the panic is not in the log:\n%s", body)
	}
}

// A daemon panicking every tick forever is worse than one that exits: it holds
// the singleton, so nothing else can take over, and it publishes nothing.
func TestLoopGivesUpAfterAnUnbrokenRunOfPanics(t *testing.T) {
	h := newHarness(t, func(_ context.Context, _ int) error {
		panic("every tick")
	})
	for range maxConsecutivePanics {
		h.tick(t)
	}
	select {
	case err := <-h.done:
		h.done <- err
		if err == nil {
			t.Fatal("Run returned nil after an unbroken run of panics")
		}
		if !strings.Contains(err.Error(), "panic") {
			t.Errorf("err = %v, want it to name the panics", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop is still running after an unbroken run of panics")
	}
}

func TestASuccessfulTickBreaksThePanicRun(t *testing.T) {
	h := newHarness(t, func(_ context.Context, call int) error {
		if call%2 == 1 {
			panic("every other tick")
		}
		return nil
	})
	for range maxConsecutivePanics * 2 {
		h.tick(t)
	}
	select {
	case err := <-h.done:
		h.done <- err
		t.Fatalf("the loop gave up (%v) although the panics were never consecutive", err)
	default:
	}
	if got := h.loop.Panics(); got != maxConsecutivePanics {
		t.Errorf("Panics() = %d, want %d", got, maxConsecutivePanics)
	}
}

// A tick that returns an error is a tick doing its job badly, not a bug, so it
// breaks a run of panics exactly as a clean tick does.
func TestAnErrorAlsoBreaksThePanicRun(t *testing.T) {
	h := newHarness(t, func(_ context.Context, call int) error {
		if call%2 == 1 {
			panic("every other tick")
		}
		return errors.New("and an error in between")
	})
	for range maxConsecutivePanics * 2 {
		h.tick(t)
	}
	select {
	case err := <-h.done:
		h.done <- err
		t.Fatalf("the loop gave up (%v) although the panics were never consecutive", err)
	default:
	}
}

func TestLoopLogsATickErrorAndCarriesOn(t *testing.T) {
	h := newHarness(t, func(_ context.Context, call int) error {
		if call == 1 {
			return errors.New("the usage endpoint refused this credential")
		}
		return nil
	})
	h.tick(t)
	h.tick(t)

	if calls, _ := h.counts(); calls != 2 {
		t.Errorf("the body ran %d times, want 2", calls)
	}
	if body := readFile(t, mustPath(LogPath())); !strings.Contains(body, "refused this credential") {
		t.Errorf("the tick error is not in the log:\n%s", body)
	}
	if got := h.loop.Panics(); got != 0 {
		t.Errorf("Panics() = %d; an error is not a panic", got)
	}
}

// The two cadences are pinned against literals, so shrinking either constant
// cannot shrink the assertion that it is what it is.
func TestLoopCadenceIsOneSecondAndRotationCheckIsFiveMinutes(t *testing.T) {
	if tickInterval != time.Second {
		t.Errorf("tickInterval = %v, want 1s", tickInterval)
	}
	if rotateCheckInterval != 5*time.Minute {
		t.Errorf("rotateCheckInterval = %v, want 5m — 300 ticks at the 1 s cadence, as wall clock", rotateCheckInterval)
	}
}

// With no ticks injected the loop builds its own ticker, and a cancelled
// context has to win over waiting a whole second for one.
func TestLoopWithARealTickerStopsPromptly(t *testing.T) {
	isolate(t)
	log := openTestLog(t, 1<<20, 3)
	l := &Loop{Tick: func(context.Context) error { return nil }, Log: log}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly on cancellation")
	}
}

// A tick that fails the same way every second must not write a line every
// second. The daemon that made this rule logged 11,300 identical copies of
// `tick failed: security find-generic-password: empty` over three hours and
// 900 KB, and the repetition carried no information the first line did not:
// nobody reads the 400th, and rotation throws away the context around it.
func TestARunOfIdenticalTickFailuresIsLoggedOnce(t *testing.T) {
	boom := errors.New("security find-generic-password: said-nothing (exit 60)")
	h := newHarness(t, func(context.Context, int) error { return boom })
	h.loop.RepeatEvery = 5 * time.Minute

	for i := 0; i < 300; i++ {
		h.tick(t)
	}

	body := readFile(t, mustPath(LogPath()))
	if got := strings.Count(body, "tick failed"); got != 1 {
		t.Fatalf("logged %d \"tick failed\" lines for one unbroken run, want 1", got)
	}
}

// Once, though, is not enough either: a failure that is still going an hour
// later has to say so, or the log's silence reads as recovery.
func TestAStillFailingTickIsLoggedAgainOnItsOwnCadence(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })
	h.loop.RepeatEvery = 5 * time.Minute

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)

	body := readFile(t, mustPath(LogPath()))
	if !strings.Contains(body, "still failing") {
		t.Fatalf("log = %q, want a line saying the run is still going", body)
	}
	if !strings.Contains(body, "2 ticks") {
		t.Fatalf("log = %q, want the repeat line to count the ticks", body)
	}
}

// A DIFFERENT error is not a repeat. Folding it into the run would hide the
// one event most worth seeing: the failure changing shape.
func TestAChangedTickErrorIsLoggedStraightAway(t *testing.T) {
	var which error = errors.New("first")
	h := newHarness(t, func(context.Context, int) error { return which })

	h.tick(t)
	which = errors.New("second")
	h.tick(t)

	body := readFile(t, mustPath(LogPath()))
	if !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Fatalf("log = %q, want both errors named", body)
	}
}

// Recovery is an EVENT and it gets a line. A log that only ever records
// failures cannot answer "when did it come back", which is the question asked
// of it after every incident.
func TestRecoveryIsLoggedWithWhatItCost(t *testing.T) {
	fail := true
	h := newHarness(t, func(context.Context, int) error {
		if fail {
			return errors.New("boom")
		}
		return nil
	})

	h.tick(t)
	h.tick(t)
	fail = false
	h.tick(t)

	body := readFile(t, mustPath(LogPath()))
	if !strings.Contains(body, "recovered after 2 failed ticks") {
		t.Fatalf("log = %q, want the recovery line naming what it cost", body)
	}
	if h.loop.Health().Consecutive != 0 {
		t.Fatalf("Consecutive = %d after a passing tick, want 0", h.loop.Health().Consecutive)
	}
}

// The wedge rule, and the reason the whole mechanism exists: retrying IN
// PROCESS recovered nothing across 11,300 attempts, and a fresh process was
// healthy on its first tick. So a run of failures long enough to be structural
// ends the loop, and the caller replaces the process.
//
// It is a DURATION and not a count, for the reason rotateCheckInterval is:
// time.Ticker drops ticks rather than queueing them, so a laptop that slept
// through the window would otherwise need 300 wakeful seconds to notice.
func TestAnUnbrokenRunOfFailuresWedgesTheLoop(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })
	h.loop.WedgedAfter = 5 * time.Minute
	h.loop.RecoveryBudget = 1

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)

	err := h.stop(t)
	if !errors.Is(err, ErrWedged) {
		t.Fatalf("Run = %v, want ErrWedged", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run = %v, want the last error named in it", err)
	}
}

// A budget of zero is a daemon that has already replaced itself as often as it
// is allowed to. It must NOT keep exiting: a machine whose keychain is broken
// for good would spawn a successor every five minutes forever, and the state
// that leaves — no daemon at all, or a replacement storm — is worse than a
// daemon that keeps trying and says so. doctor is what shouts.
func TestAWedgedLoopWithNoBudgetKeepsTicking(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })
	h.loop.WedgedAfter = 5 * time.Minute
	h.loop.RecoveryBudget = 0

	h.tick(t)
	h.clock.advance(5 * time.Minute)
	h.tick(t)
	h.tick(t)

	if got := h.loop.Health().Consecutive; got != 3 {
		t.Fatalf("Consecutive = %d, want the loop still ticking after the wedge point", got)
	}
	if err := h.stop(t); err != nil {
		t.Fatalf("Run = %v, want a clean stop when there is no budget to give up on", err)
	}
}

// Health is what the status document publishes and doctor reads. The streak,
// when it started, and the error -- the three facts that were nowhere on disk
// while the daemon span for three hours with every doctor row reading ok.
func TestHealthCarriesTheStreakAndItsCause(t *testing.T) {
	h := newHarness(t, func(context.Context, int) error { return errors.New("boom") })

	if got := h.loop.Health(); got.Consecutive != 0 || got.EverPassed {
		t.Fatalf("Health before any tick = %+v, want the zero state", got)
	}
	started := h.clock.now()
	h.tick(t)
	h.clock.advance(time.Minute)
	h.tick(t)

	got := h.loop.Health()
	if got.Consecutive != 2 {
		t.Fatalf("Consecutive = %d, want 2", got.Consecutive)
	}
	if !got.Since.Equal(started) {
		t.Fatalf("Since = %v, want the FIRST failure's time %v", got.Since, started)
	}
	if got.LastError != "boom" {
		t.Fatalf("LastError = %q, want the tick's error", got.LastError)
	}
	if got.EverPassed {
		t.Fatal("EverPassed = true, but no tick has ever passed")
	}
}
