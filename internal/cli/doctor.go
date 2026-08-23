package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Everything ccdad does is reverse-engineered against exactly one pinned Claude
// Code, and the risk register rates "Claude Code changes these internals
// between releases" High. doctor is the mitigation: the only thing that tells
// a user their Claude Code moved out from under ccdad BEFORE a switch destroys
// a credential.
//
// Two rules shape the whole file.
//
// The probe must not create what it probes. Bringing the daemon lock file into
// existence while checking for it destroys the one piece of genuine evidence
// that no daemon ever started here — and the same argument reaches the store
// directory, which is why nothing here calls store.Open: store.Open does an
// MkdirAll, so a diagnostic built on it would manufacture the very thing it was
// asked to report on. Every path below is stat-ed, and every reader used is one
// that returns "absent" rather than creating.
//
// And it reports; it does not repair. Whether doctor should attempt repairs or
// only report is still undecided, so there is deliberately no `--fix` and no
// code behind one. A repair added later has to be an explicit act by the user.
//
// The two checks this file once listed as absent — the leaked `ccdad run`
// session directories, and the stale `Claude Code-credentials` keychain item —
// are both here now, as checkSessions and checkKeychain. Neither changed the
// report-only rule: checkKeychain finds the item and prints the `security`
// command that removes it, rather than removing it — and says what running it
// costs, which is the half that was missing.

// checkLevel is how much a finding matters.
type checkLevel string

const (
	// levelOK is nothing to do.
	levelOK checkLevel = "ok"
	// levelWarn is worth knowing and not broken. An unrecognised credential key
	// is preserved by the deny-list; a fresh machine has no store yet.
	levelWarn checkLevel = "warn"
	// levelFail is something that will bite. It is the only level that changes
	// the exit code.
	levelFail checkLevel = "fail"
	// levelSkipped is a check that does not apply here — file modes on Windows,
	// where chmod is a no-op, or a check whose subject is missing.
	levelSkipped checkLevel = "skipped"
)

type check struct {
	Name   string
	Level  checkLevel
	Detail string
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the layout ccdad depends on, and the hazards around it",
		Long: "doctor reports; it never repairs, and it never creates anything it is\n" +
			"checking for. Exit 0 when nothing failed, 1 when something did — a\n" +
			"warning is not a failure.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runChecks()
			failed := false
			for _, c := range checks {
				if c.Level == levelFail {
					failed = true
				}
			}

			if asJSON {
				rows := make([]map[string]any, 0, len(checks))
				for _, c := range checks {
					rows = append(rows, map[string]any{
						"name": c.Name, "level": string(c.Level), "detail": c.Detail,
					})
				}
				if err := writeJSON(cmd, map[string]any{
					"schemaVersion": 1,
					"ok":            !failed,
					"checks":        rows,
				}); err != nil {
					return err
				}
			} else {
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				for _, c := range checks {
					fmt.Fprintf(w, "%s\t%s\t%s\n", c.Level, c.Name, c.Detail)
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			if failed {
				// The command has already said what is wrong, in its own words
				// and in the format the caller asked for.
				return WithCode(errSilent, ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// runChecks is the whole diagnostic, in a fixed order so two runs are diffable.
func runChecks() []check {
	root, rootErr := ccpath.StoreHome()
	if rootErr != nil {
		// Almost every other check derives its path from this one, so there is
		// nothing further to report and reporting each of them as "skipped"
		// would bury the single fact that explains all of them. The three that
		// do NOT derive from it — path, api-key and environment — are suppressed
		// here on the same argument rather than a different one: StoreHome fails
		// when no home directory can be resolved at all, which is the same
		// condition that leaves a binary off PATH and Claude Code's own config
		// unlocatable, so their answers would be three more symptoms above the
		// one cause. The fixed order the rest of this function keeps is a
		// property of a run that got that far.
		return []check{{"store", levelFail, rootErr.Error()}}
	}
	storeCheck, storeUsable := checkStore(root)

	live, liveErr := cclink.Load()
	// Read, never created: LoadGlobalConfig opens the file read-only and a
	// missing one resolves to an empty config. This file's rule is that the
	// probe must not CREATE what it probes, which is why store.Open is refused
	// three lines down and this is not.
	cfg, cfgErr := cclink.LoadGlobalConfig()
	report, probeErr := observeDaemon()

	return []check{
		storeCheck,
		checkPath(),
		checkPermissions(root, storeUsable),
		checkLocks(report, probeErr),
		checkPidfile(storeUsable),
		checkStatusFile(report),
		checkUsageCache(storeUsable),
		checkEngineState(storeUsable),
		checkConfig(storeUsable),
		checkSessions(root, storeUsable),
		checkProfiles(root, storeUsable),
		checkCredentialHome(report),
		checkClaudeCode(live, liveErr),
		checkCredentialKeys(live, liveErr),
		checkKeychain(),
		checkEnvironment(),
		checkAPIKey(cfg, cfgErr),
	}
}

// checkStore reports where ccdad's own state lives, and whether it is there.
//
// A missing store is a WARNING, not a failure, and the wording carries both
// readings on purpose. The daemon probe makes the same point one layer down: a
// fresh install and a mistyped CCDAD_HOME are indistinguishable at this layer —
// both are an *fs.PathError satisfying os.ErrNotExist — and making a new
// machine look broken is the worse of the two mistakes.
//
// A RELATIVE store is a failure, because store.Open refuses one outright: it
// would put a credentials tree in whatever directory ccdad happened to be run
// from, a different one each time, with live tokens in it.
func checkStore(root string) (check, bool) {
	if !filepath.IsAbs(root) {
		return check{"store", levelFail, fmt.Sprintf(
			"the store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)}, false
	}
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return check{"store", levelWarn, fmt.Sprintf(
			"%s does not exist — either ccdad has not run on this machine yet, or CCDAD_HOME points somewhere unintended", root)}, false
	case err != nil:
		return check{"store", levelFail, fmt.Sprintf("%s cannot be read: %v", root, err)}, false
	case !info.IsDir():
		return check{"store", levelFail, fmt.Sprintf("%s is not a directory", root)}, false
	}
	return check{"store", levelOK, root}, true
}

// checkPermissions holds the store to 0700 and everything in it to 0600.
//
// Windows gets no chmod and the ACL inherited from %USERPROFILE% is what
// protects the files there, so this is skipped rather than guessed at.
func checkPermissions(root string, usable bool) check {
	if runtime.GOOS == "windows" {
		return check{"permissions", levelSkipped, "file modes are not how Windows protects these; the %USERPROFILE% ACL is"}
	}
	if !usable {
		return check{"permissions", levelSkipped, "there is no store to check"}
	}

	var loose []string
	want := map[string]fs.FileMode{
		root:                               0o700,
		filepath.Join(root, "credentials"): 0o700,
	}
	for path, mode := range want {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			loose = append(loose, fmt.Sprintf("%s cannot be read (%v)", path, err))
			continue
		}
		if info.Mode().Perm() != mode {
			loose = append(loose, fmt.Sprintf("%s is %04o, want %04o", path, info.Mode().Perm(), mode))
		}
	}

	// Every file that holds a token, by glob rather than through the store: the
	// point is to see what is on disk, not what ccdad believes it wrote.
	//
	// The two names are store's and are spelled out here because store exports
	// no path accessors and opening it would create the tree. If they ever
	// change, TestDoctorReportsALooseCredentialFile fails on its own glob rather
	// than passing while checking nothing.
	files, _ := filepath.Glob(filepath.Join(root, "credentials", "*"))
	files = append(files, filepath.Join(root, "accounts.toml"))
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm() != 0o600 {
			loose = append(loose, fmt.Sprintf("%s is %04o, want 0600", path, info.Mode().Perm()))
		}
	}

	if len(loose) > 0 {
		sort.Strings(loose)
		return check{"permissions", levelFail, strings.Join(loose, "; ")}
	}
	// Named rather than "everything in it": this reads one level deep and skips
	// directories, so the per-session and per-profile homes `ccdad run` creates
	// are not covered here. checkSessions covers those.
	return check{"permissions", levelOK, "the store's own files and directories are 0700/0600"}
}

// checkLocks is the highest-value check here, and the easiest to get wrong.
//
// The question is not "is a daemon running". It is "do locks work on this
// filesystem at all" — ENOLCK on an NFS or CIFS mount with no lock daemon — and
// answering that with "no daemon" is precisely the `status || spawn` respawn
// loop the exit contract introduced exit 5 to prevent. A doctor that made that
// mistake would have reproduced clauth's bug inside the tool written to find
// it.
func checkLocks(report daemon.Report, probeErr error) check {
	if probeErr != nil {
		if errors.Is(probeErr, daemon.ErrLocksUnsupported) {
			return check{"locks", levelFail, fmt.Sprintf(
				"this filesystem cannot take a lock, so nothing here can tell whether a daemon is running: %v. "+
					"An NFS or CIFS mount with no lock daemon does this; move CCDAD_HOME onto local storage", probeErr)}
		}
		return check{"locks", levelFail, fmt.Sprintf("the singleton lock could not be probed: %v", probeErr)}
	}
	if report.State == daemon.DaemonRunning {
		return check{"locks", levelOK, "locking works here, and a daemon holds the singleton"}
	}
	return check{"locks", levelOK, "locking works here, and the singleton is free"}
}

// checkPidfile surfaces the one pidfile state the reader refuses to fold into
// "nothing to read": a body that IS committed and does not parse. pidfile.go
// names this command as the reader that needs to see it, because a supervisor
// that cannot tell "no daemon" from "this store is damaged" respawns forever.
func checkPidfile(usable bool) check {
	if !usable {
		return check{"pidfile", levelSkipped, "there is no store to check"}
	}
	pid, ok, err := daemon.ReadPID()
	switch {
	case err != nil:
		return check{"pidfile", levelFail, fmt.Sprintf("%s is damaged: %v", namePath(daemon.PIDPath()), err)}
	case !ok:
		return check{"pidfile", levelOK, "no pid recorded, which is what a stopped or never-started daemon leaves"}
	}
	// Deliberately not checked for liveness. The process may have died and the
	// number may have been recycled onto something unrelated; only the singleton
	// knows, and it was asked above.
	return check{"pidfile", levelOK, fmt.Sprintf("records pid %d (a recorded pid is never liveness evidence)", pid)}
}

func checkStatusFile(report daemon.Report) check {
	if report.StatusErr != nil {
		return check{"status-file", levelFail, fmt.Sprintf("%s cannot be read: %v", namePath(daemon.StatusPath()), report.StatusErr)}
	}
	if !report.HasStatus {
		return check{"status-file", levelOK, "no daemon has published one"}
	}
	return check{"status-file", levelOK, fmt.Sprintf("schema %d, generated %s",
		report.Status.SchemaVersion, report.Status.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"))}
}

// checkUsageCache is a warning rather than a failure by the cache's own design:
// an unreadable cache leaves every account UNKNOWN, which the engine already
// knows how to handle. It is invisible everywhere else, which is why LoadError
// exists at all.
func checkUsageCache(usable bool) check {
	if !usable {
		return check{"usage-cache", levelSkipped, "there is no store to check"}
	}
	c, err := usage.LoadCache()
	if err != nil {
		return check{"usage-cache", levelFail, err.Error()}
	}
	if cerr := c.LoadError(); cerr != nil {
		return check{"usage-cache", levelWarn, fmt.Sprintf(
			"%v — every account will read as unknown until it is rewritten", cerr)}
	}
	return check{"usage-cache", levelOK, namePath(usage.CachePath())}
}

// checkSessions reports the per-session credential directories `ccdad run`
// creates under the store.
//
// Each one holds a live refresh token at 0600. `run` deletes its own on the way
// out, but a machine powered off mid-session, a SIGKILL, or a run whose
// adopt-back failed all leave one — and nothing else in the tree would ever
// mention it again.
//
// It cannot tell a leftover from a session running RIGHT NOW, and does not
// pretend to: the tree's doctrine is that a recorded pid is not liveness
// evidence, and there is nothing else to read. So the text says both readings
// and the level stays a warning; deciding which one it is belongs to the human
// who knows whether they have a session open.
//
// Report-only, like every other check here, and for the reason the top of this
// file gives: there is no --fix, and deleting a directory that turns out to
// belong to a live session would take the credentials out from under it.
func checkSessions(root string, usable bool) check {
	if !usable {
		return check{"sessions", levelSkipped, "there is no store to check"}
	}
	container := filepath.Join(root, SessionsDirName)
	entries, err := os.ReadDir(container)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The container is created by `run`, never by this probe.
			return check{"sessions", levelOK, "no parallel-session credentials are left behind"}
		}
		return check{"sessions", levelFail, err.Error()}
	}

	var live []string
	for _, e := range entries {
		// Claude Code's legacy refresh lock is a directory named after the
		// session with ".lock" appended, created BESIDE it. It is not a
		// session, and counting it would double every number here.
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		live = append(live, e.Name())
	}
	if len(live) == 0 {
		return check{"sessions", levelOK, "no parallel-session credentials are left behind"}
	}
	sort.Strings(live)

	// The permissions check cannot see any of this: it reads one level deep and
	// skips directories, so a world-readable token inside a session would be
	// reported as "ok" by the check whose whole job is modes.
	if loose := looseSessions(container, live); len(loose) > 0 {
		return check{"sessions", levelFail, strings.Join(loose, "; ")}
	}
	return check{"sessions", levelWarn, fmt.Sprintf(
		"%d session credential director%s under %s (%s). A running 'ccdad run' owns one each; "+
			"any others hold a refresh token nothing is using, and are safe to delete once no session is open",
		len(live), plural(len(live), "y", "ies"), container, strings.Join(live, ", "))}
}

// looseSessions names every session directory or credential file that anyone
// but the owner can read.
//
// Windows has no mode bits and the inherited profile ACL is what protects
// these, so there is nothing here to answer.
func looseSessions(container string, names []string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	var loose []string
	for _, name := range names {
		dir := filepath.Join(container, name)
		want := map[string]fs.FileMode{
			dir: 0o700,
			filepath.Join(dir, ccpath.CredentialsFile): 0o600,
		}
		for path, mode := range want {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Mode().Perm() != mode {
				loose = append(loose, fmt.Sprintf("%s is %04o, want %04o", path, info.Mode().Perm(), mode))
			}
		}
	}
	sort.Strings(loose)
	return loose
}

// plural picks a suffix. It exists because "1 directories" in a diagnostic is
// the kind of thing a reader stops trusting the rest of the sentence over.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// checkEngineState is the anti-flap state: cooldown, last switch, quarantines.
// Unreadable, it degrades towards MORE switching rather than less, so it is a
// warning — but a permanently unreadable one means the cooldown never applies
// and nothing else would ever say so.
func checkEngineState(usable bool) check {
	if !usable {
		return check{"engine-state", levelSkipped, "there is no store to check"}
	}
	st, err := strategy.LoadState()
	if err != nil {
		return check{"engine-state", levelFail, err.Error()}
	}
	if serr := st.LoadError(); serr != nil {
		return check{"engine-state", levelWarn, fmt.Sprintf(
			"%v — the cooldown and every quarantine are being ignored until it is rewritten", serr)}
	}
	return check{"engine-state", levelOK, namePath(strategy.StatePath())}
}

// checkConfig answers whether ~/.ccdad/config.toml is doing anything.
//
// This is the one check whose subject fails SILENTLY by design:
// config.Reloader keeps the daemon on the last config that parsed when an edit
// breaks the file, which is right — a daemon that dies on a typo stops
// switching accounts — and it means a user can edit a threshold, see no error
// anywhere, and have nothing take effect. Here is where they find out.
//
// A missing file is OK rather than a warning: the defaults are a complete
// configuration and most machines never need one. An unusable file is a warning
// rather than a failure for the same reason — ccdad goes on working, on numbers
// the user did not choose.
func checkConfig(usable bool) check {
	if !usable {
		return check{"config", levelSkipped, "there is no store to check"}
	}
	path, perr := config.Path()
	if perr != nil {
		return check{"config", levelFail, perr.Error()}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return check{"config", levelOK, fmt.Sprintf(
			"no %s, so the engine runs on the built-in defaults", config.FileName)}
	}
	cfg, err := config.Load()
	if err != nil {
		return check{"config", levelWarn, fmt.Sprintf(
			"%v — every value in it is being ignored and the engine is running on its defaults", err)}
	}
	detail := path
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if unknown, uerr := config.UnknownKeys(raw); uerr == nil && len(unknown) > 0 {
			return check{"config", levelWarn, fmt.Sprintf(
				"%s carries keys this ccdad does not know, which are preserved but ignored: %v", path, unknown)}
		}
	}
	if cfg.MaxAutoSpend > 0 {
		// Unattended spending is on. The risk register rates it High, and
		// doctor is where a user checks what their machine will do without them.
		detail = fmt.Sprintf("%s — unattended credit spending is armed up to %v", path, cfg.MaxAutoSpend)
	}
	return check{"config", levelOK, detail}
}

// checkCredentialHome answers the question no other check can: is something
// ELSE driving the Claude Code login this ccdad manages.
//
// The daemon singleton is keyed on the STORE, so two shells with different
// CCDAD_HOME values each take their own and each write the same
// .credentials.json. cclock serialises the writes, so nothing is corrupt and
// nothing errors — the two engines simply undo each other's switches, and until
// this check existed there was no line anywhere in the tree that said so.
//
// It reports, like everything else here, and it creates nothing: credhome.Probe
// takes a SHARED try-lock with O_CREATE removed, so a machine where no engine
// has ever run still has no lock file after `ccdad doctor`.
//
// It asserts no file modes. checkPermissions skips them on Windows because the
// inherited profile ACL is what protects these files there, and a second check
// contradicting that in the same output is worse than a gap.
func checkCredentialHome(report daemon.Report) check {
	claim, err := credentialHomeClaim()
	switch {
	case errors.Is(err, credhome.ErrLocksUnsupported):
		// The same condition checkLocks names, on the OTHER filesystem. A local
		// store with a network ~/.claude reaches this one with the singleton
		// already taken, so the two are genuinely independent answers and the
		// wording has to say which filesystem is meant.
		return check{"credential-home", levelFail, fmt.Sprintf(
			"the filesystem holding Claude Code's credential home cannot take a lock, so ccdad cannot tell "+
				"whether a second store is driving this login: %v. An NFS or CIFS mount with no lock daemon "+
				"does this; the engine keeps running, but unguarded", err)}
	case err != nil:
		return check{"credential-home", levelFail, fmt.Sprintf(
			"the credential-home claim could not be probed: %v", err)}
	}

	detail := claim.Home
	if drift := credentialHomeDrift(report, claim.Home); drift != "" {
		return check{"credential-home", levelWarn, drift}
	}
	switch {
	case !claim.Held:
		return check{"credential-home", levelOK, detail + " — no ccdad engine is driving it"}
	case claim.Ours:
		return check{"credential-home", levelOK, fmt.Sprintf(
			"%s — driven by this store's engine (pid %d)", detail, claim.Owner.PID)}
	case !claim.Named:
		// Held is a kernel fact and the name is not, so this is a warning about
		// the NAME rather than about the claim. It is the transient state of an
		// engine that has taken the claim and not yet written its document.
		return check{"credential-home", levelWarn, fmt.Sprintf(
			"%s is held by an engine that has not named itself (%v); if it persists, something is "+
				"holding the claim and cannot write beside it", detail, claim.OwnerErr)}
	}
	return check{"credential-home", levelWarn, fmt.Sprintf(
		"%s is driven by the ccdad store at %s (pid %d), not by this one. Two stores on one login undo "+
			"each other's switches; point CLAUDE_CONFIG_DIR at a directory of this store's own, or stop "+
			"that engine", detail, claim.Owner.Store, claim.Owner.PID)}
}

// credentialHomeDrift catches a running daemon that is managing a DIFFERENT
// credential home from the one this shell resolves, and returns the sentence
// for it or "".
//
// `ccdad run --full-profile` is how this happens without anybody doing anything
// wrong: it points CLAUDE_CONFIG_DIR at a per-session directory and unsets
// CLAUDE_SECURESTORAGE_CONFIG_DIR, so auto-start's scoped-credential refusal
// does not fire, and a daemon started from inside such a session manages that
// session's directory for the rest of its life. Every file on the machine looks
// normal afterwards. The daemon's own published document is the only place the
// two homes can be compared.
func credentialHomeDrift(report daemon.Report, resolved string) string {
	if report.State != daemon.DaemonRunning || !report.HasStatus {
		return ""
	}
	recorded := report.Status.CredentialHome
	// credhome.SamePath, never ==. ccdad manufactures the two spellings itself:
	// daemon.ChildEnv pins an absolute, symlink-resolved CLAUDE_CONFIG_DIR into
	// every daemon it spawns, while ccpath hands this shell's own spelling back
	// untouched. A trailing slash is enough — filepath.Abs cleans it for the
	// child and nothing cleans it here — and a string compare would then print
	// this warning on every doctor run forever, telling the user to restart a
	// daemon that is driving exactly the right directory.
	if recorded == "" || credhome.SamePath(recorded, resolved) {
		return ""
	}
	return fmt.Sprintf(
		"the running daemon is driving %s, but this shell resolves %s — so its switches change a login "+
			"nothing here reads. A daemon started from inside 'ccdad run --full-profile' does this; "+
			"restart it from an ordinary shell", recorded, resolved)
}

// checkClaudeCode is the risk register's actual mitigation: has Claude Code's
// layout moved.
//
// cclink.Load's refusals ARE the drift signals — a symlink at the path, a file
// over the 1 MiB cap, a body that is not JSON — and switch deliberately cannot
// repair the last of those, because overwriting destroys the machine-scoped keys
// still in it. So this is the place a user finds out.
func checkClaudeCode(live cclink.Blob, err error) check {
	// Resolved independently of the store root: CCDAD_HOME can be set while
	// HOME is not, which resolves ccdad's own tree and leaves Claude Code's
	// unlocatable — a real drift state, and one only this check can name.
	home, homeErr := ccpath.CredentialHome()
	if homeErr != nil {
		return check{"claude-code", levelFail, homeErr.Error()}
	}
	path := filepath.Join(home, ccpath.CredentialsFile)
	// Inside a `ccdad run` session this file is the SESSION's, and every
	// sentence below would otherwise describe it as the machine's live login.
	// Measured before this line existed: a user who ran doctor to find out why
	// a switch had done nothing was told that the switch's own file read fine.
	scope := scopedSessionNote()
	if err != nil {
		return check{"claude-code", levelFail, fmt.Sprintf("%s: %v%s", path, err, scope)}
	}
	if len(live) == 0 {
		return check{"claude-code", levelWarn, fmt.Sprintf(
			"no login in %s — Claude Code has not logged in on this machine, or it keeps its credentials elsewhere%s", home, scope)}
	}
	return check{"claude-code", levelOK, fmt.Sprintf("%s reads as %d top-level keys%s", path, len(live), scope)}
}

// scopedSessionNote is the clause every credential answer in this report needs
// when ccdad is running inside one of its own `ccdad run` sessions: without
// it, each check describes the session's copy of Claude Code's state as though
// it were the machine's.
//
// It says "session" rather than repeating the whole explanation in each check
// — checkEnvironment carries the long form, and the two are read together.
func scopedSessionNote() string {
	session, ok := currentScopedSession()
	if !ok {
		return ""
	}
	return fmt.Sprintf(" — but this shell is inside a `ccdad run` session (%s), so that is the session's own login and not the machine's live one",
		session.describe())
}

// checkCredentialKeys is the unknown-key probe's "on startup" half, and the
// last part of that probe that was still missing.
//
// Unrecognised keys are a warning, not a failure: the swap is a DENY-list, so
// anything ccdad has never heard of is preserved rather than destroyed. What
// this is telling the user is that ccdad is behind Claude Code — six machine
// keys drifted in after clauth's one-key carry list was written, which is why
// this is demonstrated drift rather than a hypothetical.
func checkCredentialKeys(live cclink.Blob, err error) check {
	if err != nil {
		return check{"credential-keys", levelSkipped, "the credentials file could not be read"}
	}
	unknown := cclink.UnknownKeys(live)
	if len(unknown) == 0 {
		return check{"credential-keys", levelOK, "every top-level key is one ccdad knows"}
	}
	sort.Strings(unknown)
	return check{"credential-keys", levelWarn, fmt.Sprintf(
		"unrecognised top-level keys: %s. They are preserved on a switch, but an ACCOUNT-scoped one would leak between accounts until ccdad is updated",
		strings.Join(unknown, ", "))}
}

// keychainProbe is doctor's window onto the legacy macOS Keychain, and it is a
// var so the tests can describe a machine that has an item on it. Without the
// seam every branch below except "skipped" would be unreachable from a Linux
// development machine, and the branch that matters — a stale item found — would
// ship having never been rendered.
var keychainProbe = cclink.ProbeCredentialKeychainItem

// keychainDetailer is an error that already knows how to explain itself to a
// user. cclink.KeychainError is the one that does; matching on the BEHAVIOUR
// rather than on that concrete type is what lets a test describe a locked
// keychain without cclink having to export the classification it keeps private.
type keychainDetailer interface{ Detail() string }

// checkKeychain looks for the credential item a Keychain-era Claude Code would
// have written, which a DOWNGRADED Claude Code would still read in preference
// to the file ccdad writes. The risk register rates "Claude Code changes these
// internals between releases" High and names doctor as the mitigation; this is
// the half of that mitigation which points backwards rather than forwards.
//
// It is the one check here that runs another program, and two properties keep
// that honest. The lookup asks for the item's ATTRIBUTES and never its secret,
// so it cannot raise the "wants to use your keychain" dialog on anybody's
// machine; and every spawn has a wall-clock deadline with a kill behind it,
// because a locked keychain on a headless host does not fail, it waits.
//
// Report-only, like the rest of the file, and now for a measured reason rather
// than a cautious one. cclink's keychain header records the read order: 2.1.112
// and earlier read the keychain and fall back to the file, so deleting the item
// is a REPAIR and not a logout. What deleting still costs is the login in the
// item, which on such a machine is the live one — so the act needs a user who
// knows that, which is what the detail below now says. Whether doctor should
// ever repair rather than report is still undecided, and doing this inside a
// switch would put a `security` spawn in the credential-lock window on every
// macOS switch to serve a population that shrinks with every release.
func checkKeychain() check {
	found, err := keychainProbe(context.Background())
	item := found.Item

	if errors.Is(err, cclink.ErrKeychainUnsupported) {
		return check{"keychain", levelSkipped, "there is no macOS Keychain on this platform"}
	}
	if err != nil {
		// A probe that could not answer is NOT an absence. Saying "no stale
		// item" because a locked keychain refused to be read is the same
		// mistake as reporting "no daemon" for a filesystem where locks do not
		// work, one check up.
		var explained keychainDetailer
		if errors.As(err, &explained) {
			return check{"keychain", levelWarn, fmt.Sprintf(
				"ccdad could not check for a stale %s item: %s", item.Service, explained.Detail())}
		}
		return check{"keychain", levelWarn, fmt.Sprintf(
			"ccdad could not check for a stale %s item: %v", item.Service, err)}
	}
	if !found.Present {
		return check{"keychain", levelOK, fmt.Sprintf(
			"no legacy item for %q under %s", item.Account, keychainNameList(found.Checked))}
	}
	// A warning rather than a failure, and deliberately: 2.1.113 removed the
	// backend, so on any Claude Code a user can install today nothing is broken
	// right now. What it costs is a downgrade — or a pin to 2.1.112 or earlier,
	// which is the same machine seen from the other side.
	//
	// The remedy carries its price now. The read order settles that deleting the
	// item hands a keychain-era Claude Code back to the credentials file rather
	// than logging it out — that much is safe — but the login INSIDE the item
	// goes with it, and on a machine still running 2.1.112 or earlier that item
	// is the live login rather than a leftover. Printing the command without
	// that sentence was handing someone a way to lose an account ccdad may not
	// hold.
	return check{"keychain", levelWarn, fmt.Sprintf(
		"a legacy keychain item %q for %q is still there. Claude Code 2.1.112 and earlier read this item BEFORE the credentials file, so such a build reads it INSTEAD of what ccdad writes and every switch silently does nothing; 2.1.113 removed that backend, so a current Claude Code ignores the item entirely. WHICH ONE YOU ARE ON DECIDES THE REMEDY. On 2.1.113 or later, removing the item is cleanup — nothing recreates it, and it stops a future downgrade from shadowing ccdad. On 2.1.112 or earlier, removing it is NOT a fix: that item is the live login, and the next credential write recreates it and deletes the credentials file with it, so upgrade Claude Code instead. Remove it with: /usr/bin/security delete-generic-password -a %q -s %q",
		item.Service, item.Account, item.Account, item.Service)}
}

// keychainNameList renders the names a probe looked for. It is a list rather
// than one name because a decomposed CLAUDE_CONFIG_DIR splits the item into two
// spellings, and an "ok" that named only one of them would be the same
// half-answer this check exists to stop giving.
func keychainNameList(items []cclink.KeychainItem) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, strconv.Quote(item.Service))
	}
	return strings.Join(names, " or ")
}

// checkEnvironment names the variables that make a switch pointless.
//
// Claude Code prefers CLAUDE_CODE_OAUTH_TOKEN over the stored login outright, so
// with it set a switch writes a credentials file nothing reads. The list is the
// four ccdad's own `displacingAuth` already names, minus the apiKeyHelper
// setting, which is not a variable:
//
//   - ANTHROPIC_AUTH_TOKEN, which the bundle carries beside CLAUDE_CODE_OAUTH_TOKEN
//     in the list it attributes a session's credential to ("from
//     ANTHROPIC_AUTH_TOKEN", "from CLAUDE_CODE_OAUTH_TOKEN").
//   - ANTHROPIC_API_KEY.
//   - CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, which Claude Code reports under the
//     SAME source name as ANTHROPIC_API_KEY, which is why identity's
//     DisplacesOAuth puts it on that side too.
//
// The two this check does NOT name are files rather than variables — the
// apiKeyHelper setting and primaryApiKey in ~/.claude.json — and which source
// actually wins is a resolution rather than a list. checkAPIKey answers that,
// and the two are read together. Reporting a variable here that the resolver
// then reports as not winning is deliberate: this check answers "is anything
// set that could defeat a switch", which is the question a user brings.
//
// switch.go warns about CLAUDE_CODE_OAUTH_TOKEN when the user tries, and only
// about that one — the fuller list is `displacingAuth`, which runs AFTER an
// API-key activation. For the other three, this is the only place that says so
// before the switch rather than after it.
//
// The VALUES are never printed. A diagnostic is the output a user pastes into an
// issue, and every one of these is a live credential.
func checkEnvironment() check {
	var hazards []string
	for _, name := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	} {
		if os.Getenv(name) != "" {
			hazards = append(hazards, name)
		}
	}

	// The path overrides are not hazards; they are just worth seeing, because
	// they decide which files everything above was talking about.
	var paths []string
	for _, name := range []string{"CCDAD_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			paths = append(paths, fmt.Sprintf("%s=%s", name, v))
		}
	}
	suffix := ""
	if len(paths) > 0 {
		suffix = ". Set: " + strings.Join(paths, ", ")
	}

	// A path override pointing into ccdad's OWN sessions or profiles container
	// is not merely worth seeing: it means every answer above described a
	// `ccdad run` session rather than the machine. Listing the variable
	// without saying so — which is what this check did — is how the override
	// stayed invisible in the report that exists to surface it.
	if session, inSession := currentScopedSession(); inSession {
		return check{"environment", levelWarn, fmt.Sprintf(
			"this shell is inside a `ccdad run` session: %s. Every answer above describes that session, "+
				"not the live login, and the commands that would write Claude Code's own state refuse in here%s",
			session.describe(), suffix)}
	}

	if len(hazards) > 0 {
		return check{"environment", levelWarn, fmt.Sprintf(
			"%s set — Claude Code reads these instead of the credentials file, so a switch would have no effect%s",
			joinAnd(hazards), suffix)}
	}
	return check{"environment", levelOK, "nothing set that would make a switch a no-op" + suffix}
}

// joinAnd is "a", "a and b", "a, b and c". It exists because checkEnvironment
// now names four variables and `strings.Join(…, " and ")` reads as a chant once
// there are more than two of them.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// checkAPIKey answers which API key Claude Code would actually use, and whether
// that displaces the login ccdad manages.
//
// checkEnvironment names the variables a user can see. This one runs identity's
// resolver, because two of the five sources it models are not visible from the
// environment at all — the apiKeyHelper setting, and primaryApiKey in
// ~/.claude.json.
//
// Listing those five as hazards would be the wrong check, and this is the
// reason the item that asked for one is not implemented that way. primaryApiKey
// is the single source that does NOT displace an OAuth login — identity's
// DisplacesOAuth reads `BE()`, which turns the gate off for the first five
// sources and not for the stored key — and `ccdad switch` WRITES it for every
// api-key account. A check that warned on it would warn on ccdad's own steady
// state, on the machine of every user who has an api-key account, forever. What
// matters is which source WINS, and whether that win makes a switch a no-op.
//
// It creates nothing, which is what lets it read two files the rest of this
// command does not: LoadGlobalConfig opens read-only and resolves a missing
// file to an empty config, and the helper probe is an os.ReadFile that skips
// what it cannot read.
//
// The key is never printed, for checkEnvironment's reason.
func checkAPIKey(cfg *cclink.GlobalConfig, cfgErr error) check {
	// cfg is nil when the read failed, and claudeAPIKeyEnvironment takes that:
	// the approval list and the stored key are simply absent.
	env := claudeAPIKeyEnvironment(cfg)
	_, source := env.Resolve()

	if cfgErr != nil {
		// Nothing else in this command reads ~/.claude.json, so an unreadable
		// one would otherwise be invisible in the report. It is a warning
		// rather than a failure because ccdad goes on working.
		//
		// The question this check exists for stays ANSWERABLE without the file,
		// and saying "cannot tell" for both halves would be the weaker answer.
		// Every displacing source is visible from the environment and the
		// settings — and the one source the config carries, the stored key, is
		// the one that never displaces. So whether a switch survives is
		// certain; only WHICH source wins is not, because the approval list
		// that decides between them lives in the file that could not be read.
		if env.EnvKey != "" || env.FileDescriptorKey || env.Helper {
			return check{"api-key", levelWarn, fmt.Sprintf(
				"a key from the environment or an apiKeyHelper resolves ahead of the login, so a switch "+
					"would write a file nothing reads. Which of them wins cannot be said, because %v", cfgErr)}
		}
		return check{"api-key", levelWarn, fmt.Sprintf(
			"nothing that displaces a login is set, so a switch takes effect — but %v, so ccdad cannot "+
				"say whether a key is stored in it", cfgErr)}
	}

	// The one state where the answer depends on how Claude Code is started, and
	// it is reported rather than resolved: claudeAPIKeyEnvironment hard-codes
	// Interactive because ccdad is being asked about a session that has not
	// started yet, and picking one of the two answers would be wrong half the
	// time. noteEnvKeyApproval says the same thing at the moment of a switch.
	approval := ""
	if env.EnvKeyNeedsApproval() {
		approval = ". ANTHROPIC_API_KEY is set but is not in Claude Code's approved list, so an interactive " +
			"session refuses it while `claude -p` uses it outright — the answer above is the interactive one"
	}

	switch {
	case source == identity.APIKeyNone:
		return check{"api-key", levelOK,
			"no API key resolves, so Claude Code authenticates with the login in the credentials file" + approval}
	case source.DisplacesOAuth():
		return check{"api-key", levelWarn, fmt.Sprintf(
			"Claude Code would take its key %s, and a key from there makes it ignore the credentials file "+
				"entirely — so a switch would write a login nothing reads%s", "from "+source.String(), approval)}
	}
	// APIKeyManaged: the source ccdad itself writes for an api-key account.
	// Saying so is the point rather than staying silent — it is the state a
	// working api-key account leaves behind, and a user who found it in
	// ~/.claude.json needs to be told it is not the thing breaking their switch.
	return check{"api-key", levelOK, fmt.Sprintf(
		"Claude Code would use %s, which does NOT displace a login — a switch still takes effect%s",
		source.String(), approval)}
}

// checkProfiles reports a `ccdad run --full-profile` profile whose account is
// gone.
//
// <store>/profiles/<uuid> is not scratch space: `ccdad run --full-profile`
// writes an API-key account's primaryApiKey into that profile's own Claude Code
// config, so an orphan is a credential nothing else on this machine would ever
// mention again. `ccdad remove` deletes the profile and `uninstall` takes the
// whole store, so the path that CREATES orphans going forward is closed. What
// is left is a store restored from an export, an account removed by an older
// ccdad, or a uuid that changed.
//
// It is a SET DIFFERENCE and not a count, and that is the whole design. A
// profile whose account still exists is legitimate persistent state and is not
// reported at all; a report that counted directories would tell every user of
// --full-profile that their working setup is a problem.
//
// The account list comes from store.AccountsAt, never store.Open: Open does an
// MkdirAll and this file's rule is that the probe must not create what it
// probes. TestDoctorCreatesNothing is the test that keeps it that way.
func checkProfiles(root string, usable bool) check {
	if !usable {
		return check{"profiles", levelSkipped, "there is no store to check"}
	}
	container := filepath.Join(root, ProfilesDirName)
	entries, err := os.ReadDir(container)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The container is created by `run`, never by this probe.
			return check{"profiles", levelOK, "no --full-profile profiles are stored here"}
		}
		return check{"profiles", levelFail, err.Error()}
	}

	accounts, err := store.AccountsAt(root)
	if err != nil {
		// Without the account list there is no set to difference against, and
		// answering "no orphans" from a failed read would be the lie this check
		// exists to remove.
		return check{"profiles", levelFail, fmt.Sprintf(
			"the profiles under %s cannot be matched against the account list: %v", container, err)}
	}
	known := make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		known[a.UUID] = struct{}{}
	}

	var orphans []string
	for _, e := range entries {
		// checkSessions's rule, for the same reason: Claude Code's legacy
		// refresh lock is a directory named after its neighbour with ".lock"
		// appended, and counting one as a profile would report an orphan for
		// every profile in daily use.
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		if _, ok := known[e.Name()]; ok {
			continue
		}
		orphans = append(orphans, e.Name())
	}
	if len(orphans) == 0 {
		return check{"profiles", levelOK, "every profile here belongs to an account this store still has"}
	}
	sort.Strings(orphans)

	// A warning, not a failure: nothing is broken by an orphan sitting there.
	// What it costs is a stored API key with no account left to name it, which
	// is worth a sentence and is not something to delete on ccdad's initiative
	// — whether doctor should ever repair is undecided, and this file reports.
	return check{"profiles", levelWarn, fmt.Sprintf(
		"%d profile director%s under %s belong%s to no account this store has (%s). Each may hold that "+
			"account's API key in its own Claude Code config; `ccdad remove` no longer leaves these, so they "+
			"are from an older ccdad, a restored export, or a uuid that changed",
		len(orphans), plural(len(orphans), "y", "ies"), container,
		plural(len(orphans), "s", ""), strings.Join(orphans, ", "))}
}

// checkPath answers whether typing `ccdad` in a new shell finds this binary.
//
// It reads TWO facts, because in the ordinary case they disagree: `ccdad
// setup-path` writes a block into a startup file, and the PATH of the process
// running doctor right now does not show it until a new shell has read that
// file. Which of the two it is decides whether the advice is "open a new shell"
// or "run a command", so reporting either alone would send half of the users
// who need it to the wrong remedy.
//
// It is never a failure, and that is a judgement about doctor's taxonomy rather
// than a convenience: ccdad invoked by its absolute path works exactly as well,
// so this does not meet this file's bar for levelFail ("something that will
// bite"). It is also what lets the shared doctor and `--json` contract
// fixtures stay as they are — their binary lives in a t.TempDir() that is by
// construction not on PATH, and a warning moves neither the exit code nor `ok`.
//
// It creates nothing: os.Executable, the live PATH, and a read of the startup
// files setup-path would have written.
func checkPath() check {
	exe, err := executablePath()
	if err != nil {
		return check{"path", levelWarn, fmt.Sprintf(
			"ccdad cannot locate its own binary, so it cannot tell whether it is on PATH: %v", err)}
	}
	dir := filepath.Dir(exe)

	// onPathList, not a hand-rolled split: it is setup-path's own decider, it
	// matches whole components rather than substrings, and it is already tested
	// against both platforms' rules.
	if onPathList(os.Getenv("PATH"), dir, livePathRules) {
		return check{"path", levelOK, fmt.Sprintf("%s is on PATH", dir)}
	}

	places, perr := pathRegistrations(dir)
	if perr != nil {
		// A scan that could not finish is not evidence that nothing is
		// registered — the same rule uninstall applies to the same reader.
		return check{"path", levelWarn, fmt.Sprintf(
			"%s is not on this shell's PATH, and the startup files that would say otherwise could not be "+
				"read: %v", dir, perr)}
	}
	if len(places) > 0 {
		return check{"path", levelWarn, fmt.Sprintf(
			"%s is not on THIS shell's PATH, but ccdad has registered it in %s — open a new shell, or "+
				"source that file",
			dir, joinAnd(places))}
	}
	return check{"path", levelWarn, fmt.Sprintf(
		"%s is not on PATH, so `ccdad` only works by its full path. `%s setup-path` adds it", dir, exe)}
}
