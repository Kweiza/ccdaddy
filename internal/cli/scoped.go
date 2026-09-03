package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// A `ccdad run` session is a whole Claude Code, and Claude Code has a Bash
// tool. Anything typed in there -- by the user or by the model -- inherits the
// session's CLAUDE_SECURESTORAGE_CONFIG_DIR, or its CLAUDE_CONFIG_DIR under
// --full-profile, and every ccpath resolver reads the environment at CALL
// time. So `ccdad switch other` typed inside a parallel session wrote
// <session>/.credentials.json, printed "Switched to", and changed nothing
// about the live login -- while replacing the session's own login with another
// account's, in a directory `run` deletes on the way out.
//
// autostart.go's rule 3 already refuses to spawn a DAEMON into such a shell.
// This is the same policy for the commands a human types, and it is
// deliberately NOT the same test.
//
// Rule 3 asks whether CLAUDE_SECURESTORAGE_CONFIG_DIR is DEFINED, which is the
// right question there: a daemon outlives the terminal it was born in, so any
// scoping at all is a scoping it would carry forever. A command a human typed
// is different. A user who scoped their own shell has TOLD ccdad where their
// live login is, and switching into it is the correct answer rather than a
// bug. What ccdad knows better about is the directories it created itself --
// so this asks a narrower question: is the credential home one of MINE.
//
// The narrowness is load-bearing in the other direction too. The whole test
// suite runs with a scoped credential home (see isolate), and a DEFINED test
// here would refuse every command in it.

// scopedSession is a `ccdad run` session this process is running INSIDE.
type scopedSession struct {
	// envVar is the variable that put us here, named in the refusal because it
	// is what a user has to look at to believe the message.
	envVar string
	// home is the credential or config home it points at.
	home string
	// ephemeral is true for a default-mode session, which `run` deletes when
	// the run ends. A --full-profile profile is kept.
	ephemeral bool
}

// describe is the half of the refusal that names the session. It is the
// directory rather than the account: the account would have to be parsed back
// out of the directory name, which is os.MkdirTemp's shape rather than a
// format ccdad chose, and a refusal that names the wrong account is worse than
// one that names none. The uuid is in the path either way.
func (s scopedSession) describe() string {
	if s.ephemeral {
		return fmt.Sprintf("%s=%s, a credential home ccdad created for one `ccdad run` and deletes when that run ends",
			s.envVar, s.home)
	}
	return fmt.Sprintf("%s=%s, the `ccdad run --full-profile` config home ccdad keeps for that account",
		s.envVar, s.home)
}

// currentScopedSession reports the `ccdad run` session this process is inside.
//
// A store root that cannot be resolved answers "not inside one". This function
// only ever DENIES work, so failing open is right: the alternative is a
// machine with no HOME where every command refuses with a message about
// sessions rather than about HOME, which is the error it actually has.
func currentScopedSession() (scopedSession, bool) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return scopedSession{}, false
	}
	// The default mode. Empty is not a session: Claude Code reads a
	// defined-but-empty value as ~/.claude, which is the live credential home
	// and the opposite of a scoped one.
	if v := os.Getenv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); v != "" {
		if inside(filepath.Join(root, SessionsDirName), v) {
			return scopedSession{envVar: "CLAUDE_SECURESTORAGE_CONFIG_DIR", home: v, ephemeral: true}, true
		}
	}
	// --full-profile REMOVES the variable above rather than emptying it, and
	// scopes with CLAUDE_CONFIG_DIR instead, so a check that reads only the
	// credential variable sees an unscoped shell here.
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		if inside(filepath.Join(root, ProfilesDirName), v) {
			return scopedSession{envVar: "CLAUDE_CONFIG_DIR", home: v}, true
		}
	}
	return scopedSession{}, false
}

// inside reports whether path is a directory under container.
func inside(container, path string) bool {
	c, cErr := filepath.Abs(container)
	p, pErr := filepath.Abs(path)
	if cErr != nil || pErr != nil {
		return false
	}
	if relInside(c, p) {
		return true
	}
	// The same place reached by a different spelling is still that place. On
	// macOS a store under /tmp resolves through /private/tmp, and
	// daemon.ChildEnv hands its child a symlink-resolved copy of both
	// variables -- so the string in the environment is not always the one
	// ccpath would build. EvalSymlinks fails on a path that does not exist,
	// which is the ordinary state of a sessions container on a machine that
	// has never run `ccdad run`, so a failure here means "no" rather than
	// anything about the answer.
	rc, cErr := filepath.EvalSymlinks(c)
	rp, pErr := filepath.EvalSymlinks(p)
	if cErr != nil || pErr != nil {
		return false
	}
	return relInside(rc, rp)
}

// relInside is the containment test itself.
//
// filepath.Rel rather than a string prefix, and that is the whole reason this
// is a function: "<store>/sessions-old" has "<store>/sessions" as a string
// prefix and is a different directory, which is the bug every containment
// check written with HasPrefix has.
//
// Rel also settles the case question, which is worth writing down because an
// earlier version of this file hand-folded the paths on Windows and its
// comment said Rel does not fold. It does: Rel compares every element with
// sameWord, and sameWord is `a == b` in path_unix.go and strings.EqualFold in
// path_windows.go (checked in the Go tree, not reasoned about). So Rel is
// case-insensitive exactly where the platform is, and a manual fold was both
// redundant and — on a Linux build with goos forced to "windows" — the only
// thing its own test was exercising.
// TestWindowsASessionSpeltInADifferentCaseIsStillThatSession asserts the real
// behaviour, on the platform that has it.
func relInside(container, path string) bool {
	rel, err := filepath.Rel(container, path)
	if err != nil {
		return false
	}
	// "." is the container itself. `run` never points a session at it, and
	// treating it as one would refuse a user who scoped a shell at ccdad's own
	// sessions directory -- an odd thing to do, but theirs to do.
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// scopedSessionRefusals are the commands that must not run inside a `ccdad
// run` session, each with the clause saying what it would get wrong. Presence
// in this map is the refusal; the value is the half of the message that is not
// shared.
//
// Every entry is here because it acts on Claude Code's OWN state -- which
// inside a session is the session's copy -- or because it would outlive the
// session still carrying its scope. A command that writes only ccdad's store
// is not one of these: CCDAD_HOME is not scoped by a session, so those
// commands do exactly what they say. See scopedSessionAllowed.
var scopedSessionRefusals = map[string]string{
	// The reported bug and its unattended twin. Both end in
	// cclink.ActivateWith at ccpath.CredentialsPath(), resolved when the
	// credential lock is taken -- so both write the session's file and report
	// a switch that the live login never saw.
	"ccdad switch": "would write this session's credentials file rather than the live login",
	"ccdad auto":   "would write this session's credentials file rather than the live login",

	// add reads the live file to decide which machine-scoped keys belong to
	// the account it is storing, and with --activate writes it. Inside a
	// session both are the session's file, so the keys of whoever the session
	// is running as would be carried onto the account being added.
	"ccdad add": "reads the live login to decide which machine-scoped keys to store, and with --activate writes it",

	// The worst split of the set. activateAPIKeyAccount writes primaryApiKey
	// into (CLAUDE_CONFIG_DIR ?? $HOME)/.claude.json -- which the DEFAULT
	// session mode deliberately SHARES with the live machine -- and then
	// clears the login from the session's credentials file. Half of it lands
	// on the machine permanently and half in a directory about to be deleted.
	"ccdad add-token": "would install an API key into the machine's global config while clearing only this session's login",

	// A session may be holding a refresh token the server has already rotated
	// to, and run's adopt-back is what carries it home. Deleting the account's
	// stored credentials first makes that write fail, at which point run keeps
	// the session directory rather than deleting it -- and the only live copy
	// of the token is stranded somewhere nothing looks.
	"ccdad remove": "would delete stored credentials this session may still have to hand a rotated token back to",

	// uninstall removes CCDAD_HOME, and the session's credential home is
	// INSIDE it. The Claude Code that is running right now would lose the file
	// it authenticates with, mid-session.
	"ccdad uninstall": "would delete the ccdad store this session's own credentials live in, out from under the running Claude Code",

	// autostart.go's rule 3, for the daemon a human asks for. daemon.ChildEnv
	// makes both path variables absolute and symlink-resolved before handing
	// them on, so starting one here does not merely leak the scope, it PINS
	// it: the daemon manages a session directory for the rest of its life, and
	// keeps managing it after `run` has deleted it.
	"ccdad daemon start":   "would leave a daemon managing this session's credentials for the rest of its life",
	"ccdad daemon restart": "would leave a daemon managing this session's credentials for the rest of its life",
	// The child's own entrypoint, refused for the same reason and reached a
	// different way: every path that spawns one is guarded above, so anything
	// arriving here was started by hand.
	"ccdad " + daemon.RunArg: "would run the daemon against this session's credentials rather than the live ones",

	// Both halves write Claude Code's OWN registration, and the two run modes
	// send it to different files. In the default mode
	// CLAUDE_SECURESTORAGE_CONFIG_DIR does not move .claude.json, so the entry
	// lands on the real machine, permanently, from inside a session that is
	// about to be deleted. Under --full-profile, CLAUDE_CONFIG_DIR DOES move
	// it, so the entry is buried in one account's profile and the machine never
	// sees it. The command would print success in either case.
	//
	// Refused per subcommand rather than per scope deliberately: --scope
	// project is genuinely safe in a session, and a per-scope rule is a rule
	// nobody can remember.
	"ccdad mcp install": "would write Claude Code's own MCP registration into a file this session either shares with the machine or hides from it",
	// The mirror, and the worse half: under --full-profile it would look inside
	// this session's profile, find nothing, and report a clean machine while
	// the real entry survived.
	"ccdad mcp uninstall": "would look for Claude Code's MCP registration in this session's copy of the config and report a clean machine while the real entry survives",
}

// scopedSessionAllowed is every other command in the tree, with the reason it
// is safe. It exists so that a command added later has NO verdict rather than
// a permissive one: the totality test fails until someone writes one down.
//
// This is autostart.go's rule 1 -- "a command added later defaults to safe
// rather than to spawning" -- reached the only way it can be here. The natural
// shape of this rule is a deny-list, because what makes a command dangerous is
// a property (does it write Claude Code's state) rather than a judgement about
// the command; an allow-list of the other twenty-odd would be twenty-odd
// entries all carrying the same sentence. The totality test buys the
// allow-list's safety without pretending each of these was a separate
// decision.
var scopedSessionAllowed = map[string]bool{
	// The reads. Their answer CHANGES inside a session -- `which` names the
	// session's account, `list` marks it as the current one -- and that answer
	// is the true one for the shell the question was asked in. doctor says so
	// out loud rather than leaving the reader to notice.
	"ccdad":        true,
	"ccdad list":   true,
	"ccdad status": true,
	"ccdad which":  true,
	"ccdad doctor": true,
	"ccdad export": true,

	// runway reads the usage cache and the recorded series, both of which live
	// under CCDAD_HOME. A session scopes Claude Code's credential and config
	// homes and nothing else, so the fleet it measures is the same fleet from
	// in here as from outside, and it writes nothing at all.
	"ccdad runway": true,

	// The terminal dashboard reads the same three documents `status` does,
	// through the same function, so its answer is the true one for the shell it
	// was asked in exactly as `list` and `which` are.
	//
	// It is allowed BECAUSE every key that changes something re-enters this
	// tree through a fresh root and inherits that command's own verdict: a
	// switch from the dashboard runs `ccdad switch`, which is refused above,
	// and the dashboard renders the refusal in that command's own words. If
	// that ever stops being true -- if a key calls an internal directly --
	// this entry flips to a refusal in the same commit.
	"ccdad tui": true,

	// run REPLACES the scope in the child it launches: newSession and
	// newProfile both set the child's variables outright, so a session inside
	// a session is two independent sessions.
	//
	// It does not follow that run ignores the scope in its OWN process, and an
	// earlier version of this comment claimed it did. seedProfile reads
	// ccpath.ConfigHome() at call time, so creating a profile from inside a
	// --full-profile session would have seeded it from the OUTER account's
	// profile — carrying that account's primaryApiKey into this one's. That is
	// refused in seedProfile, where the read happens, rather than by moving
	// the whole command here: the other three shapes of `ccdad run` are safe
	// in a session and useful in one.
	"ccdad run": true,

	// probe builds the same kind of session run does and sets the child's
	// variables outright, so a probe inside a session is an independent session.
	// It never seeds a profile, which is the one thing run does that reads the
	// scope it inherits, so the trap seedProfile refuses cannot be reached from
	// here at all.
	"ccdad probe": true,

	// These write only ccdad's own store, which CCDAD_HOME points at and a
	// session does not scope. import is the one worth naming: it sounds like
	// the most destructive of them and never installs a credential into Claude
	// Code at all -- its own help says so about mcpOAuth, and it is true of
	// every key.
	"ccdad import":       true,
	"ccdad bootstrap":    true,
	"ccdad disable":      true,
	"ccdad enable":       true,
	"ccdad own":          true,
	"ccdad alias":        true,
	"ccdad move":         true,
	"ccdad primary":      true,
	"ccdad config":       true,
	"ccdad config get":   true,
	"ccdad config set":   true,
	"ccdad config unset": true,
	"ccdad config list":  true,
	"ccdad config path":  true,

	// hover writes one key of config.toml and reads the usage cache and the
	// engine state, all of which live under CCDAD_HOME. A session scopes Claude
	// Code's credential and config homes and nothing else, so all three verbs
	// answer the same in here as outside -- which is why they share one verdict
	// rather than being split by whether they write.
	"ccdad hover": true,

	// setup-path registers the directory holding the running binary into the
	// user's shell configuration, or the Windows user environment. A session
	// scopes Claude Code's credential and config homes and nothing else, so
	// the answer here is the same inside one as outside it — and the binary
	// whose directory it registers is this same ccdad either way. Its
	// counterpart is `uninstall`, which IS refused, for a reason that has
	// nothing to do with PATH: it deletes the store the session's own
	// credentials live in.
	"ccdad setup-path": true,

	// update writes the ccdad BINARY, which is not scoped by a session: a
	// session scopes Claude Code's credential and config homes and nothing
	// else. The one part that would not be safe is the daemon restart at the
	// end, and that is skipped in here rather than the whole command being
	// refused — daemon.ChildEnv makes both path variables absolute and
	// symlink-resolved before handing them on, so a daemon spawned from inside
	// a session does not merely leak the scope, it PINS it.
	"ccdad update": true,

	// The daemon verbs that do not start one. All three act on the machine's
	// daemon through the pidfile under CCDAD_HOME, which is not scoped, so
	// they do exactly what they say from in here.
	"ccdad daemon":        true,
	"ccdad daemon status": true,
	"ccdad daemon stop":   true,
	"ccdad daemon logs":   true,

	// mcp only SERVES. Every tool it exposes re-enters this same tree through
	// a fresh root, so switch, auto and daemon start are refused PER CALL, out
	// of the map above, with the same clause a person would get.
	//
	// This verdict is allowed BECAUSE that re-entry exists. If a tool ever
	// calls an internal directly instead, the gate stops firing for it and this
	// entry flips to a refusal in the same commit. It is the same sentence
	// `ccdad tui` carries, for the same reason and about the same seam.
	"ccdad mcp": true,

	// The Codex commands write only ccdad's OWN store, which CCDAD_HOME points
	// at and a session does not scope. `ccdad add` is refused in here and this
	// one is not, and the difference is what each of them touches: that one can
	// activate a Claude Code login, which is exactly the state a session scopes
	// a copy of. This one stores an account ccdad serves through its own proxy
	// and never writes Claude Code's credentials, Claude Code's config or
	// codex's home.
	//
	// The parent carries a verdict of its own because the totality test walks
	// every node of the tree, and a group with no verdict is a group whose
	// subcommands were classified and whose own path was not.
	"ccdad codex":     true,
	"ccdad codex add": true,

	// Cobra's own. They read no state.
	"ccdad help":                  true,
	"ccdad completion":            true,
	"ccdad completion bash":       true,
	"ccdad completion zsh":        true,
	"ccdad completion fish":       true,
	"ccdad completion powershell": true,
}

// refuseInsideScopedSession is the gate, run from the root's
// PersistentPreRunE.
//
// The exit code is 2. The exit contract reserves it for usage errors, and
// this is one on the axis that matters: 2 is what tells a caller "you asked
// for something that cannot be done, and running it again will not help" as
// against 1's "something went wrong". `run`'s own refusals -- an API-key
// account, a cmd.exe shim -- are the same shape and already 2.
func refuseInsideScopedSession(cmd *cobra.Command) error {
	clause, refused := scopedSessionRefusals[cmd.CommandPath()]
	if !refused {
		return nil
	}
	session, ok := currentScopedSession()
	if !ok {
		return nil
	}
	return UsageError("`%s` cannot run inside a 'ccdad run' session: it %s.\n"+
		"This shell has %s.\n"+
		"Run it from a shell outside the session.",
		cmd.CommandPath(), clause, session.describe())
}
