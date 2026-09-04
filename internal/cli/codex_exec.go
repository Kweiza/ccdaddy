package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
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

// codexHealthClient probes ccdad's own listener, and it NEVER consults the
// environment's proxy variables.
//
// Transport.Proxy is nil rather than left at http.ProxyFromEnvironment, and the
// honest reason is narrower than the child's. MEASURED on this Go: with
// HTTP_PROXY and ALL_PROXY both exported, http.ProxyFromEnvironment answers nil
// for http://127.0.0.1:9999 and the proxy URL for http://example.com -- the
// standard library exempts a loopback host by itself, unconditionally, with no
// NO_PROXY entry involved. So this is a policy pinned rather than a bug fixed.
// The exemption the child needs is spelled out by hand one process later
// exactly because ITS runtime does not carry that rule, and ccdad's own
// readiness check should not rest on a default that merely happens to agree.
//
// The timeout is short because the answer is local. A health route that takes
// two seconds on loopback is a daemon that is not answering, and the caller
// polls, so a long timeout would only spend the whole launch budget on one try.
var codexHealthClient = &http.Client{
	Timeout:   2 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// codexProxyHealth asks the port whether ccdad is behind it.
//
// The route is unauthenticated and answers nothing but a version and a port,
// which is what makes it safe to be the one thing a launcher can ask before it
// has a launch secret to ask with.
func codexProxyHealth(port int) error {
	resp, err := codexHealthClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, codexproxy.HealthPath))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drained and capped: an unread body leaves the connection unusable, and
	// something that is not ccdad on this port could answer with anything at
	// all.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("it answered %s", resp.Status)
	}
	return nil
}

// codexProxyReady polls until the published port answers ccdad's health route,
// and reports the LAST reason it did not.
//
// The last reason rather than the first: a launch that started a daemon walks
// through "no document yet", "no port yet" and "not listening yet" on its way
// to an answer, and only the state it ended in is worth telling a user about.
func codexProxyReady() (int, error) {
	deadline := time.Now().Add(daemonWaitTimeout)
	last := "the ccdad daemon published no codex proxy port"
	for {
		s, ok, err := daemon.ReadStatus()
		switch {
		case err != nil:
			last = fmt.Sprintf("the ccdad daemon's status document cannot be read (%v)", err)
		case !ok:
			last = "the ccdad daemon has published no status document"
		case s.CodexProxyPort == 0:
			last = "the ccdad daemon published no codex proxy port"
		default:
			port := s.CodexProxyPort
			herr := codexProxyHealth(port)
			if herr == nil {
				return port, nil
			}
			last = fmt.Sprintf("nothing answered ccdad on 127.0.0.1:%d (%v)", port, herr)
		}
		if !time.Now().Before(deadline) {
			return 0, errors.New(last)
		}
		time.Sleep(daemonPollInterval)
	}
}

// ensureCodexDaemon is step 3 of a launch: use the daemon that is there, start
// one, or say why neither is possible. It returns "" when a daemon should now
// exist.
//
// spawnDaemon plus waitForSingleton, and deliberately NOT startDaemonFrom.
// That function carries the Claude-login refusals -- a credential home another
// store claims, and a repair of an unreadable login that can put an interactive
// keychain prompt in front of a user who asked to run codex. None of those are
// this launch's business: a Codex session touches no Claude credential at all,
// and a keychain prompt in the middle of `codex` is the kind of surprise that
// makes people stop using a tool.
func ensureCodexDaemon() string {
	held, err := singletonHeld()
	if err == nil && held {
		return ""
	}
	if reason := autoStartRefusal(); reason != "" {
		return reason
	}
	if err != nil {
		// "Cannot determine" never becomes "not running". Spawning on an
		// unprobeable lock is a daemon per invocation forever on a filesystem
		// where locks do not work.
		return fmt.Sprintf("the ccdad singleton lock cannot be probed (%v)", err)
	}
	if err := spawnDaemon(""); err != nil {
		return fmt.Sprintf("a ccdad daemon could not be started (%v)", err)
	}
	// It WAITS, where the auto-start hook deliberately does not. The hook's
	// caller is doing something else and the next command benefits; this
	// caller's whole next step is to ask that daemon for a port.
	up, err := waitForSingleton(true)
	if err != nil {
		return fmt.Sprintf("the ccdad singleton lock cannot be probed (%v)", err)
	}
	if !up {
		return fmt.Sprintf("a ccdad daemon was started but had not taken the singleton after %s", daemonWaitTimeout)
	}
	return ""
}

// codexProxyForLaunch is steps 3 and 4 together: the port codex should be
// pointed at, or the reason there is none.
//
// Two steps and not one, because a held singleton is not evidence of a proxy.
// `ccdad auto` holds the same lock with no listener at all, and a daemon whose
// bind failed holds it too -- so the process question and the listener question
// have different answers and a launch needs the second one.
func codexProxyForLaunch() (int, string) {
	if reason := ensureCodexDaemon(); reason != "" {
		return 0, reason
	}
	port, err := codexProxyReady()
	if err != nil {
		return 0, err.Error()
	}
	return port, ""
}

// codexProviderID is the model provider ccdad declares on the command line.
//
// Declared per launch and written into no file. codex's own config.toml is the
// user's, a provider merged into it would outlive ccdad, and a codex started
// from an editor would then pick the settings up with no ccdad there to serve
// them -- a session that hangs on a dead port rather than one that falls back.
const codexProviderID = "ccdad"

// codexOverrides is the seven -c settings a routed launch carries.
//
// The first six declare one custom model provider and point codex at ccdad's
// listener, and their EFFECT was measured rather than assumed: with exactly
// these, codex sends only POST /responses; its bearer is the env_key's value;
// it sends no chatgpt-account-id of its own, so the one the proxy adds is
// authoritative; and it attempts no WebSocket, because supports_websockets is
// absent and defaults off. `requires_openai_auth = false` is what stops codex
// looking for an auth.json at all, which is why a routed session starts with no
// login and needs none.
//
// The seventh is not about the provider. codex's default
// shell_environment_policy inherits the WHOLE environment into every command
// the agent runs, so without this line a prompt-injected `env` prints the
// launch secret straight into the model's context. Excluding it does not make
// the secret private -- a same-uid process can read the launcher's environment
// either way -- it removes the one path that hands it to a model.
//
// There is no model pin here, deliberately: which model a session runs is the
// user's business and codex's default, and ccdad pinning one would silently
// override a `-c model=` the user typed.
func codexOverrides(port int) []string {
	return []string{
		"-c", "model_provider=" + codexProviderID,
		"-c", fmt.Sprintf("model_providers.%s.name=%q", codexProviderID, codexProviderID),
		"-c", fmt.Sprintf("model_providers.%s.base_url=%q", codexProviderID,
			fmt.Sprintf("http://127.0.0.1:%d", port)),
		"-c", fmt.Sprintf("model_providers.%s.env_key=%q", codexProviderID, codexKeyEnv),
		"-c", fmt.Sprintf("model_providers.%s.requires_openai_auth=false", codexProviderID),
		"-c", fmt.Sprintf("model_providers.%s.wire_api=%q", codexProviderID, "responses"),
		"-c", fmt.Sprintf("shell_environment_policy.exclude=[%q]", codexKeyEnv),
	}
}

// codexLaunchOptions is what one launch is.
type codexLaunchOptions struct {
	// Pin is the account uuid this launch is bound to, or "" for a launch that
	// follows the serving pointer. A pinned launch never falls back.
	Pin string
	// Args are everything codex is handed, verbatim.
	Args []string
}

// runCodexLaunch is the whole launcher, shared by `ccdad codex exec` and
// `ccdad run <codex-account>`.
//
// The eight steps are in order and each is load-bearing:
//
//  1. resolve the real codex, past ccdad's own shim.
//  2. hand `login` and `logout` to it untouched, unless the launch is pinned.
//  3. make sure a daemon exists, or say why one cannot be started here.
//  4. prove the LISTENER, not the process.
//  5. a pinned launch refuses if 3-4 failed; an unpinned one warns and runs.
//  6. create the launch record and hold its lock.
//  7. spawn codex with the overrides, the secret and the proxy exemption.
//  8. release the lock and delete the record when the child ends.
func runCodexLaunch(cmd *cobra.Command, opts codexLaunchOptions) error {
	// 1.
	path, err := codexBinary()
	if err != nil {
		return err
	}

	// 2. Both verbs REVOKE the stored grant server-side, with no undo. Routed
	// through the proxy a logout would revoke a grant ccdad manages and take
	// the account out of service for every session on the machine. They go to
	// the real codex with no overrides and no key: an untouched codex talking
	// to its own home, which ccdad neither reads nor writes.
	//
	// A PINNED launch refuses them instead, and which invocation each surface
	// stands for is the whole difference. `ccdad codex exec` is what the shim
	// runs, so after `ccdad codex shim install` the word `codex` IS this
	// command -- a carve-out is the only thing keeping `codex login` working on
	// that machine, and there is no account in the invocation to contradict.
	// Nothing rewrites anything into `ccdad run <acct>`: it is typed with an
	// account named, and neither verb so much as reads that account. Both act
	// on codex's own home, which is the fallback the pin exists to forbid, and
	// the silent kind -- a login against the wrong home reports success.
	if len(opts.Args) > 0 && (opts.Args[0] == "login" || opts.Args[0] == "logout") {
		if opts.Pin != "" {
			return UsageError("ccdad run %s cannot run `codex %s`: both act on codex's own home "+
				"rather than on the named account, and logout revokes that grant server-side with "+
				"no undo. Run `codex %s` from a plain shell if you mean codex's own home, or "+
				"`ccdad codex add` for an account ccdad serves.",
				codexPinLabel(opts.Pin), opts.Args[0], opts.Args[0])
		}
		// A launcher can itself be running inside a routed session; the
		// inherited key must not reach a codex ccdad is not routing.
		return startCodex(cmd, path, opts.Args, unsetEnv(childEnv(), codexKeyEnv))
	}

	// 3 and 4.
	port, reason := codexProxyForLaunch()
	if reason != "" {
		// 5. A launch that carries a pin NEVER falls back. The user named an
		// account; running the session as whatever codex's own home holds would
		// bill a different one and report success.
		if opts.Pin != "" {
			return UsageError("ccdad run %s needs the ccdad daemon and one cannot be started here: %s. "+
				"Run it from a plain shell.", codexPinLabel(opts.Pin), reason)
		}
		// An unpinned launch runs codex anyway. Refusing would make a daemon
		// that will not start mean no codex at all, which is worse than a
		// session ccdad cannot see -- but it is said out loud, in several
		// lines, because the failure is otherwise invisible: the session works,
		// and it spends an account ccdad neither chose nor can measure.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"ccdad: this codex session is NOT routed through ccdad.\n"+
				"  why: %s\n"+
				"  it spends whatever %s holds, which ccdad neither chose nor can see.\n"+
				"  fix: run it from a plain shell, or start a daemon with `ccdad daemon start`.\n",
			reason, codexOwnHome())
		if root, rerr := ccpath.StoreHome(); rerr == nil {
			// Discarded deliberately: a tally that could not be written is not
			// a reason to refuse a launch the user is entitled to. The launcher
			// has no other way to tell a daemon this happened -- there is no
			// daemon to tell, which is why this branch was taken.
			_ = codexlaunch.NoteUnrouted(root)
		}
		// A launcher can itself be running inside a routed session; the
		// inherited key must not reach a codex ccdad is not routing.
		return startCodex(cmd, path, opts.Args, unsetEnv(childEnv(), codexKeyEnv))
	}

	root, err := ccpath.StoreHome()
	if err != nil {
		return err
	}
	// 6.
	launch, err := codexlaunch.Create(root, opts.Pin)
	if err != nil {
		return err
	}
	// 8, as a defer: the lock has to be released and both files deleted
	// whatever the child did, including a panic. A record left behind with its
	// lock released is not a security hole -- the proxy try-locks it and reaps
	// it -- but it is a file per session on a machine nobody is watching.
	defer func() {
		if cerr := launch.Close(); cerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "note: this codex launch's record could not be removed (%v); "+
				"the daemon sweeps it on its next tick.\n", cerr)
		}
	}()

	// 7.
	env := withNoProxyLoopback(setEnv(childEnv(), codexKeyEnv, launch.Secret()))
	return startCodex(cmd, path, append(codexOverrides(port), opts.Args...), env)
}

// startCodex spawns codex, waits for it, and reports its status as ccdad's own.
//
// Spawn and wait rather than exec, and the launch record is the reason: it has
// to be released and deleted when the child ends, and an exec'd process has no
// "when the child ends". The stdio handles are the real files, so codex keeps
// its terminal.
//
// The cmd.exe dance is `ccdad run`'s, for the same machine: an npm-installed
// codex on Windows is a .cmd, and CreateProcessW runs a .cmd through cmd.exe,
// which re-interprets `& | < > ^ % "` in any argument it is handed. Reading the
// shim and launching its interpreter directly takes cmd.exe out of the picture;
// a shim ccdad cannot read still goes through it, exactly as it did before, and
// there an argument cmd.exe would eat is refused rather than mangled.
func startCodex(cmd *cobra.Command, path string, args, env []string) error {
	if cmdShimTarget(path) {
		past, perr := launchPastShim(path)
		bad := unsafeForCmdShim(args)
		switch {
		case perr == nil:
			if bad != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %q would not survive %s, so ccdad is running %s directly.\n",
					bad, shimBaseOf(path), shimBaseOf(past.path))
			}
			path, args = past.path, append(past.args, args...)
			for _, kv := range past.env {
				// A `#!/usr/bin/env FOO=bar node` shebang: the shim exports
				// these before running anything, so a launch that goes past the
				// shim has to carry them.
				name, value, _ := strings.Cut(kv, "=")
				env = setEnv(env, name, value)
			}
		case bad != "":
			return UsageError("%s is a cmd.exe shim, and cmd.exe would re-interpret %q rather than pass "+
				"it on. ccdad could not run its interpreter directly instead (%v); quote the argument "+
				"differently, or install a native codex.exe", path, bad, perr)
		default:
			// A shim ccdad cannot read, carrying arguments cmd.exe handles
			// correctly: the launch stays on the shim, which is what every
			// working invocation on an unrecognised .cmd already does.
		}
	}
	code, err := startChild(launchSpec{Path: path, Args: args, Env: env})
	if err != nil {
		return err
	}
	if code != ExitOK {
		// The exit status is codex's, not ccdad's: this command is a runner,
		// exactly as `ccdad run` is.
		return WithCode(errSilent, code)
	}
	return nil
}

// codexPinLabel is how a refusal names the pinned account.
//
// The uuid is the fallback and not the answer: the user typed an alias or an
// email, and a message naming a uuid asks them to go and look one up. A store
// that cannot be opened is not worth failing the refusal over -- the refusal is
// already the bad news.
func codexPinLabel(uuid string) string {
	s, err := store.Open()
	if err != nil {
		return uuid
	}
	if a, ok := s.Get(uuid); ok {
		return a.Label()
	}
	return uuid
}

// codexOwnHome names the credential home an UNROUTED codex uses, for the banner
// and for nothing else.
//
// ccdad never reads or writes it. This is the one place in the tree its name is
// spoken, and it is spoken so the banner can say whose account is about to be
// billed instead of ccdad's choice.
func codexOwnHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, err := ccpath.Home()
	if err != nil {
		return "codex's own home"
	}
	return filepath.Join(home, ".codex")
}

func newCodexExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec [-- codex args…]",
		Short: "Run codex through ccdad's local proxy",
		Long: "exec starts the real codex with its API base pointed at the loopback proxy the\n" +
			"ccdad daemon runs, so the session is billed to the account ccdad is serving and\n" +
			"the next new thread follows a switch. codex holds no OAuth token of its own:\n" +
			"ccdad owns the login, the refresh and the quota reading.\n\n" +
			"This is what `~/.ccdad/bin/codex` runs, so `ccdad codex shim install` and then\n" +
			"typing `codex` is the same thing. Run it by name on Windows, where there is no\n" +
			"shim.\n\n" +
			"Everything after `--` reaches codex verbatim. `login` and `logout` are handed\n" +
			"to the real codex untouched, because both revoke a grant server-side and a\n" +
			"routed one would revoke a grant ccdad manages.\n\n" +
			"The exit status is codex's, not ccdad's: this command is a runner. A launch\n" +
			"ccdad cannot route runs anyway and says so on stderr, because a daemon that\n" +
			"will not start should not mean no codex at all.",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// args is handed on VERBATIM. pflag has already consumed the
			// leading `--` as its own terminator -- measured, not assumed -- so
			// stripping another here would eat a separator the user typed for
			// codex.
			return runCodexLaunch(cmd, codexLaunchOptions{Args: args})
		},
	}
	// An argument list typed WITHOUT `--` stops flag parsing at the first bare
	// word, so a `-c` after it reaches codex instead of cobra.
	//
	// That form is the one a user types by hand: the shim spells the separator
	// out, but `ccdad codex exec exec -c model=x` typed at a prompt has nothing
	// to tell cobra where its own flags stop. Interspersed on, cobra reads that
	// `-c` as an unknown shorthand of its own and exits 2 before RunE. A `--`
	// tail is not what this defends -- pflag appends the remainder of one
	// verbatim either way, and removing this line breaks none of it.
	cmd.Flags().SetInterspersed(false)
	return cmd
}
