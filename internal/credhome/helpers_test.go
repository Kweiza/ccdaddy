package credhome

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Environment variables that put a re-executed copy of this test binary into a
// role other than "run the tests".
const (
	roleEnv    = "CCDAD_CREDHOME_TEST_ROLE"
	readyEnv   = "CCDAD_CREDHOME_TEST_READY"
	releaseEnv = "CCDAD_CREDHOME_TEST_RELEASE"

	roleHolder = "claim-holder"
)

// TestMain turns this test binary into its own fixture.
//
// A claim is a kernel fact about an open file description, and the one question
// this package exists to answer — "is somebody ELSE driving this credential
// home" — cannot be posed inside one process. flock(2) denies a process its own
// exclusive lock through a second descriptor exactly as it denies a stranger's,
// so a same-process fixture would be indistinguishable from the state under
// test while proving nothing about it. Every contention test here therefore
// re-execs this binary as a holder.
func TestMain(m *testing.M) {
	if os.Getenv(roleEnv) == roleHolder {
		os.Exit(runAsHolder())
	}
	os.Exit(m.Run())
}

// runAsHolder takes the claim in a process of its own and holds it until told
// to let go.
//
// It reports the failure rather than exiting silently: a holder that could not
// take the claim would otherwise present as a test whose assertions all pass
// because nothing was ever held.
func runAsHolder() int {
	c, err := Acquire()
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 1
	}
	if err := os.WriteFile(os.Getenv(readyEnv), []byte("held\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 1
	}
	release := os.Getenv(releaseEnv)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Release(); err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 1
	}
	// Acknowledged on the way out. A test that only writes the release file has
	// no way to know the child ever saw it, and a holder still holding the claim
	// after the test returns is a holder the NEXT test contends with.
	_ = os.WriteFile(release+".seen", nil, 0o600)
	return 0
}

// isolate points every home this package resolves at a directory of this test's
// own, and refuses to run if it did not work.
//
// The assertion is not ceremony. This package takes an flock and writes a file
// inside Claude Code's credential home, so a helper that quietly stopped
// sandboxing would put both beside a developer's live .credentials.json — and
// the first symptom would be their own daemon refusing to start.
//
// BOTH HOME and USERPROFILE: os.UserHomeDir reads $HOME on Unix and
// %USERPROFILE% on Windows, so setting one sandboxes half the platforms.
func isolate(t *testing.T) (store, claude string) {
	t.Helper()
	store = filepath.Join(t.TempDir(), "ccdad")
	home := t.TempDir()
	claude = filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDAD_HOME", store)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)

	if got, err := Home(); err != nil || got != claude {
		t.Fatalf("isolate: Home() = (%q, %v), want %q — refusing to run unsandboxed", got, err, claude)
	}
	return store, claude
}

// holdFromAnotherProcess starts a second process that takes the claim against
// the environment described by env, and returns once it actually holds it.
//
// env is applied ON TOP of this process's, so a caller that wants a DIFFERENT
// store passes only CCDAD_HOME and inherits the credential home under test —
// which is the shape every contention test needs.
func holdFromAnotherProcess(t *testing.T, env ...string) {
	t.Helper()
	// A directory of its own, and created BEFORE the child so its cleanup is
	// registered first and therefore runs LAST. A signal file inside a
	// directory the framework removes ahead of the child's poll is how a
	// passing run leaves a detached process holding a lock for another minute.
	signals := t.TempDir()
	ready := filepath.Join(signals, "ready")
	release := filepath.Join(signals, "release")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), roleEnv+"="+roleHolder, readyEnv+"="+ready, releaseEnv+"="+release)
	holder.Env = append(holder.Env, env...)
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatalf("starting the holder: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		if err := holder.Wait(); err != nil {
			t.Errorf("the holder exited with %v — it never took the claim, or could not give it back", err)
		}
	})
	waitFor(t, ready, 10*time.Second)
}

func waitFor(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared within %s", path, within)
}

// acquireForTest takes the claim and gives it back when the test ends.
//
// The release is not tidiness: heldClaim is package state for the life of the
// process, so a test that leaked one would make every later Acquire in this
// binary answer ErrAlreadyHeld — and the tests that assert ErrClaimed would
// then pass for having reached a different refusal entirely.
func acquireForTest(t *testing.T) *Claim {
	t.Helper()
	c, err := Acquire()
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Release(); err != nil {
			t.Errorf("Release() = %v", err)
		}
	})
	return c
}

// runHolderExpectingRefusal starts the holder and requires it to FAIL, handing
// back what it said on the way out.
//
// Its output is asserted on rather than only its exit status: a child that died
// for an unrelated reason — a bad fixture, an unresolvable home — exits non-zero
// too, and a test that accepted that would pass while proving nothing about the
// claim.
func runHolderExpectingRefusal(t *testing.T, env ...string) (string, error) {
	t.Helper()
	signals := t.TempDir()
	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(),
		roleEnv+"="+roleHolder,
		readyEnv+"="+filepath.Join(signals, "ready"),
		releaseEnv+"="+filepath.Join(signals, "release"),
	)
	holder.Env = append(holder.Env, env...)
	out, err := holder.CombinedOutput()
	return string(out), err
}
