package daemon

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/release"
)

// Options configures the daemon process.
//
// Tick and Snapshot are injected rather than built here, because composing this
// process alongside the engine they belong to — the poller fleet, the
// scheduler, the switch executor — is how shutdown correctness gets cut. Both
// have working zero values, so the process is complete and testable without any
// of them. EngineOptions below is what wires in the real engine.
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
	// Interval is the tick cadence. Zero means the tick loop's one second.
	Interval time.Duration
	// WedgedAfter is how long an unbroken run of failing ticks may last before
	// Run gives up and asks to be replaced. Zero means the tick loop's five
	// minutes; it is settable so a test does not have to spend them.
	WedgedAfter time.Duration
	// Now is the clock. Zero means time.Now.
	Now func() time.Time
	// Attach is handed the daemon's log once it is open, before the first tick.
	// The tick body is injected, so without this it has nowhere to record what
	// it decided — and a daemon that switches accounts silently is one nobody
	// can debug after the fact.
	Attach func(*Logger)
	// Drain is called after the loop has stopped and before the final status is
	// published. The tick body does not wait for the work it dispatches, so
	// this is what stops a poll landing in the cache after the document that
	// said the daemon had stopped.
	Drain func()
}

// EngineOptions is the Options the real daemon runs with: the tick loop's
// body, wired to the engine.
//
// It lives here rather than in the CLI so the process and the thing it runs are
// composed in one place. internal/cli holds the seam that lets a test drive the
// hidden entrypoint without becoming a daemon; what that entrypoint runs is
// this.
func EngineOptions() Options {
	e := engineForDaemon()
	return Options{
		Tick:     e.Tick,
		Snapshot: e.Snapshot,
		Drain:    e.Wait,
		Attach:   func(l *Logger) { e.AttachLog(l.Printf) },
	}
}

// engineForDaemon is NewEngine plus the two things only a daemon process may
// have: the release resolver, and whatever the last daemon left behind about
// the release check.
//
// It is a function of its own so a test can assert on the ENGINE rather than on
// the four funcs EngineOptions wraps it in -- a resolver that was never wired
// is invisible from the other side of that wrapper.
//
// The seeding happens HERE and not in NewEngine. NewEngine is also called from
// the CLI -- internal/cli builds a real engine in its own process for a
// refresh -- so a status read inside the constructor would put filesystem I/O
// on call sites that have nothing to do with a daemon, and would make the "no
// requests from the CLI" promise depend on which methods a refresh happens to
// call. It also has to happen before Run publishes its first document, or that
// publish overwrites the seeded deadline with nothing and a restart loop spends
// one request per restart.
//
// A status file that cannot be read leaves the engine unseeded, which is the
// same state a fresh install is in: one check shortly after start.
func engineForDaemon() *Engine {
	e := NewEngine()
	// The ONE place this is wired. Everywhere else the resolver is nil and the
	// check never runs, which is what keeps `ccdad list --refresh` and every
	// test in this package off the release origin.
	e.LatestRelease = release.NewClient().Latest
	if s, ok, err := ReadStatus(); err == nil && ok {
		e.seedRelease(s, e.now())
	}
	return e
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

func (o Options) attach(l *Logger) {
	if o.Attach != nil {
		o.Attach(l)
	}
}

func (o Options) drain() {
	if o.Drain != nil {
		o.Drain()
	}
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
//  3. take the credential-home claim, which is the OTHER axis: the singleton
//     keeps two daemons out of one store, and this keeps two stores off one
//     Claude Code login. It comes after the log rather than beside the
//     singleton because its refusal is the one a user has to read, and before
//     step 2 there is nowhere to write it;
//  4. sweep the status temp files a previous daemon's interrupted renames left;
//  5. write the pidfile;
//  6. publish a first status, so a `ccdad status` racing the start sees a daemon
//     rather than nothing.
//
// Only ErrClaimed stops the daemon. Every other reason the claim could not be
// taken — a filesystem that cannot lock, a credential home that cannot be
// written — is logged and run through: refusing there would take ccdad away
// from every machine with a network home, a configuration that works today, to
// guard a hazard that needs a second store to exist at all. `ccdad doctor` is
// where the degraded state is named.
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

	claim, claimErr := credhome.Acquire()
	switch {
	case errors.Is(claimErr, credhome.ErrClaimed):
		// The one refusal. Two engines on one credential home do not corrupt
		// anything — cclock serialises the writes — they simply undo each
		// other's switches forever, which is worse than not running.
		log.Printf("not starting: %v", claimErr)
		return claimErr
	case claimErr != nil:
		log.Printf("running WITHOUT the credential-home claim: %v", claimErr)
		log.Printf("if a second ccdad store is driving the same Claude Code login, " +
			"nothing here will notice; 'ccdad doctor' reports this state")
	default:
		defer func() { err = errors.Join(err, claim.Release()) }()
		if claim.OwnerErr != nil {
			// Held, but anonymous. Another store's engine will stand down
			// against it only if it can read this document, so say so.
			log.Printf("holding the credential-home claim, but could not record who holds it: %v", claim.OwnerErr)
		}
	}

	// Before the first tick, so nothing the engine decides is written to a log
	// it does not have yet.
	o.attach(log)

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
	// Resolved once, here, rather than per publish: every other path in this
	// process derives its credential home from the same environment, and a
	// document that re-resolved it each tick would describe a daemon that had
	// changed its mind rather than one whose environment had.
	//
	// An unresolvable home leaves the field empty, which the reader treats as
	// "this daemon did not say" rather than as ~/.claude.
	credentialHome, _ := credhome.Home()
	writer := NewStatusWriter()
	// Declared before publish and assigned after it, because the two point at
	// each other: the loop's body publishes, and what it publishes includes the
	// loop's own health. The nil check is not defensive -- the FIRST publish
	// below happens before the loop exists, and a daemon that has not ticked
	// yet has no health to report.
	var loop *Loop
	publish := func(stopped bool) error {
		s := o.snapshot()
		s.PID = os.Getpid()
		s.CredentialHome = credentialHome
		s.StartedAt = startedAt
		s.Stopped = stopped
		if loop != nil {
			h := loop.Health()
			s.TickFailures, s.TickFailingSince, s.LastTickError = h.Consecutive, h.Since, h.LastError
			s.TickHealthReported = true
		}
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
	// signal to a DETACHED_PROCESS child, so a named shutdown event is the
	// mechanism there and this is a no-op everywhere else. Both routes end in
	// the same cancel, so shutdown stays ONE path — which is the property this
	// function is organised around.
	watchShutdownRequest(runCtx, stop, log)

	loop = &Loop{
		Tick: func(c context.Context) error {
			// Publishing rides on the tick rather than on a clock of its own, so
			// what is on disk is always the state after a completed iteration
			// and never a half-applied one.
			return errors.Join(o.tick(c), publish(false))
		},
		Log:      log,
		Interval: o.Interval,
		Now:      o.Now,
		// Read from the environment HERE rather than inside the loop, so the
		// one process-scoped fact the loop acts on is visible in the process
		// that owns it.
		WedgedAfter:    o.WedgedAfter,
		RecoveryBudget: recoveryBudget(),
	}
	loopErr := loop.Run(runCtx)
	if errors.Is(loopErr, ErrWedged) {
		// EverPassed is read before the shutdown below, because it is the fact
		// that decides whether the successor inherits this chain's spent budget
		// or starts a fresh one -- and it is gone once this process is.
		health := loop.Health()
		loopErr = &WedgedError{Err: loopErr,
			NextRecovery: NextRecoveryCount(recoveriesSoFar(), health.EverPassed || health.Rearmed)}
	}

	log.Printf("ccdad daemon stopping")
	// Before the final document, never after: it is the one that says the
	// daemon has stopped, and a poll still in flight would write into the cache
	// behind it.
	o.drain()
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
// Windows delivers none of this to a DETACHED_PROCESS child; a named shutdown
// event is the mechanism there, and watchShutdownRequest is where it lives.
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
