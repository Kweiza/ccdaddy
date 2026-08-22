package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envValue(t *testing.T, env []string, key string) (string, int) {
	t.Helper()
	value, count := "", 0
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == key {
			value, count = v, count+1
		}
	}
	return value, count
}

// ccpath resolves CCDAD_HOME at CALL TIME and Spawn sets the child's working
// directory to the root of the volume — so a store spelled relatively resolves
// against a different directory in the child than in the parent, and the daemon
// flocks one file while the CLI probes another. storeRoot already refuses a
// relative store; this is where that refusal reaches the child.
func TestChildEnvPinsTheStoreToAnAbsolutePath(t *testing.T) {
	dir := isolate(t)

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	got, count := envValue(t, env, "CCDAD_HOME")
	if count != 1 {
		t.Fatalf("CCDAD_HOME appears %d times in the child environment, want exactly 1", count)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("CCDAD_HOME = %q, want an absolute path", got)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Errorf("CCDAD_HOME = %q, want the resolved %q", got, resolved)
	}
}

// Two spellings of one directory must not look like two stores to the daemon.
// flock is per-inode, so the LOCK already coincides — what does not is every
// path the daemon prints and every path it derives after leaving its birth
// directory, which is why the resolution happens once, here, rather than being
// repeated at each use.
func TestChildEnvResolvesASymlinkedStore(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this platform cannot make a symlink: %v", err)
	}
	t.Setenv("CCDAD_HOME", link)

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := envValue(t, env, "CCDAD_HOME")
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Errorf("CCDAD_HOME = %q, want the symlink resolved to %q", got, resolved)
	}
}

// A relative CLAUDE_CONFIG_DIR is the same hazard one layer over: the daemon
// would manage a config home under the volume root that does not exist, and
// nothing would say so.
func TestChildEnvMakesARelativeClaudeConfigDirAbsolute(t *testing.T) {
	isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "relative-claude")

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	got, count := envValue(t, env, "CLAUDE_CONFIG_DIR")
	if count != 1 {
		t.Fatalf("CLAUDE_CONFIG_DIR appears %d times, want exactly 1", count)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want an absolute path", got)
	}
}

// Claude Code tests whether CLAUDE_SECURESTORAGE_CONFIG_DIR is DEFINED, not
// whether it is non-empty, and defined-but-empty means ~/.claude rather than
// the config home. Absolutising "" would turn that into the working directory
// and silently move the daemon's idea of where credentials live.
func TestChildEnvLeavesADefinedButEmptyCredentialHomeAlone(t *testing.T) {
	isolate(t)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	got, count := envValue(t, env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if count != 1 || got != "" {
		t.Errorf("CLAUDE_SECURESTORAGE_CONFIG_DIR = %q x%d, want one empty entry carried through unchanged", got, count)
	}
}

// The sentinel is half of the recursion guard, and the half that survives the
// child running something other than the hidden entrypoint.
func TestChildEnvMarksTheChild(t *testing.T) {
	isolate(t)

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got, count := envValue(t, env, ChildEnvVar); count != 1 || got == "" {
		t.Errorf("%s = %q x%d, want exactly one non-empty entry", ChildEnvVar, got, count)
	}
}

// Everything else is inherited. PATH, HOME and the terminal's own variables are
// what let the daemon find `claude` and resolve a home directory at all.
func TestChildEnvInheritsTheRestOfTheEnvironment(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_TEST_UNRELATED", "carried")

	env, err := ChildEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(t, env, "CCDAD_TEST_UNRELATED"); got != "carried" {
		t.Errorf("an unrelated variable came through as %q, want %q", got, "carried")
	}
}

// A relative store is refused rather than pinned to whatever the parent's
// working directory happened to be: the parent's cwd is not the store the user
// meant, and guessing produces a credentials tree in a different directory on
// every run.
func TestChildEnvRefusesARelativeStore(t *testing.T) {
	t.Setenv("CCDAD_HOME", "relative-store")
	if _, err := ChildEnv(); err == nil {
		t.Fatal("ChildEnv() = nil error for a relative CCDAD_HOME, want a refusal")
	}
}
