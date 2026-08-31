package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

const (
	// tickInterval is the tick loop's cadence: once a second.
	tickInterval = time.Second

	// recoveryRearmAfter is how long a wedge may last before a loop with no
	// replacement budget left is allowed one more attempt.
	//
	// The cap exists because a machine can be in a state a fresh process does
	// not fix. What it got wrong is that the state can END: the fault that
	// exhausted a chain on 2026-08-31 was a locked login keychain, and a user
	// logging back into the GUI session clears it. With no re-arm the daemon
	// that watched that happen kept failing for the rest of the night, because
	// the decision had been taken an hour before the machine changed.
	//
	// An hour, so the pathological case is one spawn per hour rather than one
	// per five minutes, and every attempt is a log line.
	recoveryRearmAfter = time.Hour

	// rotateCheckInterval is how often the tick loop checks daemon.log for
	// rotation. At a 1 s tick, 5 minutes is 300 ticks; this is deliberately the
	// wall-clock form and not the count. The difference is not pedantry.
	// time.Ticker DROPS ticks rather than queueing them when a body overruns, so
	// 300 ticks is 5 minutes only on a machine where nothing ever runs long —
	// and a laptop that sleeps for eight hours wakes to ONE tick, not 28,800.
	// Every deadline this loop compares is therefore a timestamp, never a
	// counter.
	rotateCheckInterval = 5 * time.Minute

	// maxConsecutivePanics is where "keep going" stops being the right answer. A
	// daemon panicking every tick forever is worse than one that exits: it holds
	// the singleton, so nothing can take over, and it publishes nothing while
	// looking alive to every probe.
	maxConsecutivePanics = 10

	// wedgedAfter is the same judgement for a tick that ERRORS rather than
	// panicking, and it is a duration rather than a count for the reason
	// rotateCheckInterval is: a laptop that slept through the window would
	// otherwise owe 300 wakeful seconds before anyone noticed.
	//
	// Five minutes is chosen from what the failure it exists for looked like.
	// A daemon whose keychain read had started failing did so on its FIRST tick
	// and on all 11,300 after it, so any threshold at all would have caught it;
	// what the number has to avoid is catching an ordinary outage. Nothing in
	// the tick body retries for anything like five minutes -- the poll timeout
	// and every `security` spawn are seconds -- so an unbroken run this long is
	// structural rather than transient.
	wedgedAfter = 5 * time.Minute

	// tickErrorRepeatEvery is how often an unchanged tick error is written
	// again. The first line of a run is always logged; this is what keeps the
	// 400th from being.
	tickErrorRepeatEvery = 5 * time.Minute
)

// ErrWedged is Run reporting that the tick body has failed without interruption
// for long enough to be structural, and that the caller should replace the
// process rather than wait for it to come round.
//
// It is a REPLACEMENT and not a shutdown, and the evidence says which. Retrying
// in process recovered nothing across 11,300 consecutive failures; a fresh
// process was healthy on its first tick, spawning the same `security` with the
// same argv from the same user. Nothing found a difference between the two
// beyond the process itself -- environment, working directory, session and
// keychain state were all measured equal -- so the loop does not claim to know
// what a new process fixes, only that one does.
var ErrWedged = errors.New("the tick body has been failing without interruption")

// TickHealth is what the loop knows about the tick body's recent history. It is
// published in the status document and read back by doctor, because none of it
// was anywhere on disk while a daemon span for three hours with every doctor
// row reading ok.
type TickHealth struct {
	// Consecutive is the length of the current run of failing ticks, and 0 the
	// moment one passes.
	Consecutive int
	// Since is when the current run began -- the FIRST failure's time, not the
	// most recent, because the age of the run is the fact worth reporting.
	Since time.Time
	// LastError is the most recent tick error, rendered.
	LastError string
	// EverPassed is whether any tick in THIS process has ever succeeded. It is
	// what decides whether a replacement inherits the recovery budget or starts
	// a fresh one: a process that worked for a day and then wedged is not the
	// same evidence as one that never worked at all.
	EverPassed bool
	// Rearmed is whether this run gave up on a wedge with its replacement
	// budget already spent, because the wedge had outlasted recoveryRearmAfter.
	// It makes the successor a FRESH chain for the same reason EverPassed does:
	// the evidence the cap was counting is stale.
	Rearmed bool
}

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
	// WedgedAfter is how long an unbroken run of failing ticks may last before
	// Run gives up on it. Zero means wedgedAfter.
	WedgedAfter time.Duration
	// RepeatEvery is how often an unchanged tick error is logged again. Zero
	// means tickErrorRepeatEvery.
	RepeatEvery time.Duration
	// RecoveryBudget is how many more times the PROCESS may replace itself, and
	// a loop with none must not give up: a machine broken for good would
	// otherwise spawn a successor every five minutes forever. Zero -- the zero
	// value, so nothing that builds a Loop without asking for this behaviour
	// gets it -- means the loop keeps ticking through a wedge and lets the
	// published health be what raises the alarm.
	RecoveryBudget int

	// RecoveryRearmAfter overrides recoveryRearmAfter. Zero means the constant.
	RecoveryRearmAfter time.Duration

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
	// looped receives once at the END of every completed iteration, with the
	// loop about to wait for its next tick. Production leaves it nil.
	//
	// The tick body finishing is NOT that moment, and the difference is the
	// whole reason this exists: the rotation check below runs AFTER runTick
	// returns, so a test that read rotation counts as soon as its body returned
	// was reading them in a race with this loop. It lost often enough on
	// windows-latest to fail TestASleepJumpProducesOneRotationCheckNotThousands
	// with "rotation was checked 0 times", on a test with a fake clock and no
	// timing in it at all.
	looped chan<- struct{}

	panics int

	// saidInherited latches noteInheritedFault for one run of failures, so the
	// decision is stated once rather than at 1 Hz.
	saidInherited bool
	health        TickHealth
	// lastLoggedAt is when the current run of failures was last written to the
	// log, which is not when it last failed.
	lastLoggedAt time.Time
}

// Panics is how many ticks have panicked over this loop's life.
func (l *Loop) Panics() int { return l.panics }

// Health is the tick body's recent history, for the status document.
//
// It is read from the loop's own goroutine by the tick body's publisher, one
// tick behind: publishing happens INSIDE the tick, so the document written this
// tick carries the streak as of the previous one. That is a property worth
// naming rather than fixing -- fixing it means publishing after the body, which
// is the ordering that lets a half-applied iteration reach disk.
func (l *Loop) Health() TickHealth { return l.health }

// noteInheritedFault says once, per run of failures, that the loop is wedged on
// something replacing the process cannot fix. It is latched on the run rather
// than repeated, because noteTickFailure is already saying the error itself on
// its own cadence and this line is about the DECISION, which does not change.
func (l *Loop) noteInheritedFault(now time.Time, err error) {
	if l.saidInherited {
		return
	}
	l.saidInherited = true
	l.logf("not replacing this daemon: it has been failing for %s and the cause is one a fresh "+
		"process on this machine would hit identically (%v). macOS scopes that refusal to the audit "+
		"session, which a child inherits, so a replacement would fail on its own first tick. Restart "+
		"ccdad from a shell that can already read the keychain",
		now.Sub(l.health.Since).Round(time.Second), err)
}

// survivesRestart is this file's window onto the classification, and it is a
// var for the reason doctor's keychainProbe is one: the only error that answers
// true is built by a `security` spawn this suite cannot make happen.
var survivesRestart = cclink.SurvivesRestart

// rearmAfter is how long this loop stays given up before it may try once more.
func (l *Loop) rearmAfter() time.Duration {
	if l.RecoveryRearmAfter > 0 {
		return l.RecoveryRearmAfter
	}
	return recoveryRearmAfter
}

func (l *Loop) wedgedAfter() time.Duration {
	if l.WedgedAfter > 0 {
		return l.WedgedAfter
	}
	return wedgedAfter
}

func (l *Loop) repeatEvery() time.Duration {
	if l.RepeatEvery > 0 {
		return l.RepeatEvery
	}
	return tickErrorRepeatEvery
}

// noteTickFailure folds one failing tick into the current run and decides
// whether it is worth a line.
//
// Three things get logged and nothing else does: the first failure of a run,
// a failure whose error has CHANGED -- the event most worth seeing, and the one
// a naive "log every N" would bury -- and the run still going one repeat window
// later.
func (l *Loop) noteTickFailure(now time.Time, err error) {
	text := err.Error()
	first := l.health.Consecutive == 0
	changed := !first && text != l.health.LastError

	if first {
		l.health.Since = now
	}
	l.health.Consecutive++
	l.health.LastError = text

	switch {
	case first, changed:
		l.logf("tick failed: %v", err)
	case now.Sub(l.lastLoggedAt) >= l.repeatEvery():
		l.logf("tick still failing: %d ticks over %s, most recently: %v",
			l.health.Consecutive, now.Sub(l.health.Since).Round(time.Second), err)
	default:
		return
	}
	l.lastLoggedAt = now
}

// noteTickSuccess ends a run, and says what it cost. A log that records only
// failures cannot answer "when did it come back", which is the question asked
// of it after every incident.
func (l *Loop) noteTickSuccess(now time.Time) {
	if l.health.Consecutive > 0 {
		l.logf("tick recovered after %d failed ticks over %s",
			l.health.Consecutive, now.Sub(l.health.Since).Round(time.Second))
	}
	l.health = TickHealth{EverPassed: true}
	l.lastLoggedAt = time.Time{}
	// The decision is about a RUN of failures, so a tick that passes ends the
	// run and the next one may be reported afresh.
	l.saidInherited = false
}

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
			now := l.now()
			l.noteTickFailure(now, err)
			// A fault a FRESH PROCESS would hit identically is not worth a
			// replacement, and spending three on one is how a chain arrives
			// with nothing left for a wedge a restart could have cleared. See
			// cclink.SurvivesRestart for the scoping rule that makes the
			// successor's failure certain rather than likely.
			if now.Sub(l.health.Since) >= l.wedgedAfter() && survivesRestart(err) {
				l.noteInheritedFault(now, err)
				break
			}
			// The budget is checked BEFORE the window, not after, so a loop
			// that may not give up never even measures one -- it has nothing
			// to do with the answer, and a wedge it cannot act on is not an
			// event.
			//
			// A spent budget is not permanent any more. A wedge that outlasts
			// rearmAfter earns one more attempt, because the machine the cap
			// gave up on may have changed since -- see recoveryRearmAfter.
			if (l.RecoveryBudget > 0 || now.Sub(l.health.Since) >= l.rearmAfter()) &&
				now.Sub(l.health.Since) >= l.wedgedAfter() {
				l.health.Rearmed = l.RecoveryBudget <= 0
				return fmt.Errorf("%w for %s (%d ticks), most recently: %w",
					ErrWedged, now.Sub(l.health.Since).Round(time.Second), l.health.Consecutive, err)
			}
		default:
			consecutivePanics = 0
			l.noteTickSuccess(l.now())
		}

		// Recomputed from the clock just read, never advanced by one period at a
		// time: after a sleep the loop owes one check, not one per period it was
		// asleep for.
		if now := l.now(); !now.Before(nextRotate) {
			l.checkRotation()
			nextRotate = now.Add(l.rotateEvery())
		}

		if l.looped != nil {
			l.looped <- struct{}{}
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
