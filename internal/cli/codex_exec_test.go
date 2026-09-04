package cli

import (
	"errors"
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

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
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
	return listeningPort(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codexproxy.HealthPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"ccdad":"test","port":0}`)
	})
}

// listeningPort starts a loopback server and answers the port it took, which is
// the only part of it a launcher ever sees: the daemon publishes a number, and
// what is behind that number is exactly what the launcher's probe has to decide
// about.
func listeningPort(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
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

// A port that ANSWERS but is not ccdad is refused, and the status code is the
// only thing that tells the two apart. Ports are reused: a status document
// written by a daemon that has since died names a number some other program can
// take, and a launcher that accepted a completed connection as proof would point
// codex at a stranger's server. codex's symptom then is its own endless
// "Reconnecting", with no error text anywhere.
//
// A refused connection cannot show this. There the probe fails at the transport,
// before any status code exists, so the check under test is never reached -- the
// listener here completes the connection and answers every route, ccdad's health
// route included, with a 404.
func TestTheCodexLaunchRefusesAPortAStrangerAnswers(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	stranger := listeningPort(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	writeCodexProxyStatus(t, stranger)

	port, reason := codexProxyForLaunch()
	if reason == "" {
		t.Fatalf("codexProxyForLaunch accepted port %d, where something that is not ccdad answered", port)
	}
	if !strings.Contains(reason, "404") {
		t.Errorf("the reason does not say what answered instead: %q", reason)
	}
}

// "Cannot determine" never becomes "not running". A filesystem where locks do
// not work answers every probe with an error, and a launcher that read that as
// "no daemon here" would spawn one per invocation forever -- none of which can
// take the lock either.
//
// The spawn COUNT is the load-bearing assertion. The reason reads the same
// whichever way the launcher reached it, because the wait after a spawn reports
// an unprobeable lock in those very words too; only the count separates a
// launcher that refused from one that spawned first and gave up afterwards.
func TestTheCodexLaunchWillNotSpawnADaemonOnAnUnprobeableLock(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{probeErr: errors.New("ENOLCK")})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")

	_, reason := codexProxyForLaunch()
	if reason == "" {
		t.Fatal("the launcher took a lock it cannot probe as a daemon that is already running")
	}
	if !strings.Contains(reason, "cannot be probed") {
		t.Errorf("the reason does not say the lock could not be probed: %q", reason)
	}
	if f.spawns != 0 {
		t.Errorf("the launcher spawned %d daemons on a lock it cannot probe; on a filesystem where "+
			"locks do not work that is one more daemon on every invocation, forever", f.spawns)
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

// routedWorld is a machine with a live daemon, a live proxy and a stubbed
// codex, and it returns the spec the launcher handed the child.
func routedWorld(t *testing.T, code ExitCode, during func(launchSpec)) (*claudeStub, int) {
	t.Helper()
	// The Claude resolver is stubbed too, so a Codex launch that wrongly took
	// the Claude route fails the same way on every machine rather than on
	// whether claude happens to be installed.
	stubLookClaude(t, filepath.Join(t.TempDir(), "claude"))
	// A launcher can itself be running inside a routed codex session -- an
	// agent shell that inherited the parent's key, or a user who exported one
	// by hand. Exporting one here makes every key assertion below about what
	// the launcher BUILT rather than about what the developer's shell held.
	t.Setenv(codexKeyEnv, "inherited-from-a-parent-session")
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	port := fakeProxy(t)
	writeCodexProxyStatus(t, port)

	codex := filepath.Join(t.TempDir(), "codex")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) { return codex, nil }

	var stub claudeStub
	savedChild := startChild
	t.Cleanup(func() { startChild = savedChild })
	startChild = func(spec launchSpec) (ExitCode, error) {
		stub.started, stub.spec = true, spec
		if during != nil {
			during(spec)
		}
		return code, nil
	}
	return &stub, port
}

// The seven overrides are the whole wiring, and their shape was measured
// rather than assumed. Six declare one custom model provider and point codex
// at ccdad's listener; the seventh keeps the launch secret out of the
// environment codex hands to the commands the agent runs.
func TestCodexExecSpawnsCodexWithTheSevenOverridesAndTheKey(t *testing.T) {
	isolate(t)
	stub, port := routedWorld(t, ExitOK, nil)

	code, _, errOut, top := runRoot(t, "codex", "exec", "--", "exec", "say hi")
	if code != ExitOK {
		t.Fatalf("codex exec = %d, want 0\n%s\n%s", code, errOut, top)
	}
	if !stub.started {
		t.Fatal("codex exec started no child")
	}
	want := []string{
		"-c", "model_provider=ccdad",
		"-c", `model_providers.ccdad.name="ccdad"`,
		"-c", fmt.Sprintf(`model_providers.ccdad.base_url="http://127.0.0.1:%d"`, port),
		"-c", `model_providers.ccdad.env_key="CCDAD_CODEX_KEY"`,
		"-c", "model_providers.ccdad.requires_openai_auth=false",
		"-c", `model_providers.ccdad.wire_api="responses"`,
		"-c", `shell_environment_policy.exclude=["CCDAD_CODEX_KEY"]`,
		"exec", "say hi",
	}
	if len(stub.spec.Args) != len(want) {
		t.Fatalf("codex was given %q, want %q", stub.spec.Args, want)
	}
	for i := range want {
		if stub.spec.Args[i] != want[i] {
			t.Fatalf("argument %d is %q, want %q\nfull: %q", i, stub.spec.Args[i], want[i], stub.spec.Args)
		}
	}
	switch v := envValueOf(stub.spec.Env, codexKeyEnv); {
	case v == "":
		t.Error("the child got no launch secret, so every request it makes is an unknown bearer")
	case v == "inherited-from-a-parent-session":
		t.Error("the child got the key this process inherited rather than this launch's own secret; " +
			"the proxy would attribute the session to somebody else's launch record")
	}
	if v := envValueOf(stub.spec.Env, "NO_PROXY"); !strings.Contains(v, "127.0.0.1") {
		t.Errorf("NO_PROXY = %q, so an HTTP_PROXY in the environment would capture every request to ccdad", v)
	}
}

// A SECOND `--` is codex's and survives. pflag consumes the first one as its
// own terminator, so the launcher must not strip another -- doing so would eat
// the separator a user typed for codex.
func TestCodexExecKeepsASecondSeparator(t *testing.T) {
	isolate(t)
	stub, _ := routedWorld(t, ExitOK, nil)

	if code, _, errOut, _ := runRoot(t, "codex", "exec", "--", "exec", "--", "-x"); code != ExitOK {
		t.Fatalf("codex exec = %d, want 0\n%s", code, errOut)
	}
	tail := stub.spec.Args[len(stub.spec.Args)-3:]
	if tail[0] != "exec" || tail[1] != "--" || tail[2] != "-x" {
		t.Errorf("codex was handed %q; the tail must be exec -- -x verbatim", stub.spec.Args)
	}
}

// `codex login` and `codex logout` both REVOKE the stored grant server-side,
// with no undo. Routed through the proxy, a logout would revoke a grant ccdad
// manages -- so they go to the real codex untouched, with no overrides and no
// key, talking to codex's own home.
func TestCodexLoginAndLogoutAreNotRouted(t *testing.T) {
	for _, verb := range []string{"login", "logout"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			stub, _ := routedWorld(t, ExitOK, nil)

			if code, _, errOut, _ := runRoot(t, "codex", "exec", "--", verb); code != ExitOK {
				t.Fatalf("codex exec -- %s = %d, want 0\n%s", verb, code, errOut)
			}
			if len(stub.spec.Args) != 1 || stub.spec.Args[0] != verb {
				t.Errorf("codex %s was given %q, want exactly [%q]: a routed login talks to ccdad's proxy "+
					"about a grant ccdad owns", verb, stub.spec.Args, verb)
			}
			// routedWorld exports a key into this process, so this asserts the
			// STRIP and not merely the absence: a launcher that passed
			// childEnv() straight through would hand `codex logout` the key it
			// inherited from whatever routed session started it.
			if v := envValueOf(stub.spec.Env, codexKeyEnv); v != "" {
				t.Errorf("codex %s got a launch secret (%d bytes); an unrouted codex must carry none",
					verb, len(v))
			}
		})
	}
}

// The launch record has to EXIST while the child runs -- the proxy validates a
// bearer against it on every request -- and be gone afterwards, so a machine
// does not accumulate one per session.
func TestTheLaunchRecordLivesExactlyAsLongAsTheChild(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	var duringCount int
	_, _ = routedWorld(t, ExitOK, func(launchSpec) {
		entries, err := os.ReadDir(codexlaunch.Dir(root))
		if err != nil {
			t.Errorf("no launch directory while the child was running: %v", err)
			return
		}
		duringCount = len(entries)
	})

	if code, _, errOut, _ := runRoot(t, "codex", "exec"); code != ExitOK {
		t.Fatalf("codex exec = %d, want 0\n%s", code, errOut)
	}
	// One .lock and one .json.
	if duringCount != 2 {
		t.Errorf("the launch directory held %d files while the child ran, want 2 (a lock and a record)", duringCount)
	}
	entries, err := os.ReadDir(codexlaunch.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the launch directory still holds %d files after the child exited", len(entries))
	}
}

// An UNPINNED launch that cannot reach a proxy runs codex anyway, untouched,
// and says so in words a user can act on. Refusing would make a broken daemon
// mean no codex at all, which is worse than a session ccdad cannot see.
func TestAnUnpinnedLaunchWithNoProxyRunsCodexUntouchedAndSaysSo(t *testing.T) {
	isolate(t)
	// As in routedWorld, and for the same reason: this launcher may itself be
	// inside a routed session, so the assertion below is about the strip.
	t.Setenv(codexKeyEnv, "inherited-from-a-parent-session")
	root := mustPath(ccpath.StoreHome())
	stubDaemonWorld(t, &fakeDaemon{held: true})
	unsetForTest(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	// A daemon holding the singleton and publishing no port.
	codex := filepath.Join(t.TempDir(), "codex")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) { return codex, nil }
	var stub claudeStub
	savedChild := startChild
	t.Cleanup(func() { startChild = savedChild })
	startChild = func(spec launchSpec) (ExitCode, error) {
		stub.started, stub.spec = true, spec
		return ExitOK, nil
	}

	code, _, errOut, _ := runRoot(t, "codex", "exec", "--", "exec", "hi")
	if code != ExitOK {
		t.Fatalf("an unpinned codex exec with no proxy = %d, want 0 (it runs codex anyway)\n%s", code, errOut)
	}
	if !stub.started {
		t.Fatal("no codex was started at all")
	}
	if len(stub.spec.Args) != 2 || stub.spec.Args[0] != "exec" {
		t.Errorf("codex was given %q, want the arguments untouched", stub.spec.Args)
	}
	if v := envValueOf(stub.spec.Env, codexKeyEnv); v != "" {
		t.Error("an unrouted launch carried a launch secret")
	}
	if !strings.Contains(errOut, "NOT routed through ccdad") {
		t.Errorf("no banner said the session is not routed:\n%s", errOut)
	}
	if strings.Count(errOut, "\n") < 3 {
		t.Errorf("the banner is one line; it has to say why and how to fix it:\n%s", errOut)
	}
	if n := codexlaunch.UnroutedCount(root); n != 1 {
		t.Errorf("the unrouted tally is %d, want 1: doctor and status read it from there", n)
	}
}

func TestTheCodexExecPathIsAllowedInAScopedSession(t *testing.T) {
	if _, refused := scopedSessionRefusals["ccdad codex exec"]; refused {
		t.Error("`ccdad codex exec` is refused inside a scoped session; it replaces the scope in the child it launches, exactly as `ccdad run` does")
	}
	if !scopedSessionAllowed["ccdad codex exec"] {
		t.Error("`ccdad codex exec` has no scoped-session verdict")
	}
}
