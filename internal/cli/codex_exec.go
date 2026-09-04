package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/config"
)

// codexProgramName is what PATH is searched for. It carries no extension:
// exec.LookPath adds the ones in PATHEXT on Windows, which is where a codex is
// a .cmd or a .exe, and spelling one here would be a Windows fact written on a
// Linux machine.
const codexProgramName = "codex"

// errNoCodex is a machine with a shim and nothing behind it.
//
// It names the way out, because there are two and neither is guessable: codex
// may simply not be installed, or it may be installed somewhere that is not on
// this shell's PATH -- an editor's bundled copy, a version manager's shim
// directory -- in which case naming it is the answer rather than changing PATH.
var errNoCodex = errors.New("ccdad cannot find a codex on PATH outside its own shim directory. " +
	"Install codex, or name the one to use with `ccdad config set codex.binary <path>`")

// realCodexPath is the codex the shim stands in front of: the first PATH
// component outside shimDir that holds an executable codex.
//
// A hand walk rather than exec.LookPath, and the shim is the whole reason.
// LookPath would find <CCDAD_HOME>/bin/codex, which execs `ccdad codex exec`,
// which would resolve codex again -- an unbounded loop with a process per turn
// of it. Skipping one component is the only difference from LookPath, and it
// cannot be expressed to LookPath.
//
// livePathRules and never path/filepath for the SPLIT: a PATH list is
// ':'-separated here and ';'-separated on Windows, and os.PathListSeparator
// answers ':' for both under `go test` on Linux -- so a Windows bug written
// with it passes on this machine and ships. The JOIN below is filepath's,
// because that half is a real path on the platform this runs on.
//
// A RELATIVE component is skipped rather than joined, and that is not
// tidiness: filepath.Join(".", "codex") is "codex", with no separator in it,
// and exec.LookPath given a bare name searches the whole PATH -- which puts
// the shim straight back into the answer this function exists to keep it out
// of.
func realCodexPath(shimDir string) (string, error) {
	for _, dir := range livePathRules.split(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if shimDir != "" && livePathRules.same(dir, shimDir) {
			continue
		}
		path, err := exec.LookPath(filepath.Join(dir, codexProgramName))
		if err != nil {
			continue
		}
		return path, nil
	}
	return "", errNoCodex
}

// resolveCodex is the walk behind a seam, so a test can describe a machine
// whose codex is somewhere the test put it. Production never reassigns it.
var resolveCodex = realCodexPath

// codexBinary is the codex this launch will run.
//
// The configured path WINS rather than being a fallback. It is the escape
// hatch for a machine the walk cannot get right -- a codex that is not on PATH
// at all, or two of them where the first is not the one wanted -- and a
// setting that only applied when the walk failed would be a setting that
// silently did nothing on exactly the machines it was set on.
//
// A configured path that is not there is a usage error rather than a quiet
// fallback to PATH. The user said which codex to run; running a different one
// would bill a session through a binary they did not choose.
func codexBinary() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.Codex.Binary == "" {
		return resolveCodex(shimDir())
	}
	if _, err := os.Stat(cfg.Codex.Binary); err != nil {
		return "", UsageError("codex.binary in %s names %s, which ccdad cannot use (%v). "+
			"Correct it with `ccdad config set codex.binary <path>`, or clear it with "+
			"`ccdad config unset codex.binary` to use the first codex on PATH",
			config.FileName, cfg.Codex.Binary, err)
	}
	return cfg.Codex.Binary, nil
}

// codexKeyEnv carries the per-launch secret to the child. codex reads it
// because the launch declares `env_key = "CCDAD_CODEX_KEY"` for its model
// provider, so the value becomes the bearer on every request codex makes.
//
// It is treated as PUBLIC within the session, which is why the launch also
// excludes it from the environment codex hands to agent commands: the boundary
// here is the uid, a same-uid process can read the launcher's environment
// anyway, and what the secret authorises is exactly one route for one launch --
// never an OAuth token, and nothing on any other ccdad surface.
const codexKeyEnv = "CCDAD_CODEX_KEY"

// loopbackHost is the ONE entry that exempts a loopback base_url from a proxy
// named in the environment.
//
// MEASURED against codex 0.151.0, all three: an HTTP_PROXY or ALL_PROXY in the
// environment captures a request to http://127.0.0.1:<port>; a NO_PROXY of
// `localhost` does not exempt it; and a NO_PROXY of `127.0.0.1:<port>` does not
// exempt it either. Only the bare host does. The symptom of getting this wrong
// is not an error message -- it is codex's own endless "Reconnecting... waiting
// for network", with the request sitting in somebody's corporate proxy log.
const loopbackHost = "127.0.0.1"

// withNoProxyLoopback returns env with NO_PROXY and no_proxy each carrying a
// bare loopback entry.
//
// BOTH spellings, and each keeps its OWN value where it has one and borrows the
// other's where it does not. Go's http.ProxyFromEnvironment reads NO_PROXY and
// falls back to no_proxy; other runtimes read only the lower-case one; and a
// user who set one of them meant it. Merging them into a single value would
// rewrite a variable that is theirs, and setting only one would leave the two
// saying different things about the same machine.
//
// On Windows the environment is case-insensitive and os/exec folds the two into
// one entry, keeping the last. That is harmless here precisely because the two
// values can only differ when both were set, which on Windows cannot happen.
//
// Nothing is ever REMOVED. HTTP_PROXY, HTTPS_PROXY and ALL_PROXY reach the
// child exactly as the user exported them: this makes an exception for one
// host, it does not turn their proxy off.
//
// ONE RESIDUAL, stated rather than papered over: codex has an opt-in setting
// that makes it read the operating system's own proxy configuration on macOS
// and Windows instead of the environment, and that path does not consult
// NO_PROXY at all. A user who turned it on and whose system proxy covers
// loopback is not exempted by anything here. Nothing in a child's environment
// can reach that setting, so the honest answer is that it is a machine ccdad
// cannot route rather than a case to pretend is handled.
func withNoProxyLoopback(env []string) []string {
	upper, lower := envValueOf(env, "NO_PROXY"), envValueOf(env, "no_proxy")
	up, low := upper, lower
	if up == "" {
		up = lower
	}
	if low == "" {
		low = upper
	}
	env = setEnv(env, "NO_PROXY", withLoopback(up))
	return setEnv(env, "no_proxy", withLoopback(low))
}

// envValueOf reads one variable out of a child environment slice.
//
// The LAST occurrence wins, which is what os/exec hands the child and therefore
// what the child would have read.
func envValueOf(env []string, name string) string {
	prefix := name + "="
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = kv[len(prefix):]
		}
	}
	return value
}

// withLoopback appends the loopback host to a NO_PROXY value unless a bare
// entry for it is already there.
//
// Entries are compared WHOLE and trimmed. A substring test would read
// `127.0.0.1:8080` as the exemption, which it is not -- and a value that
// already ends in a comma must not gain an empty component, which some proxy
// readers treat as "exempt everything" and others as a parse error.
func withLoopback(value string) string {
	for _, entry := range strings.Split(value, ",") {
		if strings.TrimSpace(entry) == loopbackHost {
			return value
		}
	}
	if strings.TrimSpace(value) == "" {
		return loopbackHost
	}
	return strings.TrimRight(value, ", ") + "," + loopbackHost
}
