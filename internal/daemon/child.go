package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChildEnvVar marks a process ccdad started itself. It is half of the recursion
// guard, and it is the half that survives the child running something other
// than the hidden daemon entrypoint: the allow-list refuses to auto-start for
// `__daemon`, and this refuses for anything a daemon's own descendants ever
// run. A missing guard here is not a bug that degrades — the child is itself
// `ccdad <something>`, so it spawns as fast as the operating system allows.
const ChildEnvVar = "CCDAD_DAEMON_CHILD"

// ChildEnv is the environment a detached daemon must be started with.
//
// The environment is INHERITED — PATH, HOME and the rest are what let the
// daemon find `claude` and resolve a home directory at all — with the three
// variables ccpath resolves AT CALL TIME pinned to what they resolved to here,
// and the marker above added.
//
// Pinning is not tidiness. Spawn sets the child's working directory to the root
// of the volume the binary lives on, so a relative CCDAD_HOME or
// CLAUDE_CONFIG_DIR resolves against a DIFFERENT directory in the child than in
// the parent: the daemon flocks one file while the CLI probes another, and each
// invocation from each directory sees "no daemon" and starts one more.
//
// Symlinks are resolved for the same reason and to a smaller end. flock is
// per-inode, so two spellings of one store already contend for the same lock;
// what they do not share is the path the daemon prints, the path it derives
// after leaving its birth directory, and the store it reports in a status
// document. Resolving once, here, is what keeps those from depending on how a
// shell happened to spell it.
//
// What this does NOT do is decide whether a daemon should be started at all.
// A credential environment scoped to one terminal — which is what
// CLAUDE_SECURESTORAGE_CONFIG_DIR exists for — would make a daemon manage that
// terminal's credentials for the rest of its life, and pinning the resolved
// path only makes that permanent rather than preventing it. Refusing is the
// answer, and it belongs to the auto-start policy, which is the only caller
// that starts a daemon nobody asked for.
func ChildEnv() ([]string, error) {
	// storeRoot refuses a relative store outright rather than pinning it to
	// whatever the parent's working directory happened to be: that directory is
	// not the store the user meant, and guessing puts a credentials tree
	// somewhere new on every run.
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}

	env := os.Environ()
	env = setEnv(env, "CCDAD_HOME", resolvePath(root))
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		// Only a NON-EMPTY value is made absolute. Claude Code tests whether
		// CLAUDE_SECURESTORAGE_CONFIG_DIR is defined rather than whether it is
		// set, and defined-but-empty means ~/.claude — not the config home, and
		// certainly not the working directory that absolutising "" would
		// produce. An empty definition is carried through untouched.
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			return nil, fmt.Errorf("resolving %s=%q for the daemon: %w", name, v, err)
		}
		env = setEnv(env, name, resolvePath(abs))
	}
	return setEnv(env, ChildEnvVar, "1"), nil
}

// resolvePath resolves symlinks when it can, and hands back what it was given
// when it cannot.
//
// EvalSymlinks fails on a path that does not exist, which is the ordinary state
// of a store on a machine where no ccdad command has run yet — so a failure
// here is not evidence of anything and must not stop a daemon starting.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// setEnv replaces every entry for key and appends one if there was none.
//
// Every entry, not the first: a duplicate is a landmine even though exec takes
// the last one, because the next reader of this list will not know that.
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if k, _, ok := strings.Cut(entry, "="); ok && k == key {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}
