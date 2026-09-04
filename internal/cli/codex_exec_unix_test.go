//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathWith builds a PATH out of temp directories and puts an executable file
// named `name` in each directory whose entry in `holds` is true. It returns
// the directories in PATH order.
//
// Real files with a real executable bit, because that is the whole question
// realCodexPath asks: a directory holding a NON-executable codex is a
// directory with no codex in it, and a stubbed lookup could never say so.
func pathWith(t *testing.T, name string, holds ...bool) []string {
	t.Helper()
	var dirs []string
	for _, has := range holds {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		if !has {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join(dirs, ":"))
	return dirs
}

// The shim directory is skipped, and it is the only reason this is a hand
// walk rather than exec.LookPath. LookPath would find <CCDAD_HOME>/bin/codex,
// which execs `ccdad codex exec`, which would resolve codex again: an
// unbounded loop with a process per turn of it, and the symptom is a machine
// that fills with processes when a user types `codex`.
func TestRealCodexPathSkipsTheShimDirectory(t *testing.T) {
	isolate(t)
	dirs := pathWith(t, "codex", true, true)
	got, err := realCodexPath(dirs[0])
	if err != nil {
		t.Fatalf("realCodexPath = %v, want the codex in the second directory", err)
	}
	if want := filepath.Join(dirs[1], "codex"); got != want {
		t.Errorf("realCodexPath = %q, want %q: the first directory is the shim's and must be skipped", got, want)
	}
}

// Ordered, not "any hit": the first codex on PATH is the one a bare `codex`
// would have run without ccdad, and running a different one would change which
// codex the user gets as a side effect of installing a shim.
func TestRealCodexPathTakesTheFirstCodexOnPath(t *testing.T) {
	isolate(t)
	dirs := pathWith(t, "codex", false, true, true)
	got, err := realCodexPath(filepath.Join(t.TempDir(), "shim"))
	if err != nil {
		t.Fatalf("realCodexPath = %v, want the codex in the second directory", err)
	}
	if want := filepath.Join(dirs[1], "codex"); got != want {
		t.Errorf("realCodexPath = %q, want %q", got, want)
	}
}

// A file that is not executable is not a codex. Answering with it would hand
// the launcher a path whose only symptom is a permission error out of exec,
// with nothing naming the file.
func TestRealCodexPathIgnoresANonExecutableFile(t *testing.T) {
	isolate(t)
	dirs := pathWith(t, "codex", false, true)
	if err := os.Chmod(filepath.Join(dirs[1], "codex"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := realCodexPath(""); err == nil {
		t.Errorf("realCodexPath = %q, want an error: the only codex on PATH is not executable", got)
	}
}

// A RELATIVE PATH component is skipped rather than joined. filepath.Join(".",
// "codex") is "codex" -- no separator -- and exec.LookPath given a bare name
// searches the whole PATH, which puts the shim right back in the answer.
func TestRealCodexPathSkipsARelativePathComponent(t *testing.T) {
	isolate(t)
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "."+string(os.PathListSeparator)+shim)
	if got, err := realCodexPath(shim); err == nil {
		t.Errorf("realCodexPath = %q, want an error: a relative component must not resolve through the whole PATH", got)
	}
}

func TestRealCodexPathSaysWhatToDoWhenThereIsNoCodex(t *testing.T) {
	isolate(t)
	pathWith(t, "codex", false, false)
	_, err := realCodexPath("")
	if err == nil {
		t.Fatal("realCodexPath found a codex on a PATH with none on it")
	}
	if !strings.Contains(err.Error(), "codex.binary") {
		t.Errorf("the error does not name the way out: %v", err)
	}
}
