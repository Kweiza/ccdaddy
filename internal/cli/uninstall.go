package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// §11.2 fix 6 points BOTH installers at this command instead of `rm <binary>`,
// precisely because there is a daemon holding a lock, a token directory and
// possibly an MCP registration to unwire — so shipping the installers without
// it publishes a dead pointer.
//
// It is the most destructive command in the tree, and the shape it takes from
// that is remove.go's: enumerate, refuse without an explicit yes, and treat a
// non-interactive caller with no --yes as a usage error rather than as consent.
// What it deletes is every managed account's OAuth REFRESH TOKEN, and those are
// not recoverable — a user who wanted the daemon gone and their logins kept has
// no way back afterwards.
//
// What it deliberately does NOT delete is Claude Code's own credentials file.
// Uninstalling ccdad is not logging out, and the account the user is left on is
// named on the way out, because every other one has just become unreachable.

// accountsFileName and credentialsDirName are internal/store's, spelled out
// here because store exports no path accessor and its Open does an MkdirAll — a
// command that must never create what it is about to delete cannot go through
// it to ask. doctor.go spells the same two for the same reason, and the tests
// below fail on their own spelling rather than passing while checking nothing.
const (
	accountsFileName   = "accounts.toml"
	credentialsDirName = "credentials"
)

// storeMarkers are the top-level entries only ccdad puts in its store. Every
// one that HAS an owning package is taken from that package's exported
// basename, so a rename there cannot silently make this stop recognising its
// own store. The names are used rather than the path accessors deliberately:
// this question is about basenames, and the accessors resolve a home directory
// that a command deciding "is this a ccdad store" already has in hand.
// countSessions is how many per-session credential homes the store holds. A
// missing container is none, and this never creates one.
func countSessions(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, SessionsDirName))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		// The `.lock` siblings Claude Code leaves are not sessions.
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".lock") {
			n++
		}
	}
	return n
}

func storeMarkers() []string {
	return []string{
		accountsFileName,
		credentialsDirName,
		daemon.LockFileName,
		daemon.PIDFileName,
		daemon.StatusFileName,
		daemon.LogFileName,
		usage.CacheFileName,
		strategy.StateFileName,
		// `ccdad run` puts these here, and a store whose accounts have all been
		// removed can still hold one. Without them such a store is refused as
		// "not a ccdad store", leaving a directory nothing else cleans up.
		SessionsDirName,
		ProfilesDirName,
	}
}

var (
	// executablePath is a seam so a test of "delete the binary" does not delete
	// the test binary.
	executablePath = os.Executable

	// unwireMCP removes ccdad's registration from Claude Code, and finds
	// nothing today: `ccdad mcp install` is deferred out of this queue, so
	// there is no registration to remove and no file whose shape could be
	// guessed at without inventing the contract that task has to follow. It is
	// a hook rather than a stub — when the registration lands, its removal goes
	// HERE, and the caller below already treats a failure as a warning rather
	// than as a reason to leave a half-uninstalled machine behind.
	unwireMCP = func() (removed bool, err error) { return false, nil }
)

func newUninstallCmd() *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the daemon, delete ccdad's store, and remove the binary",
		Long: "uninstall deletes the stored OAuth refresh token of EVERY managed account.\n" +
			"They cannot be recovered; the accounts have to be logged in again.\n\n" +
			"Claude Code's own credentials file is left exactly as it is — uninstalling\n" +
			"ccdad is not logging out — so you stay logged in as whichever account is\n" +
			"live, and that account is named on the way out.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUninstall(cmd, assumeYes)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not prompt for confirmation")
	return cmd
}

func runUninstall(cmd *cobra.Command, assumeYes bool) error {
	out := cmd.ErrOrStderr()

	root, err := ccpath.StoreHome()
	if err != nil {
		return err
	}
	storeState, err := inspectStore(root)
	if err != nil {
		return err
	}

	exe, exeErr := executablePath()
	owner := ""
	if exeErr == nil {
		owner = packageManagerOwning(exe)
	}
	// A binary this command may not delete is not a binary to count as
	// something to do.
	removableBinary := exeErr == nil && owner == ""

	// What `ccdad setup-path` registered, if anything. Read before the prompt
	// and before anything is deleted, for the same reason the accounts are: a
	// user is being asked to confirm, and what they are confirming has to be on
	// the screen. A read that fails — an rc file with a half-deleted fence — is
	// reported and does not stop the uninstall, because a machine that cannot
	// be tidied is still a machine the user asked to have ccdad removed from.
	pathDir := ""
	if exeErr == nil {
		pathDir = filepath.Dir(exe)
	}
	pathPlaces, pathErr := pathRegistrations(pathDir)

	if pathErr != nil {
		fmt.Fprintf(out, "Some startup files could not be inspected for a ccdad PATH entry: %v\n", pathErr)
	}
	// A scan that could not finish is not evidence of an empty machine, so it
	// does not get to produce "nothing to uninstall" and exit 3 — that answer
	// tells a script the machine is clean while a live block still names a
	// binary about to be deleted.
	if !storeState.present && !removableBinary && len(pathPlaces) == 0 && pathErr == nil {
		fmt.Fprintln(out, "Nothing to uninstall: there is no ccdad store here"+describeBinary(exe, exeErr, owner)+".")
		return WithCode(errSilent, ExitNothingToDo)
	}

	// Attribute BEFORE anything is deleted: afterwards the stored credentials
	// are gone and the question can no longer be answered.
	live, _ := cclink.Load()
	current, hasLive := attributeLive(live, storeState.accounts, storeState.lookup)

	enumerate(out, storeState, exe, exeErr, owner, current, hasLive)
	for _, place := range pathPlaces {
		fmt.Fprintf(out, "It will also remove ccdad's PATH entry from %s.\n", place)
	}

	if !assumeYes {
		if !stdinIsTTY() {
			return UsageError("uninstall deletes the stored credentials of every managed account; pass --yes to confirm")
		}
		fmt.Fprint(out, "Uninstall ccdad? [y/N] ")
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintln(out, "Left alone.")
			return WithCode(errSilent, ExitNothingToDo)
		}
	}

	// The daemon FIRST, and only once it has actually released the singleton.
	// It rewrites status.json every second: deleting the store underneath it
	// leaves a live process writing into a directory that no longer exists, and
	// on Windows its open handle blocks the removal outright.
	if _, err := stopDaemon(cmd); err != nil {
		return fmt.Errorf("refusing to uninstall while a daemon may still be running: %w", err)
	}

	if storeState.present {
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("removing %s: %w", root, err)
		}
		fmt.Fprintf(out, "Removed %s.\n", root)
	}

	// A failure here must not strand a user whose store is already gone, so it
	// is a warning rather than a return.
	if removed, err := unwireMCP(); err != nil {
		fmt.Fprintf(out, "The Claude Code registration could not be removed: %v\n", err)
	} else if removed {
		fmt.Fprintln(out, "Removed ccdad's Claude Code registration.")
	}

	switch {
	case exeErr != nil:
		fmt.Fprintf(out, "The ccdad binary could not be located, so it is still installed: %v\n", exeErr)
	case owner != "":
		fmt.Fprintf(out, "%s owns %s, so it was left alone — remove it with %s.\n", owner, exe, uninstallHint(owner))
	default:
		scheduled, err := removeSelf(exe)
		switch {
		case err != nil:
			fmt.Fprintf(out, "%s could not be removed: %v\n", exe, err)
		case scheduled:
			fmt.Fprintf(out, "%s is running, so it was renamed aside and is scheduled for deletion at the next restart.\n", exe)
		default:
			fmt.Fprintf(out, "Removed %s.\n", exe)
		}
	}

	if hasLive {
		fmt.Fprintf(out, "You are still logged in to Claude Code as %s; ccdad did not touch that.\n", current.Label())
	}
	// PATH last, and only what ccdad itself registered: `ccdad setup-path`
	// writes a marker-fenced block on Unix and an HKCU\Environment entry on
	// Windows, and install.ps1 has written that same registry entry since it
	// shipped — so on Windows this was owed even before setup-path existed, and
	// leaving it behind points a user's PATH at a binary that has just been
	// deleted.
	//
	// It runs after the binary is gone because that is the order the user reads
	// it in, and it WARNS rather than returning: someone whose store has
	// already been deleted must not be left with a half-uninstalled machine
	// because one startup file could not be rewritten. §11.2 fix 5 is not in
	// tension with this — its reason is that `curl | bash` cannot prompt, and
	// this command has both a prompt and a --yes.
	removed, err := unregisterPath(pathDir)
	// What WAS removed is printed whatever else happened. Reporting only the
	// failure leaves a user believing nothing changed while several of their
	// startup files have in fact been rewritten.
	for _, place := range removed {
		fmt.Fprintf(out, "Removed ccdad's PATH entry from %s.\n", place)
	}
	if err != nil {
		fmt.Fprintf(out, "ccdad's PATH entry could not be removed everywhere, so some of it is still there: %v\n", err)
	}
	return nil
}

// storeInspection is what could be learned about the store WITHOUT creating any
// part of it.
type storeInspection struct {
	root     string
	present  bool
	accounts []store.Account
	lookup   func(uuid string) (cclink.Blob, error)
}

// inspectStore decides whether the store may be deleted, and refuses anything
// that is not demonstrably one.
//
// CCDAD_HOME can point ANYWHERE — at a home directory, at a shared directory,
// at a typo — and ccpath.StoreHome returns the raw value when it is set. An
// os.RemoveAll on a bare environment variable is how a user's files disappear,
// so the marker file has to be there, at the top level, before this command
// will touch the directory at all.
func inspectStore(root string) (storeInspection, error) {
	s := storeInspection{root: root, lookup: func(string) (cclink.Blob, error) { return nil, os.ErrNotExist }}
	if !filepath.IsAbs(root) {
		return s, fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}

	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return s, fmt.Errorf("reading %s: %w", root, err)
	case !info.IsDir():
		return s, fmt.Errorf("%s is not a directory", root)
	}

	markers := storeMarkers()
	found := false
	for _, name := range markers {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			found = true
			break
		}
	}
	if !found {
		return s, fmt.Errorf(
			"refusing to delete %s: it holds none of the files ccdad creates (%s), so it is not a ccdad store. "+
				"Either CCDAD_HOME is pointing somewhere unintended, or ccdad never stored anything here — "+
				"in which case there is nothing to remove and you can delete the directory yourself",
			root, strings.Join(markers, ", "))
	}

	// Only now, with the marker seen, is it safe to open — Open does an
	// MkdirAll, which on any other path would manufacture what it deleted.
	opened, err := store.Open()
	if err != nil {
		return s, err
	}
	s.present, s.accounts, s.lookup = true, opened.Accounts(), opened.Credentials
	return s, nil
}

// enumerate says exactly what is about to be destroyed, before the question is
// asked. A confirmation prompt with nothing above it is a prompt people say yes
// to.
func enumerate(out io.Writer, s storeInspection, exe string, exeErr error, owner string,
	current store.Account, hasLive bool) {
	if s.present {
		// The account list and the warning that goes with it are printed only
		// when there ARE accounts. A store that has never held one — which is
		// what the first `ccdad list` on a fresh machine leaves — would
		// otherwise announce that the refresh tokens of nobody cannot be
		// recovered.
		if len(s.accounts) == 0 {
			fmt.Fprintf(out, "This will delete %s, which holds no accounts.\n", s.root)
		} else {
			fmt.Fprintf(out, "This will delete %s and the stored credentials of %d account(s):\n", s.root, len(s.accounts))
			for _, a := range s.accounts {
				marker := " "
				if hasLive && a.UUID == current.UUID {
					marker = "*"
				}
				fmt.Fprintf(out, "  %s %s\n", marker, a.Label())
			}
			fmt.Fprintln(out, "Their OAuth refresh tokens cannot be recovered; each account has to be logged in again.")
		}
		// A parallel session's credentials live inside the store, so they go
		// with it — including those of a `ccdad run` that is still going. This
		// does not refuse: uninstall is a deliberate act, and refusing would
		// strand someone whose session is wedged. It says so, because "Removed
		// <store>" does not tell a user their running session just lost its
		// login.
		if n := countSessions(s.root); n > 0 {
			fmt.Fprintf(out, "%d parallel session(s) have credentials under %s; a session still running "+
				"will lose its login.\n", n, filepath.Join(s.root, SessionsDirName))
		}
		if len(s.accounts) > 0 {
		}
	}
	switch {
	case exeErr != nil:
		fmt.Fprintf(out, "The ccdad binary cannot be located and will be left in place: %v\n", exeErr)
	case owner != "":
		fmt.Fprintf(out, "%s is owned by %s and will be left in place.\n", exe, owner)
	default:
		fmt.Fprintf(out, "It will also remove the binary at %s.\n", exe)
	}
	if hasLive {
		fmt.Fprintf(out, "Claude Code stays logged in as %s (marked *); its credentials file is not touched.\n", current.Label())
	}
}

func describeBinary(exe string, exeErr error, owner string) string {
	switch {
	case exeErr != nil:
		return ", and the binary cannot be located"
	case owner != "":
		return fmt.Sprintf(", and %s owns %s", owner, exe)
	}
	return ""
}

func uninstallHint(owner string) string {
	if owner == "Scoop" {
		return "'scoop uninstall ccdad'"
	}
	return "'brew uninstall ccdad'"
}

// packageManagerOwning names the package manager that installed exe, or "".
//
// Deleting a Homebrew- or Scoop-owned binary is worse than leaving it: the
// manager goes on believing ccdad is installed, `brew list` still names it, and
// the next upgrade reinstates a binary the user thought they had removed.
//
// The matching is by whole path SEGMENT, and it is spelled by hand rather than
// with filepath, for the same reason internal/daemon's event name is: filepath's
// separator is fixed at build time, so a Windows Scoop path would not split at
// all on the machine these tests run on. A directory merely NAMED like a prefix
// — /home/someone/homebrewery — must not match, which segment comparison gives
// and a strings.Contains would not.
func packageManagerOwning(exe string) string {
	if prefix := os.Getenv("HOMEBREW_PREFIX"); prefix != "" && underPath(exe, prefix) {
		return "Homebrew"
	}
	for _, prefix := range []string{"/opt/homebrew", "/usr/local/Cellar", "/usr/local/opt", "/home/linuxbrew/.linuxbrew"} {
		if underPath(exe, prefix) {
			return "Homebrew"
		}
	}
	if prefix := os.Getenv("SCOOP"); prefix != "" && underPath(exe, prefix) {
		return "Scoop"
	}
	// Scoop's own layout, wherever it was installed: <root>/shims and
	// <root>/apps.
	segments := strings.Split(normalizePath(exe), "/")
	for i, seg := range segments {
		if strings.EqualFold(seg, "scoop") && i+1 < len(segments) &&
			(strings.EqualFold(segments[i+1], "shims") || strings.EqualFold(segments[i+1], "apps")) {
			return "Scoop"
		}
	}
	return ""
}

// underPath reports whether p sits inside prefix, comparing whole segments.
func underPath(p, prefix string) bool {
	p, prefix = normalizePath(p), normalizePath(prefix)
	if !strings.EqualFold(p, prefix) && !strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix)+"/") {
		return false
	}
	return true
}

func normalizePath(p string) string { return path.Clean(strings.ReplaceAll(p, `\`, "/")) }
