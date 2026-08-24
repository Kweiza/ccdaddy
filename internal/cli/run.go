package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// SessionsDirName is the directory under the ccdad store that holds one
// credential home per `ccdad run` session. It is exported because `ccdad
// doctor` reports on what is left in it and, by the rule uninstall.go states,
// a top-level name belongs to the package that creates it.
//
// Deliberately under CCDAD_HOME rather than the system temp directory: /tmp is
// age-cleaned (30 days on this machine, and cleared at boot on many), and the
// OS deleting a live session's credentials underneath a running claude is not
// a failure mode worth having.
const SessionsDirName = "sessions"

// ProfilesDirName is the directory under the ccdad store that holds one
// persistent config home per account for `ccdad run --full-profile`.
//
// Persistent, and per account rather than per invocation, because a clone is
// not cheap: measured on the machine this was written on, a live
// CLAUDE_CONFIG_DIR is 3.0 GB across 19,741 files, 97% of it `projects/`. A
// per-invocation copy of that is not a command anyone would wait for.
const ProfilesDirName = "profiles"

// launchSpec is everything the platform-specific launcher needs. It is a
// struct rather than three arguments because a test reads it back.
type launchSpec struct {
	// Path is the resolved claude binary. Never a bare name: resolution
	// happens before the launch so a relative PATH entry can be refused.
	Path string
	// Args are the arguments after the program name, verbatim: everything at
	// or after ACCT reaches claude untouched.
	Args []string
	// Env is the COMPLETE child environment. ccdad never mutates its own.
	Env []string
	// Timeout bounds the child. Zero is no bound, which is what `run` needs: a
	// session lasts as long as the human at the keyboard makes it last, and a
	// runner that killed one at a deadline would be the worst kind of surprise.
	// A caller that hands claude ccdad's OWN arguments rather than a user's has
	// nobody at the keyboard and must set one.
	Timeout time.Duration
	// Silent sends the child's three descriptors to the null device instead of
	// ccdad's. `run` must never set it — it exists to hand the terminal over,
	// and a pipe there strips the TTY and puts Claude Code into
	// non-interactive mode. A caller whose child is a one-turn transaction sets
	// it, because the answer is not the point and words on stdout are a
	// contract every other command in this tree keeps clean.
	Silent bool
}

// lookClaude resolves the claude binary on PATH. It is a var so a test can
// exercise the real launcher against a program it controls.
var lookClaude = exec.LookPath

// lookProgram resolves the INTERPRETER an npm shim names, and is separate from
// lookClaude so a test can describe the machine this exists for: one where
// claude is a .cmd shim and node is a real executable somewhere else.
var lookProgram = exec.LookPath

// describeClaudeInstall names the version of the claude this command is about
// to run. It is a var for the same reason lookClaude is one: the branch that
// matters is a machine on the far side of 2.1.113, which no development machine
// running a current release can reach.
var describeClaudeInstall = ccver.Describe

// refuseKeychainEra is the default mode's refusal on a Claude Code that does not
// know the variable the default mode scopes with.
//
// The measurement, from cclink's keychain header and the item that filed this:
// CLAUDE_SECURESTORAGE_CONFIG_DIR does not occur even ONCE in 2.1.112. It
// arrived in 2.1.113, AFTER the keychain backend it nominally outranks was
// already gone. So on such a build newSession's whole mechanism is a variable
// nothing reads: claude falls through to the machine's own ~/.claude, the
// session runs as the LIVE login, and ccdad reports success. That is the same
// silent no-op as the keychain shadow, on the same population, and this command
// exists to promise the opposite.
//
// Refusing rather than warning, because a warning that is scrolled past leaves
// the user running the wrong account under a label that says otherwise — the
// risk register's High, with a different subject. Refusing rather than silently
// promoting to
// --full-profile, because that mode creates persistent per-account state and
// copies the config home, and changing a user's blast radius without being asked
// is not a smaller decision than refusing.
//
// It fires ONLY on a version ccdad actually read. An install ccdad cannot
// classify proceeds untouched: a refusal keyed on "ccdad could not tell" would
// break machines that work, and doctor's claude-version row is the place that
// reports a launcher it could not name.
//
// AND ONLY ON THE CREDENTIAL SHAPE THAT IS ACTUALLY DEFEATED, which is why it
// takes the blob. Of authorise's three shapes only an OAuth LOGIN is scoped by
// the credential home:
//
//   - setup-token accounts are scoped by CLAUDE_CODE_OAUTH_TOKEN in the child's
//     environment, and ccdad writes no credentials file for them at all. That
//     variable long predates 2.1.113, and this tree's own measurement is that
//     Claude Code prefers it over the stored login outright — so such a session
//     runs as the intended account on a keychain-era build, and refusing it
//     would block a case that works while asserting a failure mode that cannot
//     happen to it.
//   - api-key accounts are refused by authorise itself, in its own words, and
//     the era has nothing to do with why. Answering first with a keychain
//     message would replace an accurate refusal with a misleading one.
//
// So a stored token record of either kind returns early and the two other
// answers stand.
func refuseKeychainEra(install ccver.Install, blob cclink.Blob, label string) error {
	if !install.KeychainEra() {
		return nil
	}
	if _, hasToken := cclink.TokenRecordOf(blob); hasToken {
		return nil
	}
	return UsageError("%s does not know CLAUDE_SECURESTORAGE_CONFIG_DIR — the variable arrived in %s — so this "+
		"session would not be scoped at all: %s's login is a credentials file, and claude would read the "+
		"MACHINE's own file instead and run as the live login, while ccdad reported success. Run it with "+
		"--full-profile, which scopes CLAUDE_CONFIG_DIR and does work on this build; or upgrade Claude Code to "+
		"%s or later",
		install, ccver.LastKeychainEra.NextPatch(), label, ccver.LastKeychainEra.NextPatch())
}

// startChild starts claude, waits for it, and reports its exit status. It is a
// var because starting a real process is the one thing a test in this package
// cannot arrange, which is the rule every other uncontrollable dependency here
// follows.
var startChild = runChild

// claudeArgs is the separator rule: everything after ACCT, minus a single
// literal `--` sitting immediately after it.
//
// pflag will not do this for us. Measured under SetInterspersed(false): a `--`
// after the first positional stays in args and cmd.ArgsLenAtDash() reports -1,
// so there is nothing cobra-native to consult. Exactly one is dropped, and only
// in that position — a second `--`, or one further along the tail, is an
// argument claude was asked for.
func claudeArgs(args []string) []string {
	tail := args[1:]
	if len(tail) > 0 && tail[0] == "--" {
		return tail[1:]
	}
	return tail
}

// runSession is the credential home one run gets, and whether ccdad owns its
// lifetime.
type runSession struct {
	// home is the directory Claude Code will read .credentials.json from.
	home string
	// env is the complete child environment that points at it.
	env []string
	// ephemeral is true when ccdad created this for one run and deletes it
	// afterwards. A --full-profile profile is not: it is the accumulated state
	// the mode exists to preserve.
	ephemeral bool
	// globalConfig is the Claude Code global config THIS MODE OWNS, or "" when
	// the mode shares the machine's. Only --full-profile owns one, and owning
	// it is what makes an API-key account runnable: the key lives in that file
	// rather than in a credential home, so the default mode — which
	// deliberately leaves the machine's copy shared — has nowhere to put one.
	globalConfig string
}

// newProfile returns the persistent config home for an account, creating it on
// first use.
//
// The child gets CLAUDE_CONFIG_DIR and NOT CLAUDE_SECURESTORAGE_CONFIG_DIR.
// That is the whole mode: Claude Code resolves its credential root as
// CLAUDE_SECURESTORAGE_CONFIG_DIR ?? CLAUDE_CONFIG_DIR ?? ~/.claude, and
// mcpOAuth lives inside .credentials.json under that root — so setting both
// would scope mcpOAuth away from the profile and undo the only reason to pay
// for a profile at all.
func newProfile(uuid string) (runSession, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return runSession{}, err
	}
	home := filepath.Join(root, ProfilesDirName, uuid)
	_, statErr := os.Stat(home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return runSession{}, err
	}
	// Seeded on FIRST use only. A profile is the state this mode exists to
	// accumulate — trust answers, MCP logins, whatever the user changed inside
	// it — and re-copying the live config home on every run would throw that
	// away on the second one.
	if errors.Is(statErr, os.ErrNotExist) {
		if err := seedProfile(home); err != nil {
			// The directory was created a moment ago, and its EXISTENCE is
			// what tells the next run not to seed. Leaving it behind after a
			// failed seed turns one refusal into a profile that is silently
			// never seeded again.
			_ = os.RemoveAll(home)
			return runSession{}, err
		}
	}
	env := unsetEnv(childEnv(), "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	return runSession{
		home: home,
		env:  setEnv(env, "CLAUDE_CONFIG_DIR", home),
		// Resolved with the same rule Claude Code applies inside the child,
		// rather than spelled as <profile>/.claude.json: seedProfile copies
		// top-level FILES, so on a machine with the legacy .config.json every
		// profile has one — and Claude Code reads that in preference.
		globalConfig: ccpath.GlobalConfigPathIn(home),
	}, nil
}

// authorise decides how the child is told who it is, and reports whether a
// credentials file has to be written at all.
//
// Three shapes, the same three switch.go branches on, and `run` is the only
// command that can serve all of them:
//
//   - an OAuth login is a file, which is what the session's credential home is
//     for.
//   - a setup token is read by Claude Code from CLAUDE_CODE_OAUTH_TOKEN and
//     from nowhere else — `claude setup-token` prints it and deliberately does
//     not save it. switch REFUSES such an account and its refusal prescribes
//     exactly this: "Run Claude Code with it exported for the session instead".
//     Writing the stored record into a credentials file would put something in
//     the session that looks like a login and is not one.
//   - an API key is neither: Claude Code takes it from primaryApiKey in the
//     GLOBAL config, which is not a credential home at all. Which mode this is
//     therefore decides the answer. The default mode leaves the machine's
//     global config shared with the live session on purpose, so authorising an
//     API-key account there would mean writing the user's live configuration —
//     the one thing this command promises not to do — and it refuses, naming
//     the flag that can. --full-profile owns a global config of its own, so
//     there the key goes in and nothing outside the profile moves.
//
// The environment route was measured and rejected rather than overlooked.
// ANTHROPIC_API_KEY is read outright by `claude -p`, and for an INTERACTIVE
// session it is gated on the key's last 20 characters appearing in
// customApiKeyResponses.approved in that same global config — so it would work
// for one invocation shape and silently not for the other. Worse, `ccdad run
// ACCT --bare`, or CLAUDE_CODE_SIMPLE in the environment, bypasses that gate
// outright, which would make the behaviour depend on an argument ccdad
// forwards without reading. Writing the approval needs the same config write
// this takes anyway, so the environment route is more machinery for a worse
// contract.
func authorise(stderr io.Writer, session runSession, blob cclink.Blob, label string) ([]string, bool, error) {
	rec, ok := cclink.TokenRecordOf(blob)
	if !ok {
		return session.env, true, nil
	}
	switch rec.Kind {
	case "setup-token":
		return setEnv(session.env, "CLAUDE_CODE_OAUTH_TOKEN", rec.Token), false, nil
	case cclink.APIKeyKind:
		if session.globalConfig == "" {
			return nil, false, UsageError("%s is an API key account, and Claude Code reads an API key from "+
				"its global config rather than from a credential home — which this mode shares with the live "+
				"session rather than rewriting. Run it with --full-profile, which gives the session a global "+
				"config of its own; or make it the live credential with 'ccdad switch'", label)
		}
		if err := installProfileAPIKey(session, rec.Token); err != nil {
			return nil, false, err
		}
		noteInertProfileKey(stderr, session, label)
		return session.env, false, nil
	default:
		return nil, false, UsageError("%s carries a %q credential ccdad does not know how to run", label, rec.Kind)
	}
}

// installProfileAPIKey writes the account's key into the profile's own global
// config, and only that file.
//
// UpdateGlobalConfigAt abandons a write that would leave the bytes unchanged,
// so running the same account twice does not advance the mtime of a file
// Claude Code watches.
func installProfileAPIKey(session runSession, key string) error {
	return cclink.UpdateGlobalConfigAt(session.globalConfig, func(g *cclink.GlobalConfig) error {
		return cclink.SetPrimaryAPIKey(g, key)
	})
}

// noteInertProfileKey warns when the profile already holds an OAuth login.
//
// primaryApiKey is INERT while a claudeAiOauth record sits in the credential
// home — Claude Code binds anthropicAuthEnabled from the login and a stored
// key does not affect it, which is why activateAPIKeyAccount clears the live
// login as its second write. A profile is persistent, so a user who ran /login
// inside an earlier session in this one left exactly that behind.
//
// The login is NOT removed. It is something the user made inside their own
// profile, and deleting it so that ccdad's write takes effect is a worse
// answer than saying so. Without this the session runs as the wrong identity
// and reports nothing at all.
func noteInertProfileKey(stderr io.Writer, session runSession, label string) {
	creds := filepath.Join(session.home, ccpath.CredentialsFile)
	raw, err := os.ReadFile(creds)
	if err != nil {
		return
	}
	var blob cclink.Blob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return
	}
	if _, isLogin := blob["claudeAiOauth"]; !isLogin {
		return
	}
	fmt.Fprintf(stderr, "warning: %s holds an OAuth login from an earlier session in this profile, and Claude Code "+
		"reads it in preference to %s's API key. Remove that file, or sign out inside the session, for the key to "+
		"take effect.\n", creds, label)
}

// seedProfile copies the user's own configuration into a new profile.
//
// Top-level FILES only, and the rule is worth stating because it is what keeps
// the mode usable: files carry settings, the plugin registry and the global
// config with its per-project trust answers and MCP servers, while directories
// carry the bulk and the machine-specific caches. On the machine this was
// written on the live config home was 3.0 GB across 19,741 files, and 2.9 GB of
// that was `projects/` alone.
//
// .credentials.json is excluded by name. The profile's login comes from the
// store, which is what makes --full-profile "the user's settings, this
// account's login" rather than a second copy of whoever is logged in now.
//
// A missing source is not a failure: a machine that has never run Claude Code
// has none of these, and the profile is simply empty.
func seedProfile(home string) error {
	src, err := ccpath.ConfigHome()
	if err != nil {
		return err
	}
	// The source is resolved from the environment at CALL time, so inside a
	// `ccdad run --full-profile` session it is the OUTER account's profile
	// rather than the machine's configuration — and this copies top-level
	// files, the global config among them. A nested run would therefore seed
	// one account's profile from another's, carrying over its MCP servers, its
	// trust answers and the primaryApiKey ccdad itself installs for an API-key
	// account, into a directory nothing lists and `ccdad remove` never cleans.
	//
	// scopedSessionAllowed lets `ccdad run` through on the grounds that it
	// REPLACES the scope it inherits rather than reading it. That is true of
	// the variables handed to the child and false here, which is the whole
	// reason this check exists rather than a comment saying it cannot happen.
	//
	// Only the CREATE is refused, which is what keeps it narrow: a profile
	// that already exists is never re-seeded, so a nested run of an account
	// that has been run before still works.
	if root, rootErr := ccpath.StoreHome(); rootErr == nil && inside(root, src) {
		return UsageError("this shell's Claude Code configuration is %s, which is inside ccdad's own store —\n"+
			"seeding a new profile from it would copy another account's settings, and its API key, into this one.\n"+
			"Run 'ccdad run --full-profile' from a shell outside the session, once, to create the profile", src)
	}
	entries, err := os.ReadDir(src)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == ccpath.CredentialsFile {
			continue
		}
		if err := copyInto(filepath.Join(src, e.Name()), filepath.Join(home, e.Name())); err != nil {
			return err
		}
	}

	// The global config is NOT always inside the config home: Claude Code puts
	// it at (CLAUDE_CONFIG_DIR ?? $HOME)/.claude.json, so on a machine with no
	// CLAUDE_CONFIG_DIR it sits beside the home directory and the loop above
	// never saw it. Inside the profile it lands at <profile>/.claude.json,
	// which is where the child will look once CLAUDE_CONFIG_DIR points there.
	global, err := ccpath.GlobalConfigPath()
	if err != nil {
		return err
	}
	dst := filepath.Join(home, filepath.Base(global))
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		return copyInto(global, dst)
	}
	return nil
}

// copyInto copies one file at 0600, treating a missing source as nothing to do.
func copyInto(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", src, err)
	}
	return cclink.WriteFileAtomic(dst, data, 0o600)
}

// newSession creates a credential home for one run and returns its path.
//
// os.MkdirTemp gives 0700 without a second chmod, and its random suffix is what
// makes two concurrent `ccdad run` invocations for the SAME account safe: they
// must not share a directory, because they would then share Claude Code's
// credential locks and race each other's token refresh.
//
// The uuid is in the name so `ccdad doctor` can say which account a leftover
// belongs to without reading the credentials inside it.
func newSession(uuid string) (runSession, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return runSession{}, err
	}
	container := filepath.Join(root, SessionsDirName)
	if err := os.MkdirAll(container, 0o700); err != nil {
		return runSession{}, err
	}
	home, err := os.MkdirTemp(container, uuid+"-")
	if err != nil {
		return runSession{}, err
	}
	return runSession{
		home:      home,
		env:       setEnv(childEnv(), "CLAUDE_SECURESTORAGE_CONFIG_DIR", home),
		ephemeral: true,
	}, nil
}

// seedSession writes the account's stored credentials into the session's
// credential home.
//
// The live-file swap algorithm does not apply here and must not be reached
// for: it starts from the LIVE map and deletes the account-scoped keys before
// inserting the target's, because it is editing a file Claude Code already
// owns. This file has no base — it is created from nothing — so the stored
// snapshot is written as it stands, and the machine-scoped keys are simply
// absent inside the session. Those absences have a price: a gateway-trust
// prompt, and MCP logins that are not carried in — the cost of scoping
// credentials rather than cloning a profile.
func seedSession(blob cclink.Blob, session string) error {
	data, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("encoding the session credentials: %w", err)
	}
	return cclink.WriteFileAtomic(filepath.Join(session, ccpath.CredentialsFile), data, 0o600)
}

// cmdShimMetacharacters are the characters cmd.exe acts on outside quotes.
//
// `"` is in the set because Go escapes an embedded quote as `\"`, which cmd.exe
// does not understand — it toggles quoting state on every bare quote, so the
// tail of the argument lands outside quotes where the rest of this set is live.
// `%` is in it because variable expansion happens even inside quotes. A newline
// is in it because it ends the command line outright.
const cmdShimMetacharacters = "&|<>^%\"\n\r"

// cmdShimTarget reports whether CreateProcessW would run this program through
// cmd.exe, which is true of a .cmd or a .bat and of nothing else.
//
// This is now the whole condition on the shim branch in `run`. It used to be
// half of one: resolution was reached only where an argument would ALSO have
// been refused, so what ccdad launched depended on the text of a prompt. The
// extension is the honest axis — either cmd.exe is in the launch or it is not
// — and it is the one asked here.
//
// extOf rather than filepath.Ext, for the reason cmdshim.go gives at length:
// filepath picks its separator at BUILD time, and every line of this logic is
// written on Linux about a path spelled `C:\npm\claude.cmd`. filepath.Ext
// happens to be safe on that path, and being the only filepath call left in
// the shim logic would make it the one a reader has to re-derive.
func cmdShimTarget(path string) bool {
	switch strings.ToLower(extOf(path)) {
	case ".cmd", ".bat":
		return true
	}
	return false
}

// unsafeForCmdShim reports the first argument cmd.exe would re-interpret, or
// "" if every one of them would survive being read by it.
//
// Go 1.26 has no special-casing of .bat/.cmd in argument building anywhere —
// syscall.makeCmdLine implements CommandLineToArgvW rules only — so an
// argument with no space, quote or backslash is emitted RAW: `fix&whoami`
// reaches cmd.exe as two commands.
//
// It no longer decides whether to look past a shim; cmdShimTarget does that.
// It decides what happens when looking past FAILED, which is the only place
// cmd.exe is still in the launch: an argument it would eat has to be refused,
// and an argument it would not can go through the shim exactly as it always
// has. Callers ask it only about a program cmd.exe actually runs — asking
// about a native claude.exe would be a question with no consequence, so the
// extension check that used to live here would now be dead weight rather than
// a guard.
//
// Never pass a prompt on argv on Windows; this is that rule enforced, and
// refusing is deliberate. The alternative, quoting for cmd.exe, is a
// correctness liability that fails silently and only on one platform.
func unsafeForCmdShim(args []string) string {
	for _, arg := range args {
		if strings.ContainsAny(arg, cmdShimMetacharacters) {
			return arg
		}
	}
	return ""
}

// launchPastShim is the seam between `run` and cmdshim.go: it reads the shim
// at path and says what to launch instead.
//
// Reached for EVERY .cmd and .bat, which is the end state the narrow shape was
// staged towards. Go's escaping is then exactly right for every argument
// rather than only the dangerous ones, cmd.exe leaves the picture along with
// the `SET PATHEXT=%PATHEXT:;.JS;=;%` line whose purpose ccdad reimplements by
// refusing an interpreter that resolves to a .js, and two shapes of the same
// command stop taking two different routes to the same program.
//
// The narrowing existed because none of this had run on Windows, and that
// reason has expired rather than been argued away: `test (windows-latest)` has
// been green with cmdshim_windows_test.go in it since main 72e3f61.
//
// What did NOT widen is the failure behaviour: when resolution ERRORS, the
// caller refuses only the argument cmd.exe would have eaten and otherwise
// launches the shim as it always did. A parse that gives up costs a launch
// nothing it had.
//
// A parse that SUCCEEDS on a wrong model is the class the widening genuinely
// changed, and the narrow shape's "it can only turn a refusal into a different
// refusal" does not survive here — that held because resolution ran only on
// invocations already bound for a refusal, and every .cmd launch consumes the
// result now. Two things bound it, and neither is a reason to look away.
// parseNpmShim refuses outright whatever it does not model: a missing
// preamble, an unmodelled %VAR%, a quote inside a token. The leniency it does
// have — accepting a shim that carries EXTRA lines and silently dropping them
// — was measured rather than assumed harmless: the generated shim runs
// `endLocal` in the same &-chain and BEFORE `"%_prog%"`, so a variable such a
// line set is already gone by the time the child starts. (That is also why
// resolvePastShim reimplements the .js refusal instead of trusting the shim's
// own PATHEXT line, which the same endLocal discards.) What is left is a
// hand-edit that survives npm regenerating the file AND does something other
// than set a variable. A narrow gap, and a real one rather than a proved
// impossibility.
func launchPastShim(path string) (pastShim, error) {
	text, err := readShim(path)
	if err != nil {
		return pastShim{}, err
	}
	shim, ok := parseNpmShim(text, shimDirOf(path))
	if !ok {
		return pastShim{}, fmt.Errorf("%s is not a shim ccdad recognises", shimBaseOf(path))
	}
	return resolvePastShim(shim, fileExists, lookProgram)
}

// fileExists is a var so a test can describe a node.exe beside a shim on a
// machine that has neither.
var fileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// adoptBack copies a credential the session rotated back into the store.
//
// Claude Code refreshes inside the session's own credential home, and the
// server ROTATES the refresh token when it does — revoking the one ccdad still
// has. Deleting the session without looking would leave the store holding a
// dead token: `ccdad switch` would install it, the daemon could not poll with
// it, and the only repair would be logging in again. internal/tokens carries
// exactly this rule for the live file; this is the same rule for a file only
// `run` knows about.
//
// Only claudeAiOauth is carried back, the way tokens.save does it. The rest of
// what a session accumulates — mcpOAuth above all — belongs to the session's
// own scope and is not the account's.
func adoptBack(uuid, home string) error {
	raw, err := os.ReadFile(filepath.Join(home, ccpath.CredentialsFile))
	if err != nil {
		// No file is the ordinary case for a session that never refreshed, and
		// for one that never started: Claude Code exits before creating
		// anything when it finds no credentials.
		return nil
	}
	var session cclink.Blob
	if err := json.Unmarshal(raw, &session); err != nil {
		return fmt.Errorf("reading the session credentials back: %w", err)
	}
	fresh, ok := session["claudeAiOauth"]
	if !ok {
		return nil
	}
	return store.WithStore(func(st *store.Store) error {
		current, err := st.Credentials(uuid)
		if err != nil {
			return err
		}
		if _, isLogin := current["claudeAiOauth"]; !isLogin {
			// The account's stored credential is not an OAuth login — it is a
			// setup token or an API key — so there is nothing here that Claude
			// Code could have rotated. A session is a whole Claude Code, and a
			// user who runs /login inside one leaves a claudeAiOauth in the
			// session's own home; carrying that back would silently attach an
			// OAuth login to a token account and change what `switch` and
			// attribution make of it. The account's identity is not the
			// session's to change.
			return nil
		}
		if bytes.Equal(current["claudeAiOauth"], fresh) {
			return nil
		}
		next := cclink.Blob{}
		for k, v := range current {
			next[k] = v
		}
		next["claudeAiOauth"] = fresh
		acct, ok := st.Get(uuid)
		if !ok {
			return fmt.Errorf("%w: %q", store.ErrNotFound, uuid)
		}
		return st.Add(acct, next)
	})
}

// removeSession deletes a session's credential home and the lock Claude Code
// keeps beside it.
//
// The sibling is not a detail: Claude Code's legacy OAuth refresh lock is
// `realpath(<credential home>) + ".lock"`, created in the PARENT directory, so
// os.RemoveAll on the home alone leaves a directory behind on every session
// that ever refreshed. Both spellings are removed because cclock resolves
// symlinks before appending, and on macOS a path under /tmp resolves through
// /private/tmp — so the name Claude Code used is not always the one ccdad
// passed.
func removeSession(home string) error {
	locks := []string{home + ".lock"}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != home {
		locks = append(locks, resolved+".lock")
	}
	var first error
	for _, path := range append(locks, home) {
		if err := os.RemoveAll(path); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// childEnv is the environment every run starts from.
//
// A defined-but-empty CLAUDE_CONFIG_DIR is removed rather than passed on.
// Measured against Claude Code 2.1.238: its An() coalesces with `??`, so an
// empty value stays empty and every path derived from it becomes relative — the
// session creates projects/, sessions/ and backups/ in whatever directory the
// user happened to be in. ccpath.ConfigHome reads the same variable with
// `!= ""` and treats it as unset. Two readings of one value is a bug waiting
// for a report nobody can reproduce, so ccdad settles it here.
func childEnv() []string {
	env := os.Environ()
	if v, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok && v == "" {
		env = unsetEnv(env, "CLAUDE_CONFIG_DIR")
	}
	return env
}

// unsetEnv returns env with name removed entirely.
//
// Removing is not the same as setting it to "": Claude Code tests
// CLAUDE_SECURESTORAGE_CONFIG_DIR for DEFINEDNESS, and a defined-but-empty
// value resolves to ~/.claude — the LIVE credential home — rather than to
// CLAUDE_CONFIG_DIR. Emptying it to "turn isolation off" would point the child
// at exactly the file this command exists not to touch.
func unsetEnv(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// setEnv returns env with name set to value, replacing every earlier
// occurrence rather than appending a second one. os/exec would keep the last
// entry anyway, but a child that inherits the variable twice is a child whose
// environment reads differently depending on who looks at it.
func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// runChild starts claude, waits for it, and reports its status.
//
// The stdio handles are the real *os.File values on purpose: os/exec passes an
// *os.File straight to the child and replaces anything else with a pipe, which
// would strip the TTY and put Claude Code into non-interactive mode.
func runChild(spec launchSpec) (ExitCode, error) {
	ctx := context.Background()
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if spec.Silent {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return ExitFailure, fmt.Errorf("opening %s: %w", os.DevNull, err)
		}
		defer devnull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	}

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return ExitFailure, fmt.Errorf("starting %s: %w", filepath.Base(spec.Path), err)
		}
	}
	// Asked AFTER the wait, because a child killed by the deadline exits with a
	// signal status and exitStatus would report that as 137 — a number that says
	// the machine killed it and not that ccdad did.
	if ctx.Err() != nil {
		return ExitFailure, fmt.Errorf("%s did not finish within %s", filepath.Base(spec.Path), spec.Timeout)
	}
	return exitStatus(cmd.ProcessState), nil
}

// exitStatus is the child's status in ccdad's terms.
//
// ProcessState.ExitCode() answers -1 for a process killed by a signal, and
// os.Exit(-1) exits 255 — a value that means nothing to anyone. The shell
// convention is 128+N, which for SIGINT is 130, the same number the exit
// contract already gives ExitInterrupted; a session the user Ctrl-C'd
// therefore reads the same whether the shell reported it or ccdad did.
//
// No build tag: syscall.WaitStatus exists on every GOOS ccdad ships to, and on
// Windows Signaled() is hardcoded false, so the branch is simply never taken
// there. That is the correct answer for Windows, which has no signals — a
// Ctrl-C'd child exits with STATUS_CONTROL_C_EXIT and that number is its status.
func exitStatus(state *os.ProcessState) ExitCode {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitCode(128 + int(ws.Signal()))
	}
	return ExitCode(state.ExitCode())
}

// atLeastOneAccount is spelled out rather than delegated to cobra.MinimumNArgs
// so the violation carries this binary's exit code and a message that says what
// to do next. Cobra's own Args errors are plain errors, which would exit 1.
// The shape is switch.go's exactlyOneAccount; it lives here because `run` is
// its only caller, the way checkSwitchFlags lives beside switch.
func atLeastOneAccount(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == 0 {
			return UsageError("%s needs an account; run 'ccdad list' to see them", verb)
		}
		return nil
	}
}

func newRunCmd() *cobra.Command {
	var fullProfile bool

	cmd := &cobra.Command{
		Use:   "run <ACCOUNT> [claude args…]",
		Short: "Start a Claude Code session as an account, without changing the live login",
		Long: "ACCOUNT may be a display index, an alias, an email address, or a uuid prefix.\n" +
			"There is no interactive disambiguation: an ambiguous reference is exit 2, because\n" +
			"this command hands control to claude and callers need it to be deterministic.\n\n" +
			"Everything at or after ACCOUNT is passed to claude verbatim, hyphens included. A\n" +
			"single '--' immediately after ACCOUNT is dropped; a second one is claude's. That\n" +
			"cuts both ways: 'ccdad run --full-profile ACCOUNT' sets ccdad's flag, while\n" +
			"'ccdad run ACCOUNT --full-profile' hands the same word to claude, and\n" +
			"'ccdad run ACCOUNT --help' prints claude's help rather than this one.\n\n" +
			"The live login is never changed. By default the session gets a credential home\n" +
			"of its own holding only that account's login, which is the smallest blast radius\n" +
			"available — at the cost that MCP logins do not come with it, because Claude Code\n" +
			"keeps them in the same file.\n\n" +
			"That default needs Claude Code 2.1.113 or later: the variable it scopes with\n" +
			"did not exist before then, so on an older build the session would silently read\n" +
			"the machine's own login. ccdad refuses to start rather than run as the wrong\n" +
			"account, and names --full-profile, which scopes something every era reads. Only\n" +
			"for accounts whose login is a credentials file: a setup-token account is scoped\n" +
			"by CLAUDE_CODE_OAUTH_TOKEN instead, which every era reads, so it still runs.\n\n" +
			"--full-profile gives the account a whole config home instead, kept between runs\n" +
			"under the ccdad store, so its MCP logins and trust answers survive. It is seeded\n" +
			"once from the live config home — top-level files only, never the project history —\n" +
			"and never from the live login, which still comes from the ccdad store.\n\n" +
			"The exit status is claude's, not ccdad's: this command is a runner. A session\n" +
			"killed by a signal reports 128 plus the signal number, as a shell would.",
		Args:          atLeastOneAccount("run"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			// Every account-taking command turns a resolution failure into a
			// usage error, and this command is the reason there is no
			// interactive fallback to reach for: `ccdad run` ends in an exec,
			// so callers need determinism.
			target, err := store.Resolve(s.Accounts(), args[0])
			if err != nil {
				return UsageError("%s", err.Error())
			}

			// Read before the session directory is created rather than after,
			// because the refusal below turns on which credential shape this
			// account is — and a refusal that first made a directory holding
			// nothing would be tidied by the defer, but only after the fact.
			blob, err := s.Credentials(target.UUID)
			if err != nil {
				return err
			}

			path, err := lookClaude("claude")
			if err != nil {
				return err
			}
			// Described from the path PATH just gave, not probed independently:
			// the version that matters is the one belonging to the binary this
			// invocation is about to exec, and a second resolution could name a
			// different one. Before the shim dance below for the same reason —
			// launchPastShim replaces the path with an interpreter, which is
			// node rather than claude.
			if !fullProfile {
				if err := refuseKeychainEra(describeClaudeInstall(path), blob, target.Label()); err != nil {
					return err
				}
			}
			tail := claudeArgs(args)
			var shimEnv []string
			if cmdShimTarget(path) {
				past, err := launchPastShim(path)
				bad := unsafeForCmdShim(tail)
				switch {
				case err == nil:
					// The note is kept for the rescue and dropped for the
					// ordinary launch. It is only TRUE when there is an
					// argument that would not have survived, and on the
					// ordinary path it would print on every Windows session
					// an npm install ever starts — a line the reader cannot
					// act on, in the middle of claude's own output.
					if bad != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "note: %q would not survive %s, so ccdad is running %s directly.\n",
							bad, shimBaseOf(path), shimBaseOf(past.path))
					}
					path, tail, shimEnv = past.path, append(past.args, tail...), past.env
				case bad != "":
					return UsageError("%s is a cmd.exe shim, and cmd.exe would re-interpret %q rather than "+
						"pass it on. ccdad could not run its interpreter directly instead (%v); quote the "+
						"argument differently, or install the native claude.exe", path, bad, err)
				default:
					// A shim ccdad cannot read, carrying arguments cmd.exe
					// handles correctly: the launch stays on the shim, which
					// is what it did before any of this existed. Refusing
					// here instead would turn every working invocation on an
					// unrecognised .cmd — a wrapper someone wrote, a shim
					// from a future npm, the no-shebang shape cmd.exe runs by
					// file association — into a usage error, and that is the
					// one thing widening the resolution must not do.
				}
			}

			newHome := newSession
			if fullProfile {
				newHome = newProfile
			}
			session, err := newHome(target.UUID)
			if err != nil {
				return err
			}
			// Deferred rather than written after the wait: a seeding failure,
			// a missing binary or a panic all leave a directory holding a live
			// refresh token, and only the defer covers all three.
			defer func() {
				// The session is NOT removed when the adopt-back fails. The
				// file in it may be the only copy of a token the server has
				// already rotated to, so deleting it would turn a reportable
				// problem into an unrecoverable one.
				if err := adoptBack(target.UUID, session.home); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"note: this session's credentials could not be carried back into the store (%v).\n"+
							"They are kept at %s; 'ccdad doctor' reports it.\n", err, session.home)
					return
				}
				if !session.ephemeral {
					return
				}
				if err := removeSession(session.home); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: could not remove the session directory %s (%v); "+
						"'ccdad doctor' reports it.\n", session.home, err)
				}
			}()
			env, needsFile, err := authorise(cmd.ErrOrStderr(), session, blob, target.Label())
			if err != nil {
				return err
			}
			if needsFile {
				if err := seedSession(blob, session.home); err != nil {
					return err
				}
			}

			for _, kv := range shimEnv {
				// A `#!/usr/bin/env FOO=bar node` shebang: the shim exports
				// these before running anything, so a launch that goes past
				// the shim has to carry them or the interpreter starts in an
				// environment the package did not ask for.
				name, value, _ := strings.Cut(kv, "=")
				env = setEnv(env, name, value)
			}
			code, err := startChild(launchSpec{
				Path: path,
				Args: tail,
				Env:  env,
			})
			if err != nil {
				return err
			}
			if code != ExitOK {
				return WithCode(errSilent, code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fullProfile, "full-profile", false,
		"give the session a whole config home of its own, so its MCP logins survive")
	// Everything at or after ACCT must reach claude verbatim.
	// Without this, cobra parses `-p` as its own and exits 2 before RunE.
	cmd.Flags().SetInterspersed(false)
	return cmd
}
