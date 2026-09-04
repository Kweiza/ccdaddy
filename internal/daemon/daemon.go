package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/release"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
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
	// StartProxy binds the Codex proxy's listener. It runs BEFORE the first
	// status publish, so the port that document carries is a port something is
	// actually listening on -- a launcher proves the LISTENER by reading the
	// port out of the document and asking it for health, because holding the
	// singleton is not evidence that a proxy is up.
	//
	// An error stops the daemon. That is the whole point of the seam: a bind
	// failure on a port the user configured must not be papered over, because
	// serving on a different one leaves every codex session pointed at a port
	// nothing answers, and codex's symptom for that is an endless reconnect.
	//
	// Nil means this daemon runs no proxy, which is what every test in this
	// package that is not about the proxy gets.
	StartProxy func(ctx context.Context) (Proxy, error)
	// Sweep is housekeeping the tick body has no reason to know about, run once
	// per tick before it. Today that is removing the launch records of codex
	// processes that are gone; whatever it does, it must be cheap and it must
	// not fail, because nothing here reads a result from it.
	Sweep func()
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
		Tick:       e.Tick,
		Snapshot:   e.Snapshot,
		Drain:      e.Wait,
		Attach:     func(l *Logger) { e.AttachLog(l.Printf) },
		StartProxy: e.startCodexProxy,
		Sweep:      e.reapCodexLaunches,
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
	// check never runs, which is what keeps `ccdad status --refresh` and every
	// test in this package off the release origin.
	e.LatestRelease = release.NewClient().Latest
	// The Codex lane's seams, wired here for the same reason and a sharper one:
	// only the daemon may refresh a Codex grant, because the endpoint kills a
	// refresh token that is used twice. A CLI process that acquired a refresher
	// would be a second spender of the same grant.
	wireCodex(e)
	if s, ok, err := ReadStatus(); err == nil && ok {
		e.seedRelease(s, e.now())
	}
	return e
}

// wireCodex gives the engine the four Codex seams. It is a function of its own
// so a test can assert on the ENGINE rather than on EngineOptions' wrappers.
func wireCodex(e *Engine) {
	e.CodexBook = &codexproxy.LimitBook{}
	e.CodexRefresher = codexauth.NewRefresher(codexauth.RefresherConfig{
		Log: func(format string, a ...any) { e.logf(format, a...) },
	})
	e.CodexAccessToken, e.CodexFetchUsage = CodexReadSeams(nil)
}

// CodexReadSeams is the READ half of the Codex seams: hand out the stored
// access token, and spend it on the usage endpoint.
//
// It is EXPORTED and the refresher is not, and that split is the whole of how
// "only the daemon refreshes a Codex token" is kept. `ccdad status --refresh`
// builds an Engine of its own and needs exactly these two; a function that also
// handed back a refresher would make that process a second spender of a
// single-use grant, and the endpoint kills a refresh token that is used twice.
//
// client may be nil, which builds one bounded by codexUsageTimeout.
func CodexReadSeams(client *http.Client) (
	func(ctx context.Context, uuid string) (string, string, error),
	func(ctx context.Context, accessToken, accountID string) (*usage.Snapshot, codexusage.Identity, error)) {

	if client == nil {
		client = &http.Client{Timeout: codexUsageTimeout}
	}
	token := func(_ context.Context, uuid string) (string, string, error) {
		var access, accountID string
		err := store.WithStore(func(s *store.Store) error {
			creds, cerr := s.Credentials(uuid)
			if cerr != nil {
				return cerr
			}
			c, ok, perr := codexauth.FromBlob(creds)
			if perr != nil {
				return perr
			}
			if !ok {
				return fmt.Errorf("%s holds no codex credential", uuid)
			}
			access, accountID = c.AccessToken, c.AccountID
			return nil
		})
		return access, accountID, err
	}
	fetch := func(ctx context.Context, accessToken, accountID string) (*usage.Snapshot, codexusage.Identity, error) {
		return codexusage.Fetch(ctx, client, accessToken, accountID, buildinfo.Version)
	}
	return token, fetch
}

// codexUsageTimeout bounds one call to the Codex usage endpoint. It is the
// poll's own bound as well, through Engine.pollTimeout; this one stops a
// connection that never answers from holding the goroutine past it.
const codexUsageTimeout = 30 * time.Second

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

func (o Options) sweep() {
	if o.Sweep != nil {
		o.Sweep()
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
//  6. bind the Codex proxy, so the port the first document carries is one
//     something is listening on;
//  7. publish a first status, so a `ccdad status` racing the start sees a daemon
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
// minute over a Ctrl-C. The loop finishes the tick in flight, the proxy drains
// its in-flight turns, the engine drains its polls, the final document
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
	// The store's version rule is decided ONCE, here, and never inside a tick.
	// A document this build cannot read -- a version-2 header over rows with no
	// provider, which is what a ccdad that predates Codex support leaves behind
	// -- is a fact about the machine, and re-deciding it on a cadence would
	// turn one refusal into a log line per tick. Nothing is created: a store
	// that is not there is not a store with a bad version.
	//
	// Ahead of the "up" line below on purpose: a start this build refuses logs
	// "not starting" and stops there, rather than logging "up" for a daemon
	// that unwinds on the very next statement.
	root, rerr := ccpath.StoreHome()
	if rerr != nil {
		return rerr
	}
	if verr := store.CheckVersionAt(root); errors.Is(verr, store.ErrProviderMissing) {
		log.Printf("not starting: %v", verr)
		return verr
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

	// The run context is created HERE rather than after the first publish,
	// because the proxy has to be bound before that publish and it is
	// cancelled by the same stop as everything else. Nothing between here and
	// watchSignals below blocks on it, so moving it earlier changes when the
	// context exists and nothing else.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	var (
		proxyPort     int
		proxyFellBack bool
		proxyDone     chan struct{}
	)
	if o.StartProxy != nil {
		proxy, perr := o.StartProxy(runCtx)
		if perr != nil {
			log.Printf("not starting: %v", perr)
			return perr
		}
		proxyPort, proxyFellBack = proxy.Port(), proxy.FellBack()
		log.Printf("the codex proxy is listening on 127.0.0.1:%d", proxyPort)
		if proxyFellBack {
			log.Printf("that is not the port that was asked for; codex sessions started before this daemon must be relaunched")
		}
		proxyDone = make(chan struct{})
		go func() {
			defer close(proxyDone)
			if serr := proxy.Serve(runCtx); serr != nil {
				log.Printf("the codex proxy stopped: %v", serr)
			}
		}()
	}

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
		s.CodexProxyPort = proxyPort
		s.CodexProxyFellBack = proxyFellBack
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

	watchSignals(runCtx, stop, log)
	// The same stop, reached the only other way it can be: Windows delivers no
	// signal to a DETACHED_PROCESS child, so a named shutdown event is the
	// mechanism there and this is a no-op everywhere else. Both routes end in
	// the same cancel, so shutdown stays ONE path — which is the property this
	// function is organised around.
	watchShutdownRequest(runCtx, stop, log)

	loop = &Loop{
		Tick: func(c context.Context) error {
			// Before the tick body rather than after it, so a tick that fails
			// does not also stop the housekeeping. It has its own cadence and
			// returns nothing.
			o.sweep()
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
	// The proxy first, then the engine, and only then the final document. Both
	// drains exist for one reason: a request the proxy is still forwarding
	// harvests a usage reading when it finishes, and a reading that landed
	// after the document saying the daemon had stopped would be a fact nothing
	// on the machine could account for.
	//
	// stop() is called explicitly rather than left to the deferred one, because
	// the loop can return without the context having been cancelled at all --
	// a wedged loop gives up on its own -- and the proxy would then never be
	// told to stop.
	stop()
	if proxyDone != nil {
		<-proxyDone
	}
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
