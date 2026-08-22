package daemon

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

const (
	// tickInterval is §8.4's cadence: once a second.
	tickInterval = time.Second

	// rotateCheckInterval is what §8.4 writes as "every 300 ticks: rotate
	// daemon.log if large", expressed as the wall-clock period it was meant to
	// name. The difference is not pedantry. time.Ticker DROPS ticks rather than
	// queueing them when a body overruns, so 300 ticks is 5 minutes only on a
	// machine where nothing ever runs long — and a laptop that sleeps for eight
	// hours wakes to ONE tick, not 28,800. Every deadline this loop compares is
	// therefore a timestamp, never a counter.
	rotateCheckInterval = 5 * time.Minute

	// maxConsecutivePanics is where "keep going" stops being the right answer. A
	// daemon panicking every tick forever is worse than one that exits: it holds
	// the singleton, so nothing can take over, and it publishes nothing while
	// looking alive to every probe.
	maxConsecutivePanics = 10
)

// Loop is the daemon's 1 Hz tick loop.
//
// The body is injected rather than written in. That is what let this land and be
// tested before the poller fleet, the scheduler and the switch executor existed:
// composed later, under the pressure of getting those three working, shutdown
// correctness is the first thing that gets cut.
//
// Two rules the body inherits and this type cannot enforce:
//
//   - Never hold a Claude Code lock across a network call. cclock says so in
//     prose for the CLI, and the daemon is where it will actually be violated,
//     because the poller and the switch executor share one process.
//   - The body gets the loop's context. It is cancelled on shutdown as a
//     courtesy, not as a kill: the loop always waits for the call to return.
type Loop struct {
	// Interval is the tick cadence. Zero means tickInterval.
	Interval time.Duration
	// Tick is the body. A nil body is a loop that only keeps house.
	Tick func(context.Context) error
	// Log is where a tick error, a panic and a rotation are recorded.
	Log *Logger
	// RotateEvery is the wall-clock cadence of the log rotation check. Zero
	// means rotateCheckInterval.
	RotateEvery time.Duration
	// Now is the clock. Zero means time.Now.
	Now func() time.Time

	// ticks replaces the internal ticker so a test can deliver one tick at a
	// time. Production leaves it nil.
	ticks <-chan time.Time
	// rotate replaces the log rotation check so a test can count them.
	// Production leaves it nil and Log is used.
	rotate func() (bool, error)
	// ready is closed once the loop has read its clock and is waiting for the
	// first tick. Production leaves it nil; a test that arranges the clock needs
	// to know the baseline was taken before it did, or the two race.
	ready chan struct{}

	panics int
}

// Panics is how many ticks have panicked over this loop's life.
func (l *Loop) Panics() int { return l.panics }

func (l *Loop) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return tickInterval
}

func (l *Loop) rotateEvery() time.Duration {
	if l.RotateEvery > 0 {
		return l.RotateEvery
	}
	return rotateCheckInterval
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loop) logf(format string, a ...any) {
	if l.Log != nil {
		l.Log.Printf(format, a...)
	}
}

// Run ticks until the context is cancelled, then returns.
//
// It returns nil for a clean stop, and an error only when it has given up: an
// unbroken run of panicking ticks. It never calls os.Exit and it never releases
// anything it did not take — the singleton, the pidfile and the final status
// document belong to the caller, which is the one place shutdown ordering lives.
func (l *Loop) Run(ctx context.Context) error {
	tickC := l.ticks
	if tickC == nil {
		ticker := time.NewTicker(l.interval())
		defer ticker.Stop()
		tickC = ticker.C
	}

	// Checked before the first tick, not five minutes into it: this daemon
	// inherits whatever its predecessor left in daemon.log, which may already be
	// over the cap.
	l.checkRotation()
	nextRotate := l.now().Add(l.rotateEvery())
	consecutivePanics := 0
	if l.ready != nil {
		close(l.ready)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tickC:
			// Re-checked after the receive, not only in the select. With both
			// cases ready Go chooses at RANDOM, so a cancelled daemon that only
			// looked at the select would start another tick roughly half the
			// time — and a tick started during shutdown is a swap that shutdown
			// is about to be blamed for.
			if ctx.Err() != nil {
				return nil
			}
		}

		err, panicked := l.runTick(ctx)
		switch {
		case panicked:
			l.panics++
			consecutivePanics++
			if consecutivePanics >= maxConsecutivePanics {
				return fmt.Errorf("giving up: %d ticks panicked in a row, most recently %w", consecutivePanics, err)
			}
		case err != nil:
			consecutivePanics = 0
			l.logf("tick failed: %v", err)
		default:
			consecutivePanics = 0
		}

		// Recomputed from the clock just read, never advanced by one period at a
		// time: after a sleep the loop owes one check, not one per period it was
		// asleep for.
		if now := l.now(); !now.Before(nextRotate) {
			l.checkRotation()
			nextRotate = now.Add(l.rotateEvery())
		}
	}
}

// runTick calls the body and turns a panic into an error, reporting which of the
// two happened. The distinction is the whole point: an error is a tick doing its
// job badly, a panic is a bug, and only the second one is grounds for giving up.
func (l *Loop) runTick(ctx context.Context) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			err = fmt.Errorf("the tick panicked: %v", r)
			// The stack goes in the log with it. A daemon's panic is not
			// reproducible on demand, and the line number is the whole value.
			l.logf("%v\n%s", err, debug.Stack())
		}
	}()
	if l.Tick == nil {
		return nil, false
	}
	return l.Tick(ctx), false
}

func (l *Loop) checkRotation() {
	rotate := l.rotate
	if rotate == nil {
		if l.Log == nil {
			return
		}
		rotate = l.Log.RotateIfLarge
	}
	rotated, err := rotate()
	if err != nil {
		l.logf("rotating the daemon log failed: %v", err)
		return
	}
	if rotated {
		l.logf("rotated the daemon log")
	}
}
