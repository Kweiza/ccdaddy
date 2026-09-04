package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// npmShimDir is the directory a fixture pretends to sit in. Spelled as a Windows
// path on purpose, including on Linux: %dp0% expands to a Windows path in the
// only place this code ever runs, and a fixture written in the host's spelling
// would test the one arrangement that never happens.
const npmShimDir = `C:\Users\dev\AppData\Roaming\npm`

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cmdshim", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The fixtures are real generator output, so this is also the assertion that
// they still ARE: a checkout that converted their CRLF would be a checkout
// testing a file npm never writes.
func TestTheShimFixturesKeepTheLineEndingsNpmWrites(t *testing.T) {
	for _, name := range []string{"env-node.cmd", "env-dash-s-flags.cmd", "env-with-vars.cmd", "absolute-node.cmd", "no-shebang.cmd"} {
		if got := readFixture(t, name); !strings.Contains(got, "\r\n") {
			t.Errorf("%s has no CRLF; .gitattributes is converting a fixture whose line endings are the point", name)
		}
	}
}

func TestParseNpmShim(t *testing.T) {
	script := npmShimDir + `\` + `\node_modules\@anthropic-ai\claude-code\cli.js`
	for _, tc := range []struct {
		name     string
		fixture  string
		prog     string
		fallback string
		args     []string
		env      []string
	}{
		{
			name:     "the ordinary node package",
			fixture:  "env-node.cmd",
			prog:     npmShimDir + `\` + `\node.exe`,
			fallback: "node",
			args:     []string{script},
		},
		{
			// The flags belong to the interpreter and have to arrive BEFORE
			// the script; appending them would hand them to claude instead.
			name:     "interpreter flags from a -S shebang",
			fixture:  "env-dash-s-flags.cmd",
			prog:     npmShimDir + `\` + `\node.exe`,
			fallback: "node",
			args:     []string{"--experimental-vm-modules", script},
		},
		{
			name:     "variables the shebang asked for",
			fixture:  "env-with-vars.cmd",
			prog:     npmShimDir + `\` + `\node.exe`,
			fallback: "node",
			args:     []string{script},
			env:      []string{"FOO=bar"},
		},
		{
			// The generator emits a POSIX interpreter path verbatim. Parsing
			// it is right; what to do about it is resolvePastShim's problem.
			name:     "an absolute POSIX interpreter, emitted verbatim",
			fixture:  "absolute-node.cmd",
			prog:     npmShimDir + `\` + `\/usr/bin/node.exe`,
			fallback: "/usr/bin/node",
			args:     []string{script},
		},
		{
			// No shebang: the shim has no interpreter and calls the target.
			name:    "a target with no shebang",
			fixture: "no-shebang.cmd",
			prog:    script,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim, ok := parseNpmShim(readFixture(t, tc.fixture), npmShimDir)
			if !ok {
				t.Fatalf("%s did not parse", tc.fixture)
			}
			if shim.prog != tc.prog {
				t.Errorf("prog = %q, want %q", shim.prog, tc.prog)
			}
			if shim.fallback != tc.fallback {
				t.Errorf("fallback = %q, want %q", shim.fallback, tc.fallback)
			}
			if !slices.Equal(shim.args, tc.args) {
				t.Errorf("args = %q, want %q", shim.args, tc.args)
			}
			if !slices.Equal(shim.env, tc.env) {
				t.Errorf("env = %q, want %q", shim.env, tc.env)
			}
		})
	}
}

// Everything this does not fully understand has to come back false, because
// the caller's fallback is the refusal that shipped before any of this — so a
// misparse costs a refused argument rather than a wrong program.
func TestParseNpmShimRefusesWhatItDoesNotRecognise(t *testing.T) {
	good := readFixture(t, "env-node.cmd")
	for _, tc := range []struct {
		name string
		text string
	}{
		{"an empty file", ""},
		{"a hand-written batch file", "@echo off\r\nnode C:\\tools\\claude.js %*\r\n"},
		{"the preamble alone, with nothing to run", "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n"},
		{
			// A variable this does not model is the case where guessing runs
			// the wrong program, so it is refused rather than passed through.
			name: "a variable ccdad does not expand",
			// Replaced in the SET line, not the IF EXIST above it: the SET is
			// what `"%_prog%"` on the last line expands to, so it is the one
			// the parse reads. Getting this wrong the first time is why the
			// distinction is written down.
			text: strings.Replace(good, `SET "_prog=%dp0%\node.exe"`, `SET "_prog=%NODE_HOME%\node.exe"`, 1),
		},
		{
			name: "a quote in the middle of a token",
			text: strings.Replace(good, `"%dp0%\node_modules`, `"%dp0%"\node_modules`, 1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if shim, ok := parseNpmShim(tc.text, npmShimDir); ok {
				t.Fatalf("parsed as %+v; it should have been refused", shim)
			}
		})
	}
}

// A shim written out with LF endings — a checkout with autocrlf off, an editor
// that rewrote it — is still that shim.
func TestParseNpmShimAcceptsAShimWhoseLineEndingsWereConverted(t *testing.T) {
	lf := strings.ReplaceAll(readFixture(t, "env-node.cmd"), "\r\n", "\n")
	shim, ok := parseNpmShim(lf, npmShimDir)
	if !ok {
		t.Fatal("an LF-converted shim did not parse")
	}
	if shim.fallback != "node" {
		t.Fatalf("fallback = %q, want node", shim.fallback)
	}
}

// A shim carrying lines ccdad does not model is ACCEPTED, and they are
// dropped rather than refused. Pinned here because the widening made it matter
// and because, left unwritten, it reads as an oversight the next reader closes.
//
// Refusing instead is the tempting move now that every .cmd is resolved past.
// It is the wrong one: nothing here can tell a hand-edit from a future npm
// template, so a strict rule would route every user of a template ccdad has
// not seen back through cmd.exe, silently, with no signal that the improvement
// stopped applying — and a false rejection is invisible in a way a false
// acceptance is not.
//
// What makes the leniency safe for the shape that actually occurs is not in Go
// and cannot be asserted from here: the generated shim runs `endLocal` in the
// same &-chain and BEFORE `"%_prog%"`, so a variable one of these lines set is
// discarded before the child starts either way — with or without ccdad. The
// residue is a hand-edit that survives npm regenerating the file AND does
// something other than set a variable, such as the CALL below. It is knowingly
// left, and it is the reason this test asserts the seam rather than blessing
// the parser as complete.
func TestParseNpmShimAcceptsAShimCarryingLinesItDoesNotModel(t *testing.T) {
	extra := strings.Replace(readFixture(t, "env-node.cmd"), "CALL :find_dp0\r\n",
		"CALL :find_dp0\r\nSET ANTHROPIC_BASE_URL=https://proxy.example\r\nCALL \"%dp0%\\preflight.cmd\"\r\n", 1)
	if extra == readFixture(t, "env-node.cmd") {
		t.Fatal("the insertion point moved; this test is measuring the unmodified fixture")
	}

	shim, ok := parseNpmShim(extra, npmShimDir)
	if !ok {
		t.Fatal("a shim with two unmodelled lines was refused — the seam this pins has been closed; read the comment above before deleting this test")
	}
	if len(shim.env) != 0 {
		t.Errorf("env = %q, want the unmodelled SET dropped rather than carried into the child", shim.env)
	}
	if shim.fallback != "node" {
		t.Errorf("fallback = %q, want the interpreter still read correctly around the extra lines", shim.fallback)
	}
}

func TestResolvePastShim(t *testing.T) {
	beside := npmShimDir + `\` + `\node.exe`
	shim := npmShim{prog: beside, fallback: "node", args: []string{"cli.js"}, env: []string{"FOO=bar"}}

	t.Run("prefers the node beside the shim, as the shim does", func(t *testing.T) {
		past, err := resolvePastShim(shim,
			func(p string) bool { return p == beside },
			func(string) (string, error) {
				t.Error("looked on PATH with a node.exe sitting beside the shim")
				return "", nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if past.path != beside {
			t.Errorf("path = %q, want %q", past.path, beside)
		}
		if !slices.Equal(past.args, []string{"cli.js"}) || !slices.Equal(past.env, []string{"FOO=bar"}) {
			t.Errorf("args/env = %q / %q, want the shim's", past.args, past.env)
		}
	})

	t.Run("falls back to PATH when there is none", func(t *testing.T) {
		past, err := resolvePastShim(shim,
			func(string) bool { return false },
			func(name string) (string, error) {
				if name != "node" {
					t.Errorf("looked up %q, want the interpreter the shim names", name)
				}
				return `C:\Program Files\nodejs\node.exe`, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if past.path != `C:\Program Files\nodejs\node.exe` {
			t.Errorf("path = %q", past.path)
		}
	})

	// PATHEXT is searched per PATH directory, so a .cmd in an earlier one
	// beats a .exe in a later one. Following the shim to another shim puts
	// cmd.exe straight back in, with one more layer of quoting.
	t.Run("declines an interpreter that is itself a shim", func(t *testing.T) {
		for _, ext := range []string{".cmd", ".bat", ".ps1", ".js", ".CMD"} {
			_, err := resolvePastShim(shim,
				func(string) bool { return false },
				func(string) (string, error) { return `C:\tools\node` + ext, nil })
			if err == nil {
				t.Errorf("followed a shim to a %s and called it resolved", ext)
			}
		}
	})

	t.Run("declines when PATH has no such interpreter", func(t *testing.T) {
		if _, err := resolvePastShim(shim,
			func(string) bool { return false },
			func(string) (string, error) { return "", errors.New("not found") }); err == nil {
			t.Error("resolved an interpreter that is not on PATH")
		}
	})

	// The no-shebang shape leans on cmd.exe's own file association, which
	// ccdad cannot reproduce: a .js there goes to the Windows Script Host.
	t.Run("declines a shim with no interpreter at all", func(t *testing.T) {
		none := npmShim{prog: npmShimDir + `\cli.js`}
		if _, err := resolvePastShim(none,
			func(string) bool { return true },
			func(string) (string, error) {
				t.Error("looked up an interpreter for a shim that names none")
				return "", nil
			}); err == nil {
			t.Error("resolved a shim that has no interpreter")
		}
	})
}

// filepath picks its separator at build time, so these two exist rather than
// filepath.Dir and filepath.Base. Asserted in BOTH spellings, because the code
// is written on one platform and runs on the other.
func TestShimPathHelpersReadBothSpellings(t *testing.T) {
	for _, tc := range []struct{ path, dir, base, ext string }{
		{`C:\Users\dev\npm\claude.cmd`, `C:\Users\dev\npm`, "claude.cmd", ".cmd"},
		{"/usr/local/bin/claude", "/usr/local/bin", "claude", ""},
		{"claude.cmd", ".", "claude.cmd", ".cmd"},
		{`C:\dir.with.dots\claude`, `C:\dir.with.dots`, "claude", ""},
	} {
		if got := shimDirOf(tc.path); got != tc.dir {
			t.Errorf("shimDirOf(%q) = %q, want %q", tc.path, got, tc.dir)
		}
		if got := shimBaseOf(tc.path); got != tc.base {
			t.Errorf("shimBaseOf(%q) = %q, want %q", tc.path, got, tc.base)
		}
		if got := extOf(tc.path); got != tc.ext {
			t.Errorf("extOf(%q) = %q, want %q", tc.path, got, tc.ext)
		}
	}
}

func TestReadShimRefusesAFileTooLargeToBeOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.cmd")
	if err := os.WriteFile(path, make([]byte, maxShimSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readShim(path); err == nil {
		t.Fatal("read a file far too large to be a shim")
	}
}

// installShim puts a real generated shim on disk as `claude.cmd` and points
// lookClaude at it, which is the machine this whole file exists for: one where
// `claude` on PATH is npm's batch file rather than an executable.
//
// It must be called AFTER stubClaude, which stubs the resolver itself so that
// no test depends on the developer having Claude Code installed. Calling them
// the other way round silently launches stubClaude's placeholder instead, and
// the assertion that catches it reads like a parser bug.
func installShim(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(path, []byte(readFixture(t, fixture)), 0o700); err != nil {
		t.Fatal(err)
	}
	stubLookClaude(t, path)
	return path
}

// stubLookProgram describes what the interpreter the shim names resolves to.
func stubLookProgram(t *testing.T, path string, err error) {
	t.Helper()
	saved := lookProgram
	t.Cleanup(func() { lookProgram = saved })
	lookProgram = func(string) (string, error) { return path, err }
}

// stubFileExists describes whether the node.exe npm sometimes puts beside the
// shim is there.
func stubFileExists(t *testing.T, present bool) {
	t.Helper()
	saved := fileExists
	t.Cleanup(func() { fileExists = saved })
	fileExists = func(string) bool { return present }
}

// The item's own acceptance case, as far as a non-Windows host can take it:
// `ccdad run acct -p 'fix&whoami'` used to be a usage error, because cmd.exe
// would have read the ampersand as a command separator. Going straight to the
// interpreter removes cmd.exe from the launch, and Go's escaping is then
// exactly right rather than approximately wrong.
func TestRunResolvesPastAShimRatherThanRefusingTheArgument(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	installShim(t, "env-node.cmd")
	stubFileExists(t, false)
	stubLookProgram(t, `C:\Program Files\nodejs\node.exe`, nil)

	code, _, errOut, top := runRoot(t, "run", "1", "-p", "fix&whoami")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if stub.spec.Path != `C:\Program Files\nodejs\node.exe` {
		t.Fatalf("launched %q, want the interpreter the shim names", stub.spec.Path)
	}
	if len(stub.spec.Args) != 3 {
		t.Fatalf("args = %q, want the script followed by the two the caller typed", stub.spec.Args)
	}
	if !strings.HasSuffix(stub.spec.Args[0], "cli.js") {
		t.Errorf("args[0] = %q, want the script the shim runs", stub.spec.Args[0])
	}
	// Verbatim, ampersand included. This is the whole point.
	if got := stub.spec.Args[1:]; !slices.Equal(got, []string{"-p", "fix&whoami"}) {
		t.Errorf("args = %q, want the caller's arguments untouched", got)
	}
	if !strings.Contains(errOut, "running node.exe directly") {
		t.Errorf("the user is not told the launch changed shape:\n%s", errOut)
	}
}

// The interpreter's own flags go BEFORE the script. Appending them would hand
// `--experimental-vm-modules` to claude, which would reject it — and the test
// above cannot tell, because that shim has no flags.
func TestRunKeepsTheInterpretersFlagsAheadOfTheScript(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	installShim(t, "env-dash-s-flags.cmd")
	stubFileExists(t, false)
	stubLookProgram(t, `C:\nodejs\node.exe`, nil)

	if code, _, errOut, top := runRoot(t, "run", "1", "-p", "a&b"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	want := []string{"--experimental-vm-modules"}
	if len(stub.spec.Args) < 2 || !slices.Equal(stub.spec.Args[:1], want) {
		t.Fatalf("args = %q, want %q first", stub.spec.Args, want)
	}
	if !strings.HasSuffix(stub.spec.Args[1], "cli.js") {
		t.Errorf("args[1] = %q, want the script after the interpreter's flags", stub.spec.Args[1])
	}
}

// `#!/usr/bin/env FOO=bar node`: the shim exports these before running
// anything, so a launch that goes past the shim has to carry them or the
// package starts in an environment it did not ask for.
func TestRunCarriesTheVariablesTheShimWouldHaveExported(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	installShim(t, "env-with-vars.cmd")
	stubFileExists(t, false)
	stubLookProgram(t, `C:\nodejs\node.exe`, nil)

	if code, _, errOut, top := runRoot(t, "run", "1", "-p", "a&b"); code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got, ok := envOf(stub.spec.Env, "FOO"); !ok || got != "bar" {
		t.Errorf("FOO = %q (set: %v), want bar", got, ok)
	}
}

// The shims ccdad cannot resolve past, shared by the two tests below because
// they are the two halves of one rule: when resolution fails, refuse the
// argument cmd.exe would eat and launch everything else through the shim
// exactly as before. Splitting the shapes across the two would leave each half
// asserted for some of them and neither half asserted for all.
var unresolvableShims = []struct {
	name  string
	setup func(*testing.T)
}{
	{"a .cmd that npm did not write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "claude.cmd")
		if err := os.WriteFile(path, []byte("@echo off\r\nnode c:\\claude.js %*\r\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		stubLookClaude(t, path)
	}},
	{"no shim on disk at all", func(t *testing.T) {
		stubLookClaude(t, filepath.Join(t.TempDir(), "claude.cmd"))
	}},
	{"an interpreter that is not installed", func(t *testing.T) {
		installShim(t, "env-node.cmd")
		stubFileExists(t, false)
		stubLookProgram(t, "", errors.New("executable file not found in %PATH%"))
	}},
	{"a target cmd.exe would run by file association", func(t *testing.T) {
		installShim(t, "no-shebang.cmd")
		stubFileExists(t, true)
	}},
}

// When resolution cannot be done, the refusal that shipped before this is what
// is left — and it has to say both things, or the reader is told the argument
// is at fault when the shim is.
func TestRunFallsBackToTheRefusalWhenTheShimCannotBeResolved(t *testing.T) {
	for _, tc := range unresolvableShims {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")
			stub := stubClaude(t, ExitOK)
			tc.setup(t)

			code, _, errOut, top := runRoot(t, "run", "1", "-p", "fix&whoami")
			if code != ExitUsage {
				t.Fatalf("exit = %d (%s / %s), want %d", code, errOut, top, ExitUsage)
			}
			if got := top + errOut; !strings.Contains(got, "fix&whoami") || !strings.Contains(got, "cmd.exe shim") {
				t.Errorf("the refusal does not name both the argument and the shim: %q", got)
			}
			if stub.started {
				t.Error("started a session through a shim that would have mangled the argument")
			}
		})
	}
}

// The widening, pinned where the narrowing used to be: an argument cmd.exe
// would have handled fine takes the SAME route as one it would have eaten.
// Before this, `summarize this` went through the shim and `fix&whoami` did
// not, which made the launch depend on the text of a prompt.
//
// The quiet stderr is half the assertion. On the rescue path the note explains
// why the process tree looks different from what was typed; here there is
// nothing to explain, and a line printed on every Windows session would be
// noise the reader cannot act on.
func TestRunResolvesPastTheShimForAnOrdinaryArgumentToo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	stub := stubClaude(t, ExitOK)
	path := installShim(t, "env-node.cmd")
	stubFileExists(t, false)
	stubLookProgram(t, `C:\nodejs\node.exe`, nil)

	code, _, errOut, top := runRoot(t, "run", "1", "-p", "summarize this")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if stub.spec.Path == path {
		t.Fatalf("launched the shim %q: an ordinary argument takes the same route as a dangerous one now", path)
	}
	if stub.spec.Path != `C:\nodejs\node.exe` {
		t.Fatalf("launched %q, want the interpreter the shim names", stub.spec.Path)
	}
	if got := stub.spec.Args[1:]; !slices.Equal(got, []string{"-p", "summarize this"}) {
		t.Errorf("args = %q, want the caller's arguments untouched", got)
	}
	if strings.Contains(errOut, "note:") {
		t.Errorf("a note was printed for a launch with nothing to explain:\n%s", errOut)
	}
}

// The other half of the failure rule, and the one that makes the widening safe
// to ship: a shim ccdad cannot read is not a reason to refuse arguments
// cmd.exe would have carried correctly. Every shape in unresolvableShims used
// to reach cmd.exe here and still does.
//
// Without this, widening the resolution would turn a wrapper someone wrote, a
// shim from a future npm, or the no-shebang shape into a usage error for
// invocations that work today — breaking working launches, which is exactly
// what the narrow shape was protecting against.
func TestRunKeepsAnUnresolvableShimWhenTheArgumentsAreSafe(t *testing.T) {
	for _, tc := range unresolvableShims {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")
			stub := stubClaude(t, ExitOK)
			tc.setup(t)

			code, _, errOut, top := runRoot(t, "run", "1", "-p", "summarize this file")
			if code != ExitOK {
				t.Fatalf("exit = %d (%s / %s), want 0: a shim ccdad cannot read still runs", code, errOut, top)
			}
			if !stub.started {
				t.Fatal("nothing was started; an invocation that worked before now does not")
			}
			if ext := strings.ToLower(extOf(stub.spec.Path)); ext != ".cmd" {
				t.Errorf("launched %q, want the shim itself — there was nothing to resolve past to", stub.spec.Path)
			}
			if got := stub.spec.Args; !slices.Equal(got, []string{"-p", "summarize this file"}) {
				t.Errorf("args = %q, want the caller's arguments forwarded to the shim", got)
			}
		})
	}
}
