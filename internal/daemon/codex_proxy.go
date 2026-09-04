package daemon

import (
	"context"
	"time"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// codexReapInterval is how often the launch records are swept. The tick reaches
// the sweep about once a second, and one sweep is a stat and a try-lock per
// live codex session, so the interval is what keeps a feature nobody is using
// from costing 86,400 filesystem round trips a day.
const codexReapInterval = 30 * time.Second

// Proxy is the part of the Codex proxy this process drives.
//
// It is an interface rather than the concrete type so that the ORDER this
// package is responsible for -- bind, publish, serve, drain, publish -- can be
// tested without taking a port, and so that nothing here depends on how the
// proxy answers a request.
type Proxy interface {
	Port() int
	FellBack() bool
	Serve(ctx context.Context) error
}

// startCodexProxy resolves the port and binds it.
//
// The context is not used to bind -- binding is immediate -- and Serve is
// handed the same one by the caller, which is what ties the listener's life to
// the daemon's.
func (e *Engine) startCodexProxy(context.Context) (Proxy, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := e.codexProxyConfig(root)
	if err != nil {
		return nil, err
	}
	srv, err := codexproxy.New(cfg)
	if err != nil {
		return nil, err
	}
	// Recorded AFTER the bind, from the port that was actually taken, so the
	// next daemon comes back on the same one and the codex sessions this one
	// started keep working across a restart. A failure to record costs a
	// different port next time and nothing else, so it is not fatal.
	if rerr := codexproxy.RecordPort(root, srv.Port()); rerr != nil {
		e.logf("could not record the codex proxy port: %v", rerr)
	}
	return srv, nil
}

// codexProxyConfig is everything the daemon is responsible for handing the
// proxy, for the store at root.
//
// It is separate from startCodexProxy so that what this process supplies can be
// asserted without taking a port -- the fields below are the whole seam between
// the daemon and the proxy, and a field wired to the wrong thing or left nil
// is invisible from the other side of a bind.
//
// The config is read HERE, from disk, rather than taken from e.Config(). The
// proxy is built before the first tick, and until a tick has run e.cfg is still
// the seeded default set, so reading the cache made [codex].proxy_port and
// [codex].cross_account_replay inert.
func (e *Engine) codexProxyConfig(root string) (codexproxy.Config, error) {
	cfg, _ := e.loadConfig()
	port, source, err := codexproxy.ResolvePort(root, cfg.Codex.ProxyPort)
	if err != nil {
		return codexproxy.Config{}, err
	}
	// Opened once, here, rather than per request: Credentials reads the file on
	// every call, so a token another process rotated is still picked up, and
	// re-opening the store per forwarded request would put a MkdirAll and two
	// chmods on the path of every codex turn.
	s, err := store.Open()
	if err != nil {
		return codexproxy.Config{}, err
	}
	return codexproxy.Config{
		Root:               root,
		Port:               port,
		PortSource:         source,
		Version:            buildinfo.Version,
		Refresher:          e.CodexRefresher,
		Book:               e.CodexBook,
		Accounts:           func() ([]store.Account, error) { return store.AccountsAt(root) },
		Credentials:        s.Credentials,
		RankedEligible:     e.codexRanked,
		CrossAccountReplay: cfg.Codex.CrossAccountReplay,
		Harvest:            e.harvestCodexSample,
		Log:                e.logf,
	}, nil
}

// codexRanked is the Codex lane's last ranking, best first.
//
// It hands out a COPY: the proxy reads this from a request goroutine while the
// lane replaces it from the tick.
func (e *Engine) codexRanked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.codexRankedUUIDs...)
}

// SetCodexRanked records what the Codex lane decided, so the proxy serves new
// threads from the same order the lane would rotate through.
func (e *Engine) SetCodexRanked(order []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.codexRankedUUIDs = append([]string(nil), order...)
}

// harvestCodexSample keeps the newest reading the proxy took off a real
// inference response.
//
// It is kept rather than committed here because committing is the lane's job
// and the lane owns the cache's write ordering. What this buys is a reading
// taken by a request the user actually made, which costs nothing and is exactly
// current -- the lane polls on a fifteen-minute floor, and a busy hour can
// spend an account between two polls.
func (e *Engine) harvestCodexSample(uuid string, snap *usage.Snapshot) {
	if uuid == "" || snap == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.codexSamples == nil {
		e.codexSamples = make(map[string]*usage.Snapshot)
	}
	e.codexSamples[uuid] = snap
}

// CodexSample hands the lane the newest harvested reading for uuid and forgets
// it, so one reading is committed once rather than on every tick until another
// replaces it.
func (e *Engine) CodexSample(uuid string) (*usage.Snapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snap, ok := e.codexSamples[uuid]
	if !ok {
		return nil, false
	}
	delete(e.codexSamples, uuid)
	return snap, true
}

// rankedUUIDs flattens the lane's ranking to the order the proxy reads.
//
// Result.Order is the main pool, best first. Only the uuids travel: the proxy
// has no use for the figures the ranking was made from, and copying them would
// make it look as though it might act on them.
func rankedUUIDs(ev switcher.Evaluation) []string {
	out := make([]string, 0, len(ev.Plan.Result.Order))
	for _, r := range ev.Plan.Result.Order {
		out = append(out, r.UUID)
	}
	return out
}

// reapCodexLaunches removes the launch records of codex processes that are
// gone, on its own cadence.
//
// Nothing else does this. A Lookup only ever reaches the record a LIVE codex
// quotes, so a machine that ran ten sessions and lost all ten to a reboot would
// keep ten pairs of files until somebody deleted them by hand.
func (e *Engine) reapCodexLaunches() {
	root, err := storeRoot()
	if err != nil {
		return
	}
	now := e.now()
	e.mu.Lock()
	if now.Before(e.nextCodexReapAt) {
		e.mu.Unlock()
		return
	}
	e.nextCodexReapAt = now.Add(codexReapInterval)
	e.mu.Unlock()

	reaped, err := codexlaunch.Reap(root)
	if err != nil {
		e.logf("sweeping the codex launch records: %v", err)
		return
	}
	if reaped > 0 {
		e.logf("removed %d codex launch record(s) whose launcher is gone", reaped)
	}
}
