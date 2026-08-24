package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// entrypointStub puts a `ccdad` on PATH that appends its whole argv to a log and
// exits with the code the caller chose for that argv. It is the only way to
// exercise this script: a real ccdad would detach a real daemon, and everything
// this file is about — the order, the exec, which codes are survivable — is
// visible from the log and the exit status alone.
//
// It is not smoke_version_test.go's fakeCcdad, which answers `--version` from a
// canned file and takes no argv map.
func entrypointStub(t *testing.T, exits map[string]int) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "argv.log")
	body := "#!/bin/sh\n" +
		"echo \"$*\" >>" + strconv.Quote(log) + "\n" +
		"case \"$*\" in\n"
	for argv, code := range exits {
		body += strconv.Quote(argv) + ") exit " + strconv.Itoa(code) + " ;;\n"
	}
	body += "*) exit 0 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "ccdad"), []byte(body), 0o755); err != nil {
		t.Fatalf("writing the ccdad stand-in: %v", err)
	}
	return dir, log
}

// argvLog is what the stand-in recorded, or "" when it was never run.
func argvLog(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading the argv log: %v", err)
	}
	return string(raw)
}

// runEntrypoint runs the committed script with the stand-in ahead of the real
// PATH, and hands back everything it said plus its exit code.
func runEntrypoint(t *testing.T, stubDir string, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ccdad-entrypoint is /bin/sh, for a Linux image; there is nothing here Windows runs")
	}
	script, err := filepath.Abs(filepath.Join("..", "ccdad-entrypoint"))
	if err != nil {
		t.Fatalf("resolving ccdad-entrypoint: %v", err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running ccdad-entrypoint: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

// The order is the whole of this script. An engine started before the accounts
// exist ranks an empty store and does nothing for a cadence; a command started
// before the engine gets whatever login the image happened to be built with.
func TestEntrypointBootstrapsThenStartsTheDaemonThenExecsTheCommand(t *testing.T) {
	dir, log := entrypointStub(t, nil)

	out, code := runEntrypoint(t, dir, "echo", "hello")

	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("the container's own command never ran:\n%s", out)
	}
	if got := argvLog(t, log); got != "bootstrap\ndaemon start\n" {
		t.Fatalf("the entrypoint ran:\n%swant bootstrap, then daemon start, in that order", got)
	}
}

// `ccdad daemon start` answers 3 when one is already running, which is the
// ordinary state whenever /data is shared with a second container. Under `set -e`
// a bare call makes that container refuse to run its own command over a state
// that is not a problem.
func TestEntrypointStillRunsTheCommandWhenADaemonIsAlreadyRunning(t *testing.T) {
	dir, _ := entrypointStub(t, map[string]int{"daemon start": 3})

	out, code := runEntrypoint(t, dir, "echo", "hello")

	if code != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("exit = %d and output %q: exit 3 from `daemon start` stopped the container "+
			"from running the command it was started for", code, out)
	}
}

// The other half, and it pins the SPELLING as much as the policy. 1 is "the
// daemon could not start" and 4 is "another store's engine is already driving
// this login" — a container that carries on from either is a container with no
// auto-switching, which is the whole reason it exists. The code has to arrive
// intact: `cmd || [ "$?" -eq 3 ]` under `set -e` reports the failing test's 1
// for every value, so a 4 would reach a process manager as a 1.
func TestEntrypointStopsWithTheDaemonsOwnCodeWhenItCannotStart(t *testing.T) {
	for _, want := range []int{1, 4} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			dir, _ := entrypointStub(t, map[string]int{"daemon start": want})

			out, code := runEntrypoint(t, dir, "echo", "hello")

			if code != want {
				t.Fatalf("exit = %d, want %d:\n%s", code, want, out)
			}
			if strings.Contains(out, "hello") {
				t.Fatalf("the container ran its command with no engine behind it:\n%s", out)
			}
		})
	}
}

// A document that is not a usable export is exit 2 from bootstrap, and a
// container that starts anyway has no accounts: every switch it is ever asked
// for answers 4, forever. Failing at boot is the visible form of the same state.
func TestEntrypointStopsWhenBootstrapRefusesTheDocument(t *testing.T) {
	dir, log := entrypointStub(t, map[string]int{"bootstrap": 2})

	out, code := runEntrypoint(t, dir, "echo", "hello")

	if code != 2 {
		t.Fatalf("exit = %d, want 2:\n%s", code, out)
	}
	if strings.Contains(out, "hello") {
		t.Fatalf("the container ran its command with an empty store:\n%s", out)
	}
	if got := argvLog(t, log); got != "bootstrap\n" {
		t.Fatalf("the entrypoint ran:\n%swant it to stop at the failed bootstrap", got)
	}
}

// The container's status is the command's, so a process manager can act on it.
//
// This does NOT pin the exec, and it is worth saying which of the two it is:
// a shell exits with the status of its last command, so a script that ran the
// command as a CHILD and fell off the end would pass this too. What it catches
// is a status that got swallowed on the way out — an `|| true`, or anything
// running after the command. TestEntrypointReplacesItselfWithTheCommand is the
// one that holds the exec.
func TestEntrypointExitsWithTheCommandsStatus(t *testing.T) {
	dir, _ := entrypointStub(t, nil)

	if _, code := runEntrypoint(t, dir, "sh", "-c", "exit 7"); code != 7 {
		t.Fatalf("exit = %d, want 7: the command's status is not the script's", code)
	}
}

// `exec`, and the property is the PROCESS rather than the status. It matters
// because the entrypoint is pid 1: `docker stop` sends SIGTERM there, and a
// shell sitting in front of the command is a pid 1 that does not forward it —
// so the command never learns it is being stopped and the container is killed
// ten seconds later instead. There is also one process in the namespace rather
// than two, which is what the image's own comment claims.
//
// The observable is parentage, because the exit status cannot tell the two
// apart. With exec the command REPLACES the shell this test started, so its
// parent is this test process; without it, its parent is that shell.
func TestEntrypointReplacesItselfWithTheCommand(t *testing.T) {
	dir, _ := entrypointStub(t, nil)

	out, code := runEntrypoint(t, dir, "sh", "-c", "echo $PPID")

	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if got := strings.TrimSpace(out); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the command's parent is %s, want this test process (%d): a shell is still "+
			"sitting between the container and the command it was started for", got, os.Getpid())
	}
}

// dockerfile is the committed image definition, read as text. No `docker build`
// runs here and none should: no CI job has a container runtime, so a test that
// shelled out to docker would skip everywhere and report green. What is left is
// the properties that fail when the container is RUN rather than when the image
// is BUILT, which is the wrong end of the process to find them at. Everything
// else — a missing COPY source, a bad FROM — fails loudly at build and needs
// nothing here.
func dockerfile(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatalf("resolving the Dockerfile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the Dockerfile: %v", err)
	}
	return string(raw)
}

// CCDAD_HOME and CLAUDE_CONFIG_DIR are independent axes: the first moves ccdad's
// own store and does NOT move the Claude Code login ccdad manages. An image that
// set only one would put the store on the volume and leave the login inside the
// layer, so a restart would come up with a store full of accounts and a login
// none of them wrote — and two containers over one volume would run two engines
// against one login and undo each other's switches.
func TestTheImageSetsBothPathAxesUnderTheVolume(t *testing.T) {
	body := dockerfile(t)
	for _, name := range []string{"CCDAD_HOME", "CLAUDE_CONFIG_DIR"} {
		if !strings.Contains(body, name+"=/data/") {
			t.Errorf("the Dockerfile does not point %s at a path under /data:\n%s", name, body)
		}
	}
	if !strings.Contains(body, "VOLUME /data") {
		t.Errorf("the Dockerfile declares no volume, so every restart loses the store, "+
			"the usage cache and the anti-flap state:\n%s", body)
	}
}

// COPY's destination and ENTRYPOINT's path are two independent spellings of one
// file. A drift between them builds cleanly and fails when someone runs the
// image, with an exec-format or not-found error that names neither line.
func TestTheEntrypointTheImageDeclaresIsTheOneItCopies(t *testing.T) {
	body := dockerfile(t)
	if !strings.Contains(body, "COPY ccdad-entrypoint /usr/local/bin/ccdad-entrypoint") {
		t.Errorf("the Dockerfile does not copy ccdad-entrypoint to /usr/local/bin:\n%s", body)
	}
	if !strings.Contains(body, `ENTRYPOINT ["/usr/local/bin/ccdad-entrypoint"]`) {
		t.Errorf("the Dockerfile's ENTRYPOINT is not the file it copies:\n%s", body)
	}
}

// The third property of the same kind, and the reason it is here rather than
// left to the Dockerfile's own comment: a CMD beside an ENTRYPOINT reads as
// redundant, and deleting it fails in the quietest way this image can fail.
// `exec "$@"` with no arguments is a no-op, so `docker run <image>` would start
// the daemon, fall off the end of the entrypoint and exit 0 — taking the daemon
// with it and reporting success for a container that did nothing.
func TestTheImageHasADefaultCommandToExec(t *testing.T) {
	body := dockerfile(t)
	if !strings.Contains(body, "\nCMD [") {
		t.Errorf("the Dockerfile declares no CMD, so a container started with no command "+
			"exits 0 the moment the entrypoint's `exec \"$@\"` finds nothing to exec:\n%s", body)
	}
}
