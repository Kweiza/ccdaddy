package daemon

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Options configures the daemon process.
//
// Tick and Snapshot are injected because the engine they belong to — the poller
// fleet, the scheduler, the switch executor — does not exist yet, and composing
// this process afterwards, alongside all three, is how shutdown correctness gets
// cut. Both have working zero values, so the process is complete and testable
// before any of that lands.
type Options struct {
	// Tick is the body of one iteration. It gets a context that is cancelled on
	// shutdown as a courtesy; the loop waits for it to return regardless.
	//
	// It must never hold a Claude Code lock across a network call. cclock says
	// so in prose for the CLI, and this is where it will actually be violated,
	// because the poller and the switch executor share this process.
	Tick func(context.Context) error
	// Snapshot is the engine state to publish. The process fills in what it
	// owns — pid, startedAt, the stopped flag — and stamps the time.
	Snapshot func() Status
	// Interval is the tick cadence. Zero means §8.4's one second.
	Interval time.Duration
	// Now is the clock. Zero means time.Now.
	Now func() time.Time
}

func (o Options) tick(ctx context.Context) error {
	if o.Tick == nil {
		return nil
	}
	return o.Tick(ctx)
}

func (o Options) snapshot() Status {
	if o.Snapshot == nil {
		return Status{}
	}
	return o.Snapshot()
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Run is the daemon process, from taking the singleton to giving it back.
//
// The order is the design. Startup:
//
//  1. take the singleton — everything after this depends on being the only one;
//  2. open daemon.log and point stderr at it, so a crash from here on leaves a
//     trace instead of vanishing into the null device Spawn handed the child;
//  3. sweep the status temp files a previous daemon's interrupted renames left;
//  4. write the pidfile;
//  5. publish a first status, so a `ccdad status` racing the start sees a daemon
//     rather than nothing.
//
// Shutdown is ONE path, and it is a stop channel rather than an exit. A handler
// that calls os.Exit is not theoretical damage: a tick killed mid-swap abandons
// Claude Code's three lock directories on disk, and cclock's stale windows are
// 60 s, 60 s and 15 s — so Claude Code's own token refresh wedges for up to a
// minute over a Ctrl-C. The loop finishes the tick in flight, the final document
// is published marked stopped, the pidfile is truncated, the log is closed and
// the singleton is released. The lock FILE is never removed: flock is per-inode,
// and delete-and-recreate lets two daemons each hold "the" lock on a different
// one.
func Run(ctx context.Context, o Options) (err error) {
	single, err := AcquireSingleton()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, single.Release()) }()

	log, err := OpenLog()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, log.Close()) }()
	// A platform with no way to redirect descriptor 2 loses crash traces; it
	// does not lose the daemon.
	if cerr := log.CaptureStderr(); cerr != nil {
		log.Printf("stderr stays where it was: %v", cerr)
	}
	log.Printf("ccdad daemon up, pid %d", os.Getpid())

	if serr := SweepStatusTemps(); serr != nil {
		log.Printf("sweeping orphaned status temp files: %v", serr)
	}
	if perr := WritePID(os.Getpid()); perr != nil {
		return perr
	}
	// Truncated on the way out, never removed: an absent pidfile means "no
	// daemon has ever run against this store", which would be a forged fact.
	defer func() { err = errors.Join(err, ClearPID()) }()

	startedAt := o.now()
	writer := NewStatusWriter()
	publish := func(stopped bool) error {
		s := o.snapshot()
		s.PID = os.Getpid()
		s.StartedAt = startedAt
		s.Stopped = stopped
		_, werr := writer.Write(s, o.now())
		return werr
	}
	if perr := publish(false); perr != nil {
		log.Printf("publishing the first status: %v", perr)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	watchSignals(runCtx, stop, log)
	// The same stop, reached the only other way it can be: Windows delivers no
	// signal to a DETACHED_PROCESS child, so §8.4's named event is the
	// mechanism there and this is a no-op everywhere else. Both routes end in
	// the same cancel, so shutdown stays ONE path — which is the property this
	// function is organised around.
	watchShutdownRequest(runCtx, stop, log)

	loop := &Loop{
		Tick: func(c context.Context) error {
			// Publishing rides on the tick rather than on a clock of its own, so
			// what is on disk is always the state after a completed iteration
			// and never a half-applied one.
			return errors.Join(o.tick(c), publish(false))
		},
		Log:      log,
		Interval: o.Interval,
		Now:      o.Now,
	}
	loopErr := loop.Run(runCtx)

	log.Printf("ccdad daemon stopping")
	if perr := publish(true); perr != nil {
		log.Printf("publishing the final status: %v", perr)
	}
	return loopErr
}

// watchSignals turns a signal into a stop, and nothing else.
//
// internal/cli/root.go deliberately does NOT trap SIGINT process-wide, and its
// comment explains why: trapping it removes the default terminating disposition
// for every command, so Ctrl-C became a no-op on `switch` waiting for a
// credential lock. The daemon is the exception, and it is an exception on
// purpose rather than by inheritance — it is the one process here with cleanup
// that MUST run, and the one with no terminal to press Ctrl-C at.
//
// SIGHUP is caught and ignored explicitly. Setsid means a closing shell will not
// send one, but `pkill -HUP` will, and its default disposition is to terminate:
// leaving it unhandled would kill the daemon silently, and there is nothing here
// for a HUP to reload — external config is picked up by the tick loop.
//
// Windows delivers none of this to a DETACHED_PROCESS child; §8.4's named event
// is the mechanism there, and watchShutdownRequest is where it lives.
// signal.Notify is harmless on Windows.
func watchSignals(ctx context.Context, stop func(), log *Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-ch:
				if sig == syscall.SIGHUP {
					log.Printf("%s received and ignored; nothing here reloads on it", sig)
					continue
				}
				log.Printf("%s received, stopping after the tick in flight", sig)
				stop()
				return
			}
		}
	}()
}
