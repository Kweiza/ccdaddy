package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// The configured binary is the escape hatch for a machine where the walk
// cannot get the right answer -- a codex that is not on PATH at all, or two of
// them where the first is not the one wanted -- so it has to WIN rather than
// be a fallback.
func TestTheConfiguredCodexBinaryWinsOverThePathWalk(t *testing.T) {
	isolate(t)
	elsewhere := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "[codex]\nbinary = "+quoteTOML(elsewhere)+"\n")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) {
		t.Error("codexBinary walked PATH although [codex] binary names a codex")
		return "", errNoCodex
	}

	got, err := codexBinary()
	if err != nil {
		t.Fatalf("codexBinary = %v", err)
	}
	if got != elsewhere {
		t.Errorf("codexBinary = %q, want the configured %q", got, elsewhere)
	}
}

// A configured path that is not there is a usage error and not a silent
// fallback to PATH: the user said which codex to run, and running a different
// one would bill a session through a binary they did not choose.
func TestAConfiguredCodexBinaryThatIsNotThereIsAUsageError(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope", "codex")
	writeConfig(t, "[codex]\nbinary = "+quoteTOML(missing)+"\n")

	_, err := codexBinary()
	if err == nil {
		t.Fatal("codexBinary accepted a configured path that does not exist")
	}
	if !IsUsageError(err) {
		t.Errorf("codexBinary = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "codex.binary") {
		t.Errorf("the error does not name the key to fix: %v", err)
	}
}

// The walk is handed the SHIM DIRECTORY, and codexBinary is the only place in
// production that argument comes from. Handed an empty one instead, the walk's
// skip short-circuits and its first answer is <CCDAD_HOME>/bin/codex -- the
// shim, which execs `ccdad codex exec`, which resolves codex again: an
// unbounded loop with a process per turn of it.
//
// The stub CAPTURES its argument rather than discarding it, which is the whole
// point of this test. A stub of shape func(string) (string, error) that ignores
// what it is given is green whatever codexBinary passes, including "".
func TestTheUnconfiguredCodexBinaryHandsTheWalkTheShimDirectory(t *testing.T) {
	isolate(t)
	// No [codex] binary: this is the branch where the walk decides.
	want := shimDir()
	if want == "" {
		t.Fatal("shimDir is empty in this test's environment, so it cannot be told apart from an unskipped walk")
	}
	elsewhere := filepath.Join(t.TempDir(), "codex")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	var got string
	called := false
	resolveCodex = func(shim string) (string, error) {
		called, got = true, shim
		return elsewhere, nil
	}

	binary, err := codexBinary()
	if err != nil {
		t.Fatalf("codexBinary = %v", err)
	}
	if !called {
		t.Fatal("codexBinary did not walk PATH although no [codex] binary is set")
	}
	if binary != elsewhere {
		t.Errorf("codexBinary = %q, want the walk's answer %q", binary, elsewhere)
	}
	if got != want {
		t.Errorf("the PATH walk was handed %q, want the shim directory %q: a walk that is not told "+
			"which directory to skip answers with the shim itself, and the shim execs "+
			"`ccdad codex exec`, which walks again -- an unbounded process loop", got, want)
	}
}

// quoteTOML is a basic TOML string. The paths here are t.TempDir()s, which hold
// no quote or backslash on any platform this runs on, so nothing more is needed
// and anything more would be a second TOML encoder in the test suite.
func quoteTOML(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

// The measured fact this exists for: an HTTP_PROXY or ALL_PROXY in the
// environment CAPTURES a loopback base_url, and only a BARE `127.0.0.1` entry
// in NO_PROXY exempts it -- `localhost` does not, and `127.0.0.1:<port>` does
// not. Without the exemption every request codex makes to ccdad's own listener
// goes to the user's corporate proxy instead, and the symptom is codex's
// endless "Reconnecting" with no error text at all.
func TestWithNoProxyLoopbackAddsTheBareLoopbackEntry(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		env                  []string
		wantUpper, wantLower string
	}{
		{
			name:      "neither set",
			env:       []string{"HOME=/home/u"},
			wantUpper: "127.0.0.1", wantLower: "127.0.0.1",
		},
		{
			name:      "only the upper-case one is set, and the lower borrows it",
			env:       []string{"NO_PROXY=example.com"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "only the lower-case one is set, and the upper borrows it",
			env:       []string{"no_proxy=example.com"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "each keeps its own value",
			env:       []string{"NO_PROXY=a.example", "no_proxy=b.example"},
			wantUpper: "a.example,127.0.0.1", wantLower: "b.example,127.0.0.1",
		},
		{
			name:      "a bare entry that is already there is not doubled",
			env:       []string{"NO_PROXY=127.0.0.1,example.com"},
			wantUpper: "127.0.0.1,example.com", wantLower: "127.0.0.1,example.com",
		},
		{
			name:      "a bare entry with spaces around it counts",
			env:       []string{"NO_PROXY=example.com, 127.0.0.1"},
			wantUpper: "example.com, 127.0.0.1", wantLower: "example.com, 127.0.0.1",
		},
		{
			name:      "localhost is NOT the loopback entry",
			env:       []string{"NO_PROXY=localhost"},
			wantUpper: "localhost,127.0.0.1", wantLower: "localhost,127.0.0.1",
		},
		{
			name:      "a host:port entry is NOT the loopback entry",
			env:       []string{"NO_PROXY=127.0.0.1:8080"},
			wantUpper: "127.0.0.1:8080,127.0.0.1", wantLower: "127.0.0.1:8080,127.0.0.1",
		},
		{
			name:      "a trailing comma does not become an empty component",
			env:       []string{"NO_PROXY=example.com,"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "an empty value is the same as unset",
			env:       []string{"NO_PROXY=", "no_proxy="},
			wantUpper: "127.0.0.1", wantLower: "127.0.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withNoProxyLoopback(tc.env)
			if v := envValueOf(got, "NO_PROXY"); v != tc.wantUpper {
				t.Errorf("NO_PROXY = %q, want %q", v, tc.wantUpper)
			}
			if v := envValueOf(got, "no_proxy"); v != tc.wantLower {
				t.Errorf("no_proxy = %q, want %q", v, tc.wantLower)
			}
		})
	}
}

// Nothing is ever REMOVED. This makes an exception for one host; it does not
// turn the user's proxy off, and a launcher that quietly unset HTTP_PROXY
// would send every request codex makes to anything else -- an npm install, a
// docs fetch -- straight out of a network that requires one.
func TestWithNoProxyLoopbackNeverRemovesAProxyVariable(t *testing.T) {
	env := withNoProxyLoopback([]string{
		"HTTP_PROXY=http://corp:3128",
		"HTTPS_PROXY=http://corp:3128",
		"ALL_PROXY=socks5://corp:1080",
	})
	for _, want := range []string{
		"HTTP_PROXY=http://corp:3128",
		"HTTPS_PROXY=http://corp:3128",
		"ALL_PROXY=socks5://corp:1080",
	} {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is gone from the child environment: %q", want, env)
		}
	}
}

// setEnv replaces rather than appends, so a variable the parent exported twice
// does not reach the child twice with two different values.
//
// The single copy that survives carries the LAST of them, which is the value
// os/exec would have handed the child and therefore the one it would have read.
// Counting alone does not say that: reading the FIRST occurrence instead still
// leaves exactly one copy, and it holds a value the child was never going to
// see.
func TestWithNoProxyLoopbackLeavesOneCopyOfEachVariable(t *testing.T) {
	env := withNoProxyLoopback([]string{"NO_PROXY=a", "NO_PROXY=b"})
	n, got := 0, ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_PROXY=") {
			n++
			got = kv
		}
	}
	if n != 1 {
		t.Errorf("the child environment holds %d copies of NO_PROXY: %q", n, env)
	}
	if want := "NO_PROXY=b,127.0.0.1"; got != want {
		t.Errorf("the surviving copy of NO_PROXY is %q, want %q", got, want)
	}
}

// writeCodexProxyStatus publishes the one field of the status document these
// tests are about. It writes the real file rather than stubbing a reader,
// because the launcher's whole claim is that it reads what the daemon
// published -- there being no other channel between the two processes.
func writeCodexProxyStatus(t *testing.T, port int) {
	t.Helper()
	path := mustPath(daemon.StatusPath())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schemaVersion":1,"codexProxyPort":%d}`, port)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeProxy is a listener answering ccdad's health route, and its port.
func fakeProxy(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codexproxy.HealthPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"ccdad":"test","port":0}`)
	}))
	t.Cleanup(srv.Close)
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// A held singleton is NOT evidence of a proxy. `ccdad auto` holds the same lock
// and runs no listener at all, and a daemon whose bind failed holds it too --
// so a launch that stopped at the lock would point codex at a port nothing
// answers, whose only symptom is codex's endless "Reconnecting" with no error
// text anywhere.
func TestTheCodexLaunchProvesTheListenerAndNotTheSingleton(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	// A daemon that is running and publishing, with no proxy port in what it
	// published. `ccdad auto` holds the same singleton and runs no listener at
	// all, and a daemon whose bind failed holds it too.
	writeCodexProxyStatus(t, 0)

	port, reason := codexProxyForLaunch()
	if reason == "" {
		t.Fatalf("the launch took a held singleton as proof of a proxy and answered port %d", port)
	}
	if !strings.Contains(reason, "codex proxy port") {
		t.Errorf("the reason does not say what was missing: %q", reason)
	}
}

func TestTheCodexLaunchTakesThePortTheDaemonPublished(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	want := fakeProxy(t)
	writeCodexProxyStatus(t, want)

	got, reason := codexProxyForLaunch()
	if reason != "" {
		t.Fatalf("codexProxyForLaunch refused a live proxy: %s", reason)
	}
	if got != want {
		t.Errorf("codexProxyForLaunch = %d, want the published %d", got, want)
	}
}

// A published port that nothing answers is a refusal rather than an answer.
// The daemon may have died since it published, or something else may have taken
// the port -- and both leave codex talking to nothing.
func TestTheCodexLaunchRefusesAPortNothingAnswers(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	// Port 1 is privileged and nothing in a test environment listens on it.
	writeCodexProxyStatus(t, 1)

	if port, reason := codexProxyForLaunch(); reason == "" {
		t.Fatalf("codexProxyForLaunch accepted port %d with nothing listening on it", port)
	}
}

// The health probe must not go through the environment's own proxy. The
// launcher's shell may export HTTP_PROXY or ALL_PROXY, and a client that
// consulted them would meet, one process earlier, the very capture the child's
// NO_PROXY exempts it from.
//
// TWO arms, because the end-to-end one cannot fail on its own. MEASURED: Go's
// http.ProxyFromEnvironment already answers nil for a 127.0.0.1 target with
// HTTP_PROXY and ALL_PROXY both exported, so the request below reaches its
// listener whether or not this client was handed a proxy function. The second
// arm asserts the guarantee where it actually lives -- a transport that
// consults nothing for ANY host, which is the half a loopback request can never
// show and the half a mutation can break.
func TestTheCodexHealthProbeIgnoresTheEnvironmentsProxy(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	want := fakeProxy(t)
	writeCodexProxyStatus(t, want)

	got, reason := codexProxyForLaunch()
	if reason != "" {
		t.Fatalf("the health probe went through the environment's proxy: %s", reason)
	}
	if got != want {
		t.Errorf("codexProxyForLaunch = %d, want %d", got, want)
	}

	tr, ok := codexHealthClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("codexHealthClient.Transport is %T, want a *http.Transport naming no proxy", codexHealthClient.Transport)
	}
	if tr.Proxy != nil {
		t.Error("codexHealthClient carries a proxy function, and this probe must consult none: " +
			"the loopback exemption it would be relying on is the standard library's own policy, " +
			"not a promise ccdad made")
	}
}

// The refusal predicate is shared with the auto-start hook, and this is the arm
// that matters here: inside a `ccdad run` session, no daemon may be started,
// and the launcher has to say so rather than spawning one.
func TestTheCodexLaunchWillNotStartADaemonInsideASession(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	enterFullProfileSession(t, "acct-1")

	_, reason := codexProxyForLaunch()
	if reason == "" {
		t.Fatal("the launcher started a daemon from inside a `ccdad run --full-profile` session")
	}
	if f.spawns != 0 {
		t.Errorf("the launcher spawned %d daemons from inside a session", f.spawns)
	}
}

// With no daemon and nothing refusing, the launcher starts one and WAITS -- the
// auto-start hook deliberately does not wait, and a launcher that did not
// either would point codex at a port that has not been published yet.
func TestTheCodexLaunchStartsADaemonAndWaitsForIt(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{takeAfter: 2})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	want := fakeProxy(t)
	writeCodexProxyStatus(t, want)

	got, reason := codexProxyForLaunch()
	if reason != "" {
		t.Fatalf("codexProxyForLaunch = %q, want a port", reason)
	}
	if got != want {
		t.Errorf("codexProxyForLaunch = %d, want %d", got, want)
	}
	if f.spawns != 1 {
		t.Errorf("the launcher spawned %d daemons, want exactly 1", f.spawns)
	}
	// The WAIT, which none of the three assertions above can see: the port is
	// already published and the listener already up, so a launcher that returned
	// the moment it had spawned answers the same port just as fast. What tells
	// the two apart is the daemon's state at the moment of the answer. This fake
	// takes two probes to reach its first lock, so a launcher that did not wait
	// leaves it still starting.
	if !f.held {
		t.Error("codexProxyForLaunch answered before the daemon it started had taken the singleton")
	}
}
