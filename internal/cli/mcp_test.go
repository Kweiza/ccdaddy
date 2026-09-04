package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/mcpsrv"
)

// The server's stdout IS the protocol. Anything the command printed there of
// its own would be a parse error in the client, and there is no partial-credit
// failure mode: the session simply does not open.
//
// Only a real process can say this. Every other test of this server builds it
// over mcp.NewInMemoryTransports, where stdout is not involved at all -- so
// nothing in the tree noticed if a command on the way in wrote a line. A
// completed handshake against a client that did not build the server is the
// assertion, and it is negative evidence of exactly the right kind: one stray
// byte and there is no InitializeResult to read.
func TestTheMCPCommandPrintsNothingOnStdoutOfItsOwn(t *testing.T) {
	isolate(t)
	cs := connectToRealServer(t)

	got := cs.InitializeResult()
	if got.ServerInfo.Name != mcpsrv.ServerName {
		t.Errorf("ServerInfo.Name = %q, want %q", got.ServerInfo.Name, mcpsrv.ServerName)
	}
	// The instructions are the one thing a client is told before it calls
	// anything, and they are the largest single piece of text in the
	// handshake -- the part most likely to be truncated if the framing were
	// wrong rather than merely the parse.
	if !strings.Contains(got.Instructions, "asks the person at the keyboard") {
		t.Errorf("the handshake did not carry the server's instructions:\n%s", got.Instructions)
	}
}

// The whole surface, reached the way Claude Code reaches it: a client that did
// not build this server, over a pipe, ending in a document the command tree
// itself wrote.
//
// The tool names are spot-checked one per class rather than counted. The count
// belongs to internal/mcpsrv's own registry test, which reads the class map
// that decides it; repeating the number here would mean two edits for one
// change and would still be asserting the same Register call.
func TestARealClientReachesTheToolsAndGetsTheCommandsOwnDocumentBack(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")
	cs := connectToRealServer(t)

	offered := map[string]bool{}
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("listing tools over a real pipe: %v", err)
		}
		offered[tool.Name] = true
	}
	// One per security class, so a group that failed to register is not read
	// as a smaller surface by a test that only ever looked at the reads.
	for _, want := range []string{"list", "primary", "switch", "daemon_status"} {
		if !offered[want] {
			t.Errorf("a real client is not offered %q", want)
		}
	}

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("calling list came back as a protocol error (%v); the model cannot see one", err)
	}
	if res.IsError {
		t.Fatalf("a clean list came back as an error: %s", resultText(res))
	}
	var out mcpsrv.ReadOut
	if err := json.Unmarshal([]byte(resultText(res)), &out); err != nil {
		t.Fatalf("the result is not the envelope: %v (%s)", err, resultText(res))
	}
	if !strings.Contains(out.Document, "work@example.com") {
		t.Errorf("the document did not come from the real command tree: %q", out.Document)
	}
}

// Every handler re-enters the tree through a fresh root, and the argument slice
// is never nil. Cobra falls back to os.Args[1:] on a nil one -- and for a
// process whose own argv is `ccdad mcp`, that means a NESTED protocol server on
// the same stdio, once per tool call.
//
// The fixture argv is `list --json` rather than the `mcp` a real server has,
// and the substitution is deliberate: the true failure BLOCKS on stdin, so a
// test that reproduced it exactly would hang instead of asserting. A command
// that writes a recognisable document is the same fallback with an answer.
//
// This exercises freshRootExec (TUI plan, Task V0) through this package's own
// call site rather than re-testing V0's unit tests for it: what is asserted
// here is that the seam `ccdad mcp` consumes is the one with the guarantee.
func TestTheExecutorAlwaysPassesANonNilArgumentSlice(t *testing.T) {
	isolate(t)
	// Both axes stubbed so the expected code is a fact about the executor
	// rather than about the machine: bare `ccdad` with no terminal is a usage
	// error, and `go test` gives the binary a pipe.
	stubTTYs(t, false, false)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{saved[0], "list", "--json"}

	ex := freshRootExec(NewRootCmd())
	code, stdout, stderr := ex([]string{})

	if strings.Contains(stdout, "schemaVersion") {
		t.Fatalf("an empty argument vector ran this process's own command line:\n%s", stdout)
	}
	if code != int(ExitUsage) {
		t.Fatalf("exit = %d (%s), want %d for a bare ccdad with no terminal", code, stderr, ExitUsage)
	}
}

// Both writers are redirected, and stdout is left alone. There are dozens of
// places in this package that write through the command's own writers, and any
// one of them left pointing at os.Stdout puts a human table into the JSON-RPC
// stream.
func TestTheExecutorCapturesBothWritersAndLeavesStdoutAlone(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")

	ex := freshRootExec(NewRootCmd())
	code, stdout, stderr := ex([]string{"status", "--json"})

	if code != int(ExitOK) {
		t.Fatalf("status --json = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, `"accounts"`) {
		t.Errorf("stdout = %q, want the document the command wrote", stdout)
	}
}

// THE assertion this whole design exists for: the session verdict is recomputed
// on every call, not once per process. Three consecutive runs in ONE process
// give verdict, refusal, verdict as the credential home moves in and out of
// ccdad's own sessions directory.
//
// A server that resolved the verdict at startup would pass the first and third
// legs and fail the middle one -- and on a real machine that is a `ccdad mcp`
// started outside a session and handed a switch from inside one.
func TestTheScopedSessionVerdictIsRecomputedOnEveryCallAndNotOncePerProcess(t *testing.T) {
	// isolate returns Claude Code's sandboxed config home, NOT ccdad's store,
	// so the store home is read back from the variable isolate set.
	isolate(t)
	home := os.Getenv("CCDAD_HOME")
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")
	seedAccount(t, "uuid-aaaa-0002", "spare@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-aaaa-0001", ""))
	ex := freshRootExec(NewRootCmd())

	if code, _, stderr := ex([]string{"switch", "uuid-aaaa-0002"}); code == int(ExitUsage) {
		t.Fatalf("refused outside a session: %s", stderr)
	}

	// Inside one of ccdad's own session credential homes. The directory needs
	// not exist: `inside` tries filepath.Rel on the absolute paths first and
	// only falls through to EvalSymlinks when that says no.
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", filepath.Join(home, SessionsDirName, "uuid-aaaa-0001"))
	code, _, stderr := ex([]string{"switch", "uuid-aaaa-0001"})
	if code != int(ExitUsage) {
		t.Fatalf("switch inside a session = %d, want %d; the verdict was computed once and cached",
			code, ExitUsage)
	}
	if !strings.Contains(stderr, refusalMarker) {
		t.Errorf("stderr = %q, want the refusal ccdad already writes", stderr)
	}

	// Back out again.
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", t.TempDir())
	if code, _, stderr := ex([]string{"switch", "uuid-aaaa-0002"}); code == int(ExitUsage) {
		t.Fatalf("still refused after leaving the session: %s", stderr)
	}
}

// `ccdad mcp` declares no --json flag, and that is a decision rather than an
// omission: its stdout is protocol, not a document.
// TestJSONContractCoversEveryJSONCommand fires only for a command that declares
// the flag and is runnable, so it stays silent here -- and declaring the flag
// would pull this command into four contract rules it cannot honestly satisfy
// and make it an exception. The bare dashboard is the mirror-image case.
func TestTheMCPCommandDeclaresNoJSONFlagBecauseItsStdoutIsNotADocument(t *testing.T) {
	cmd := newMCPCmd()
	if f := cmd.Flags().Lookup("json"); f != nil {
		t.Error("`ccdad mcp` declares --json; its stdout carries the protocol, and the flag would " +
			"promise a document contract it cannot satisfy")
	}
}

// The allowed verdict, exercised rather than only declared -- and the two
// refusals, pinned in the commit that wrote them rather than in the one that
// registers the subcommands.
//
// It is NOT a row in TestReadsAndStoreOnlyCommandsStillRunInsideARunSession
// beside `tui` and the reads, and that is a measurement rather than a
// preference. `ccdad mcp` blocks on stdin, and the two shapes of it that
// RETURN both short-circuit inside cobra before any persistent pre-run hook
// runs: a help flag is answered, and positional arguments are validated,
// several steps before the hook loop. A row driving either would pass with
// this verdict deleted from the map, which is worse than no row at all. So the
// gate is called at the seam the root calls it from, with the command the tree
// actually holds.
func TestTheServerIsAllowedInsideARunSessionAndItsInstallerIsNot(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")
	enterRunSession(t, "uuid-aaaa-0001")

	var served *cobra.Command
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "mcp" {
			served = c
		}
	}
	if served == nil {
		t.Fatal("`ccdad mcp` is not registered on the root")
	}
	if err := refuseInsideScopedSession(served); err != nil {
		t.Fatalf("`ccdad mcp` was refused inside a session: %v", err)
	}

	// Both halves of the installer are refused, and both entries are written
	// here rather than by the task that registers them: scoped.go has one
	// owner, and an entry for a path that is not registered yet is harmless --
	// the totality gate only looks for a registered path with no verdict, and
	// for a path in both maps.
	for _, path := range []string{"ccdad mcp install", "ccdad mcp uninstall"} {
		if _, refused := scopedSessionRefusals[path]; !refused {
			t.Errorf("%q has no refusal; the command that registers it would redden the totality "+
				"gate from a file nowhere near itself", path)
		}
	}
}

// connectToRealServer re-execs this test binary into the `ccdad mcp` role and
// speaks the protocol to it over a real pipe, through the SDK's own command
// transport.
//
// It is the only place in this repository where the server runs as a process.
// Two of the things it owes are properties of a process rather than of a value
// -- that nothing but protocol reaches stdout, and that a client which did not
// build the server completes the handshake -- and neither can be asserted by
// constructing one. The caller must have called isolate first: the child gets
// this test's sandbox through the environment.
func connectToRealServer(t *testing.T) *mcp.ClientSession {
	t.Helper()

	// Stderr goes to a file rather than to a buffer this goroutine also reads:
	// the child writes it concurrently with everything below, and a
	// strings.Builder shared across that boundary is a data race that -race
	// would find before any assertion did.
	errPath := filepath.Join(t.TempDir(), "server-stderr")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := func() string {
		b, rErr := os.ReadFile(errPath)
		if rErr != nil {
			return "(stderr unreadable: " + rErr.Error() + ")"
		}
		return string(b)
	}

	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(),
		mcpRoleEnv+"=1",
		// The recursion fuse. Every tool re-enters the tree as an ordinary
		// ccdad command and three of the reads are on the auto-start
		// allow-list, so without it a tools/call could detach a real daemon
		// pinned to a t.TempDir() the framework is about to delete. isolate's
		// CLAUDE_SECURESTORAGE_CONFIG_DIR already stops that by rule 3; this
		// is the half that does not depend on a helper keeping that property.
		daemon.ChildEnvVar+"=1",
	)
	child.Stderr = errFile

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ccdad-test-client", Version: "0.0.0-test"}, nil).
		Connect(t.Context(), &mcp.CommandTransport{Command: child}, nil)
	if err != nil {
		_ = errFile.Close()
		t.Fatalf("connecting to a real `ccdad mcp`: %v\nthe server said:\n%s", err, logs())
	}
	t.Cleanup(func() {
		// Closing the session closes the child's stdin, which is how a stdio
		// server is asked to stop; the transport waits for it before giving
		// up on it.
		_ = cs.Close()
		_ = errFile.Close()
		if t.Failed() {
			t.Logf("the server's stderr:\n%s", logs())
		}
	})
	return cs
}

// resultText is the text content of a tool result, which is where the envelope
// travels.
func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
