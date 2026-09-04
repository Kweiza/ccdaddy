package codexproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

const (
	// HealthPath is the one unauthenticated route. A launcher proves the
	// LISTENER rather than the daemon process with it, because holding the
	// daemon singleton is not evidence that a proxy is up.
	HealthPath = "/ccdad/health"
	// ResponsesPath is the only route codex uses, and the only one that
	// forwards anything.
	ResponsesPath = "/responses"
	// DefaultUpstream is where a forwarded request goes.
	DefaultUpstream = "https://chatgpt.com/backend-api/codex/responses"
	// MaxBody caps the request body held in memory. A turn carries the whole
	// history, so this is generous on purpose; unbounded it would be a way for
	// anything on the machine to exhaust the daemon.
	MaxBody = 32 << 20
	// MaxUnauthenticated caps how many bearers may be checked at once. The
	// check is a stat and a try-lock, so this is not a throughput limit; it is
	// what stops an unauthenticated flood from turning into filesystem work.
	MaxUnauthenticated = 16

	// maxErrorBody caps a non-2xx body, which the proxy has to hold because it
	// may have to re-answer with it after a failed replay.
	maxErrorBody = 1 << 20

	// drainTimeout bounds the wait for in-flight requests on shutdown. A
	// streamed turn can legitimately run for minutes, and the daemon is on its
	// way out, so the wait is bounded and then the connections are cut.
	drainTimeout = 5 * time.Second

	// responseHeaderTimeout bounds the wait for the upstream's status line. The
	// client deliberately has no overall timeout: the body that follows is a
	// stream that runs as long as the model does.
	responseHeaderTimeout = 120 * time.Second
)

// Config is everything the proxy needs and nothing it can resolve itself.
//
// Accounts, Credentials, RankedEligible and Refresher are funcs rather than
// concrete types for the reason the daemon engine's poller seams are: the
// daemon hands in the store under its own lock, and a test describes the same
// behaviour without one.
type Config struct {
	// Root is the ccdad store home.
	Root string
	// Port is the port to bind, from ResolvePort.
	Port int
	// PortSource is where Port came from: "config", "recorded" or "derived". A
	// bind failure is fatal for "config" and a fallback otherwise.
	PortSource string
	// Version is what the health route reports.
	Version string
	// Upstream is the URL every forwarded request goes to. Empty means
	// DefaultUpstream.
	Upstream string
	// Client makes the upstream request. Nil means this package's own.
	Client *http.Client
	// Refresher is the daemon's ONE Codex refresher. Nil means a 401 is passed
	// through rather than repaired.
	Refresher *codexauth.Refresher
	// RefreshFunc overrides Refresher. It exists so a test can describe an
	// outcome without a store and a token endpoint, and production leaves it
	// nil -- there must be exactly one refresher per daemon, and this is not a
	// second place to build one.
	RefreshFunc func(ctx context.Context, uuid, triggeredBy string) (codexauth.Outcome, error)
	// Accounts reads the account rows.
	Accounts func() ([]store.Account, error)
	// Credentials reads one account's credential blob.
	Credentials func(uuid string) (cclink.Blob, error)
	// RankedEligible is the lane's last ranking, best first. Nil or empty means
	// the proxy derives an order from the store itself, which is what a daemon
	// whose lane has not run yet has.
	RankedEligible func() []string
	// CrossAccountReplay allows a thread that already has responses from one
	// account to be replayed onto another.
	CrossAccountReplay bool
	// Book is the rate-limit bookkeeping shared with the lane. Nil means the
	// server keeps its own.
	Book *LimitBook
	// Lookup validates a launch bearer. Nil means codexlaunch.Lookup.
	Lookup func(root, bearer string) (codexlaunch.Record, codexlaunch.LookupResult, error)
	// Harvest is handed a usage reading taken off a real inference response.
	// Nil means the readings are dropped.
	Harvest func(uuid string, snap *usage.Snapshot)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log records what the proxy decided. It never receives a bearer, an
	// upstream Authorization, a body or the turn metadata header.
	Log func(format string, a ...any)
}

// Server is the bound proxy.
type Server struct {
	cfg      Config
	book     *LimitBook
	ln       net.Listener
	port     int
	fellBack bool

	// unauth is the pre-authentication admission gate.
	unauth chan struct{}

	mu sync.Mutex
	// threads maps a thread id to the account that produced its first
	// response, so a thread stays with the account whose encrypted reasoning
	// its later turns carry.
	threads map[string]string
	// auth caches a bearer's hash for a short window, so a burst of turns does
	// not stat and lock the same record on every request.
	auth map[string]authHit
}

// New binds the listener.
//
// A bind failure on a port the USER configured is a refusal: silently serving
// on a different one would leave every codex session pointed at a port nothing
// answers, and codex's symptom for that is an endless reconnect rather than an
// error. A bind failure on a port ccdad chose for itself falls back, because
// the alternative is a daemon that will not start.
func New(cfg Config) (*Server, error) {
	s, err := newServer(cfg)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		if cfg.PortSource == "config" {
			return nil, fmt.Errorf("the codex proxy cannot bind the configured port %d: %w", cfg.Port, lerr)
		}
		ln, lerr = net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			return nil, fmt.Errorf("the codex proxy cannot bind a loopback port: %w", lerr)
		}
		s.fellBack = true
	}
	s.ln = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	return s, nil
}

// newServer builds everything but the listener, so a handler test never takes
// a port.
func newServer(cfg Config) (*Server, error) {
	if cfg.Root == "" {
		return nil, errors.New("the codex proxy was given no store root")
	}
	if cfg.Upstream == "" {
		cfg.Upstream = DefaultUpstream
	}
	if cfg.Client == nil {
		cfg.Client = defaultClient()
	}
	if cfg.Lookup == nil {
		cfg.Lookup = codexlaunch.Lookup
	}
	book := cfg.Book
	if book == nil {
		book = &LimitBook{}
	}
	return &Server{
		cfg:     cfg,
		book:    book,
		port:    cfg.Port,
		unauth:  make(chan struct{}, MaxUnauthenticated),
		threads: make(map[string]string),
		auth:    make(map[string]authHit),
	}, nil
}

// defaultClient has no overall timeout on purpose: the answer is a stream that
// runs as long as the model does. What is bounded is the wait for the status
// line, which is the part a hung upstream would otherwise hold forever.
//
// The upstream is FIXED, and CheckRedirect is what makes that true. Go's
// default client follows up to ten hops, and for a 307 or 308 it replays the
// BODY -- which here is the whole codex turn, the thread history including its
// encrypted reasoning, travelling beside the workspace id and codex's own
// installation id in the turn metadata; a hop that stays on the upstream's own
// hostname carries the Authorization ccdad built as well. Measured: with this
// nil, a 307 from the fake upstream to a second listener delivered the body and
// both identifying headers to the second listener. send() also harvests the
// rate-limit headers off whatever finally answered, so a followed redirect
// could have a quota reading committed for the account that paid for the turn.
// http.ErrUseLastResponse hands the 3xx back unfollowed, and the attempt loop
// then passes it to codex like any other non-2xx. The Codex refresher guards
// its own client the same way, for the same reason.
func defaultClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Port is the port the listener actually took.
func (s *Server) Port() int { return s.port }

// FellBack reports that the listener is not on the port that was asked for, so
// codex sessions started before this daemon must be relaunched.
func (s *Server) FellBack() bool { return s.fellBack }

// Close releases the listener without serving. Serve does this itself; this is
// for a caller that bound and then decided not to run.
func (s *Server) Close() error {
	if s == nil || s.ln == nil {
		return nil
	}
	return s.ln.Close()
}

// Serve runs until ctx is done and drains what is in flight.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("the codex proxy was asked to serve without a listener")
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// net/http's own panic line would carry a request path and nothing
		// else useful; the guard below logs what matters through the daemon's
		// log instead.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	// TWO ways out, and the served channel is why. Waiting only on ctx.Done
	// would hang this function forever whenever Serve returned first -- a
	// listener closed under it, an Accept that failed -- because the goroutine
	// that closes the gate would still be waiting for a cancel that is never
	// coming, and the daemon's shutdown waits on this returning.
	served := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		select {
		case <-ctx.Done():
		case <-served:
			return
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			_ = srv.Close()
		}
	}()
	err := srv.Serve(s.ln)
	close(served)
	<-closed
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler is the routing table, wrapped in the never-500 guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, s.health)
	mux.HandleFunc(ResponsesPath, s.responses)
	mux.HandleFunc("/", s.notFound)
	return s.guard(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.notFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, healthBody{CCDAD: s.cfg.Version, Port: s.Port()})
}

func (s *Server) notFound(w http.ResponseWriter, _ *http.Request) {
	writeNotFound(w)
}

// guard is the never-500 rule, as code.
//
// codex answers the two failure statuses very differently: a 429 costs ONE
// request and stops, and a 500 costs thirty requests over twenty-five seconds.
// A fault in this proxy must therefore never reach codex as a 5xx, or one bug
// becomes thirty attempts at somebody's quota.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &trackedWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				// net/http's own signal for "drop this connection without a
				// terminating chunk". It is how a broken upstream stream is
				// passed on, not a fault of this proxy's.
				panic(v)
			}
			s.logf("the codex proxy recovered from a fault on %s: %v", r.URL.Path, v)
			if tracked.wrote {
				// The status is already on the wire. Ending the body cleanly
				// would tell codex the turn finished, so the honest move left
				// is to break the connection.
				panic(http.ErrAbortHandler)
			}
			writeUnavailable(tracked)
		}()
		next.ServeHTTP(tracked, r)
	})
}

// trackedWriter remembers whether a status line has gone out, which is what
// decides whether a late fault can still be answered.
type trackedWriter struct {
	http.ResponseWriter
	wrote  bool
	status int
}

func (w *trackedWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote, w.status = true, status
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote, w.status = true, http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps the streaming path working through the wrapper: without it the
// answer is buffered and codex renders nothing until the turn ends.
func (w *trackedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logf(format string, a ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(format, a...)
	}
}

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// The bodies below are structs rather than maps because their field ORDER is
// part of what this proxy promises: a map marshals its keys alphabetically, and
// the answers codex reads were pinned with type first.

type healthBody struct {
	CCDAD string `json:"ccdad"`
	Port  int    `json:"port"`
}

type brandedBody struct {
	Error brandedError `json:"error"`
}

type brandedError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type waitingBody struct {
	Error waitingError `json:"error"`
}

type waitingError struct {
	Type string `json:"type"`
	// ResetsAt is omitted rather than zeroed when nothing said when the window
	// resets: codex renders an absent value as "try again later" and a zero as
	// a date in 1970.
	ResetsAt *int64 `json:"resets_at,omitempty"`
}

func writeUnknownLaunch(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, brandedBody{brandedError{
		Type:    "ccdad_unknown_launch",
		Message: "ccdad: this codex was not launched by the running ccdad daemon (or its launch record is gone). Quit codex and start it again; `ccdad which` shows the account it will use.",
	}})
}

func writeNeedsRelogin(w http.ResponseWriter, label string) {
	writeJSON(w, http.StatusUnauthorized, brandedBody{brandedError{
		Type:    "ccdad_needs_relogin",
		Message: "ccdad: " + label + " needs a new login; run `ccdad codex add`",
	}})
}

func writeRefreshTransient(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, brandedBody{brandedError{
		Type:    "ccdad_refresh_transient",
		Message: "ccdad: token refresh failed temporarily; ccdad will retry",
	}})
}

// writeUnavailable is the answer to every fault this proxy cannot turn into a
// real upstream answer. It is a 429 rather than a 5xx for the reason the guard
// gives.
func writeUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusTooManyRequests, brandedBody{brandedError{
		Type:    "ccdad_unavailable",
		Message: "ccdad: the request could not be completed; run `ccdad doctor` and read the ccdad daemon log",
	}})
}

func writeNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, brandedBody{brandedError{
		Type:    "ccdad_not_found",
		Message: "ccdad: this ccdad codex proxy answers POST /responses and GET /ccdad/health, and nothing else",
	}})
}

func writeWaiting(w http.ResponseWriter, resetsAt time.Time, known bool) {
	body := waitingBody{waitingError{Type: "usage_limit_reached"}}
	if known {
		at := resetsAt.Unix()
		body.Error.ResetsAt = &at
	}
	writeJSON(w, http.StatusTooManyRequests, body)
}

// writeJSON marshals v and writes it with its own length, so nothing this
// proxy generates is chunked.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		// Every value passed here is a struct of strings and numbers, so this
		// is unreachable; answering rather than panicking keeps the never-500
		// rule true by construction rather than by argument.
		data = []byte(`{"error":{"type":"ccdad_unavailable","message":"ccdad: the request could not be completed"}}`)
		status = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
