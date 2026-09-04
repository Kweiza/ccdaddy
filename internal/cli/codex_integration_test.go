//go:build !windows

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// seenRequest is what the fake upstream recorded about one arrival.
type seenRequest struct {
	method, path, auth, account, upgrade string
}

// The end-to-end proof, and the only test in this package that runs the real
// codex binary.
//
// Every other test here asserts what ccdad BUILDS. This asserts what codex DOES
// with it: that the overrides make it talk to the given port at all, that it
// sends POST /responses and nothing else, that it attempts no WebSocket, that
// the bearer arriving upstream is the account's and never the launch secret,
// and that an HTTP_PROXY in the environment does not divert the request.
//
// It spends no quota. The upstream is an httptest server on loopback, and the
// only thing that reaches the network is nothing.
//
// It SKIPS when codex is not installed, which is every CI machine. A skip that
// says so cannot be mistaken for a pass, which is what a stubbed "integration"
// test would be.
func TestARealCodexReachesTheUpstreamThroughTheProxy(t *testing.T) {
	codexPath, err := exec.LookPath(codexProgramName)
	if err != nil {
		t.Skip("codex is not installed; this test drives the real binary")
	}
	isolate(t)
	seedCodexAccount(t, "cx-int-1", "c@example.com")
	root := mustPath(ccpath.StoreHome())

	var mu sync.Mutex
	var seen []seenRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{
			method:  r.Method,
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			account: r.Header.Get("chatgpt-account-id"),
			upgrade: r.Header.Get("Upgrade"),
		})
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// The smallest stream codex accepts as a finished turn. If it does not,
		// codex reports its own error and the assertions below still hold: what
		// is being measured is the REQUEST, not the answer.
		io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"resp_1"}}`+"\n\n")
		io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","output":[]}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	srv, err := codexproxy.New(codexproxy.Config{
		Root:       root,
		Port:       0,
		PortSource: "derived",
		Version:    "integration-test",
		Upstream:   upstream.URL + "/codex/responses",
		Accounts: func() ([]store.Account, error) {
			s, oerr := store.Open()
			if oerr != nil {
				return nil, oerr
			}
			return s.Accounts(), nil
		},
		Credentials: func(uuid string) (cclink.Blob, error) {
			s, oerr := store.Open()
			if oerr != nil {
				return nil, oerr
			}
			return s.Credentials(uuid)
		},
		Log: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("binding the proxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-served
	})

	// A PINNED launch, so the account the upstream should see is decided here
	// rather than by a serving pointer this test would also have to write.
	launch, err := codexlaunch.Create(root, "cx-int-1")
	if err != nil {
		t.Fatalf("creating the launch record: %v", err)
	}
	t.Cleanup(func() { _ = launch.Close() })

	// HTTP_PROXY, HTTPS_PROXY and ALL_PROXY all point at a port nothing listens
	// on. If the NO_PROXY exemption is wrong, codex's request goes there and the
	// upstream below sees nothing at all -- which is exactly the measured
	// failure this guards: only a bare 127.0.0.1 entry exempts a loopback
	// base_url, and the symptom is codex's endless "Reconnecting" with no error.
	env := append(os.Environ(),
		"CODEX_HOME="+t.TempDir(),
		codexKeyEnv+"="+launch.Secret(),
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=http://127.0.0.1:1",
	)
	env = withNoProxyLoopback(env)

	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()
	// --skip-git-repo-check: codex exec refuses to run outside a git
	// repository, and the package directory being one is not a premise this
	// test should rest on. Without the flag, a run from a copied tree or with
	// child.Dir set exits 1 before any request leaves, and the failure reads as
	// a proxy or NO_PROXY defect instead of a missing flag.
	child := exec.CommandContext(runCtx, codexPath,
		append(codexOverrides(srv.Port()), "exec", "--skip-git-repo-check", "say hi")...)
	child.Env = env
	// Stdin is left nil, which os/exec points at the null device: `codex exec`
	// reads additional input from stdin and would otherwise wait for a terminal
	// that is not there.
	out, runErr := child.CombinedOutput()

	mu.Lock()
	got := append([]seenRequest(nil), seen...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatalf("the upstream saw no request at all.\ncodex exited with %v and said:\n%s", runErr, out)
	}
	first := got[0]
	if first.method != http.MethodPost {
		t.Errorf("the upstream saw %s, want POST", first.method)
	}
	if first.path != "/codex/responses" {
		t.Errorf("the upstream saw %s, want /codex/responses", first.path)
	}
	if want := "Bearer AT-cx-int-1"; first.auth != want {
		t.Errorf("Authorization = %q, want %q: the proxy rebuilds it from the store per attempt, and "+
			"the launch secret must never reach the upstream", first.auth, want)
	}
	if want := "acct-cx-int-1"; first.account != want {
		t.Errorf("chatgpt-account-id = %q, want %q", first.account, want)
	}
	if first.upgrade != "" {
		t.Errorf("the upstream saw Upgrade: %q. codex must attempt no WebSocket, which is what leaving "+
			"supports_websockets out of the overrides buys", first.upgrade)
	}
	for i, r := range got {
		if r.path != "/codex/responses" {
			t.Errorf("request %d went to %s; the overrides must leave codex with exactly one route", i, r.path)
		}
	}
}
