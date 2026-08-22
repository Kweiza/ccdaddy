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

// tick delivers one tick and waits for the body to finish. A panicking body
// still reaches its deferred report, so this does not hang on the panic tests.
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

func TestLoopCadenceIsWhatTheSpecSays(t *testing.T) {
	if tickInterval != time.Second {
		t.Errorf("tickInterval = %v, want 1s (§8.4)", tickInterval)
	}
	if rotateCheckInterval != 5*time.Minute {
		t.Errorf("rotateCheckInterval = %v, want 5m — §8.4's 300 ticks at 1 Hz, as wall clock", rotateCheckInterval)
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
