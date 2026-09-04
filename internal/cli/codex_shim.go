package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The shim is how `codex`, typed bare at a prompt, reaches ccdad. It is a
// two-line POSIX script in a directory of ccdad's own, put ahead of the real
// codex on PATH -- and it exists because codex holds its credential in memory
// for the life of the process and re-reads it on exactly one condition, an
// HTTP 401. Quota exhaustion is a 429, so the two never meet: no file swap can
// move a running codex to another account, and the honest capability from the
// file layer is a launch-time chooser. The shim is that chooser, and what it
// launches is a codex whose API base is ccdad's own loopback proxy.
//
// The alternative was editing the user's ~/.codex/config.toml, and it was
// refused: that file is theirs, a merge of ccdad's settings into it survives
// `ccdad uninstall` only if uninstall can un-merge it exactly, and any codex
// the user starts from an editor or a desktop app would pick the settings up
// with no ccdad in sight to serve them.

// shimDirName is the directory under CCDAD_HOME that goes on PATH ahead of the
// real codex. It holds the shim and nothing else.
const shimDirName = "bin"

// shimRecordName is the install record's basename directly under CCDAD_HOME.
//
// The record is what `setup-path` keys the derived directory set on, so it is
// the thing that says "this machine wants <CCDAD_HOME>/bin on PATH". The shim
// script's own existence is deliberately NOT that signal: a user who deleted
// the script by hand has said something, and a PATH entry pointing at an empty
// directory is harmless where a re-created script would be a surprise.
const shimRecordName = "codex-shim.json"

// shimProgramName is what the shim is called. It is the name codex is invoked
// by, which is the whole mechanism.
const shimProgramName = "codex"

// shimOS is the platform the shim decision is made for.
//
// A var rather than runtime.GOOS inline, so the Windows refusal can be
// exercised on the machine this package's tests run on. Windows gets no shim in
// v1: the shim would have to be a .cmd, which puts cmd.exe in front of every
// prompt codex is handed, and this tree refuses that launch rather than
// mangling the arguments -- so a Windows shim would turn a working prompt into
// a usage error. Production never reassigns this.
var shimOS = runtime.GOOS

// codexShimRecord is what ccdad writes down about the shim it installed.
//
// The PATH is recorded rather than recomputed because CCDAD_HOME can move: a
// record naming a directory that is no longer the one ccdad would choose is
// exactly what `ccdad doctor` has to be able to see.
type codexShimRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	Path          string `json:"path"`
	InstalledAt   string `json:"installedAt"`
}

// shimDir is <CCDAD_HOME>/bin, or "" when the store cannot be resolved.
//
// An empty string rather than an error, because every caller's answer to an
// unresolvable home is the same -- there is no shim directory -- and threading
// an error through the PATH walk and the doctor row would make three of them
// say the same thing three different ways. The one caller that must not treat
// "" as a directory is registeredDirs, which skips it by name.
func shimDir() string {
	root, err := ccpath.StoreHome()
	if err != nil {
		return ""
	}
	return filepath.Join(root, shimDirName)
}

// shimPath is the shim script itself.
func shimPath() string {
	dir := shimDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, shimProgramName)
}

// shimRunnable answers whether THIS user can actually run the shim, which is
// the question a PATH search asks and is not the question a mode answers.
//
// exec.LookPath rather than a permission-bit test, because an owner or an ACL
// can withhold execute from this user on a file whose mode reads 0o755.
// Measured on darwin: a shim at 0o755 plus
// `chmod +a "user:<me> deny execute" <shim>` keeps mode 0o755, keeps
// Perm()&0o111 set, and makes exec.LookPath and every exec of the file
// "permission denied".
//
// A var so the arms that turn on the two answers disagreeing can be exercised
// on an OS or a filesystem that cannot be given an ACL. Production never
// reassigns it.
var shimRunnable = func(path string) error {
	_, err := exec.LookPath(path)
	return err
}

// errShimUnrunnable is the refusal for a shim ccdad cannot make runnable.
//
// The only repair ccdad has is os.Chmod, and it reaches the permission bits and
// nothing else: it cannot clear an ACL entry and it cannot change who owns the
// file. So on a shim whose mode already says executable and which still will
// not run, BOTH of the things install would otherwise say are false -- "already
// installed", of a shim that cannot start, and "a new terminal runs `codex`
// through ccdad", of a shim no terminal will run.
//
// Exit 4 and not 1: the user asked for something, it is blocked, and there is a
// different command to run. It is theirs and not ccdad's, which is the whole
// content of the message.
//
// Unlinking the file and writing a fresh one is deliberately NOT ccdad's repair
// here. It is somebody's decision -- an administrator's, a management profile's,
// the user's own -- and stepping out from under it with nothing said is worse
// than refusing. It is also not reliable: measured on darwin, a directory
// carrying `deny execute,file_inherit` hands the entry straight to a file
// created inside it, so the fresh shim is born unrunnable and install would
// report success on a machine it had not fixed.
func errShimUnrunnable(path string, mode fs.FileMode, cause error) error {
	return WithCode(fmt.Errorf(
		"%s is mode %v and this user still cannot execute it (%v). That is an ACL entry or the file's "+
			"ownership, not a permission bit, and ccdad repairs the mode and nothing else, so no "+
			"reinstall will put it back. Clear the deny entry yourself -- `chmod -N %s` on macOS, "+
			"`setfacl -b %s` on Linux -- and when %s hands the entry down by inheritance, clear it there "+
			"too", path, mode, cause, path, path, filepath.Dir(path)), ExitBlocked)
}

// shimRecordPath is <CCDAD_HOME>/codex-shim.json.
func shimRecordPath() string {
	root, err := ccpath.StoreHome()
	if err != nil {
		return ""
	}
	return filepath.Join(root, shimRecordName)
}

// shimRecord reads the install record. A record that is absent, unreadable or
// unparseable all answer the same: this machine has not asked for a shim.
//
// Unparseable is folded in with absent deliberately. The only consumer is a
// directory set, and refusing to register a directory because a small JSON
// document is damaged would take codex off ccdad's proxy over a file the user
// can neither see nor be told about from here -- `ccdad codex shim install`
// rewrites it, which is the repair.
func shimRecord() (codexShimRecord, bool) {
	path := shimRecordPath()
	if path == "" {
		return codexShimRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return codexShimRecord{}, false
	}
	var rec codexShimRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return codexShimRecord{}, false
	}
	return rec, true
}

// codexShimBody is the whole shim, byte for byte, and every word of it is
// load-bearing.
//
//	#!/bin/sh   the most portable interpreter there is. The shim runs on
//	            whatever the machine has, including a dash /bin/sh, and it uses
//	            nothing bash-only so it never needs to find bash.
//	exec        replaces the shell rather than leaving one waiting on a child.
//	            The process tree a user's terminal reports is then the codex
//	            they started, Ctrl-C reaches it directly, and the exit status is
//	            codex's without a second process to translate it.
//	ccdad       a BARE WORD, resolved through PATH at run time. An absolute
//	            path baked in here would be stale the first time the binary
//	            moved -- which `ccdad update` and every package manager do --
//	            and the shim is the one file a broken ccdad cannot repair.
//	--          stops ccdad parsing what follows. Without it `codex -c x=1`
//	            reaches ccdad as ccdad's own flag and exits 2 before codex is
//	            ever started.
//	"$@"        quoted, so a prompt with a space in it stays one argument.
//	            Unquoted, `codex exec "fix the tests"` becomes three.
const codexShimBody = "#!/bin/sh\nexec ccdad codex exec -- \"$@\"\n"

func newCodexShimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shim",
		Short: "Manage the codex shim ccdad puts on your PATH",
		Long: "The shim is a two-line script at <CCDAD_HOME>/bin/codex, and the directory\n" +
			"holding it goes on your PATH ahead of the real codex. Typing `codex` then runs\n" +
			"`ccdad codex exec`, which starts the real codex pointed at ccdad's own local\n" +
			"proxy -- so the session is billed to the account ccdad chose and rotates when\n" +
			"that account runs out.\n\n" +
			"ccdad never edits codex's own config.toml. That file is yours, an uninstall\n" +
			"could not reliably un-merge ccdad's settings out of it, and any codex started\n" +
			"from an editor or a desktop app would pick those settings up with no ccdad\n" +
			"there to serve them.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			return UsageError("shim needs a subcommand: install")
		},
	}
	cmd.AddCommand(newCodexShimInstallCmd())
	return cmd
}

func newCodexShimInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Write the codex shim and register its directory on your PATH",
		Long: "install writes <CCDAD_HOME>/bin/codex and registers that directory through the\n" +
			"same marker-fenced block `ccdad setup-path` manages, so there is one block on\n" +
			"the machine and `ccdad uninstall` takes it back with the removal it already\n" +
			"has.\n\n" +
			"It is exit 3 when the shim is already there and its directory is already\n" +
			"registered.\n\n" +
			"Two kinds of codex session are NOT routed by it, and both are deliberate: an\n" +
			"IDE or desktop app that spawns codex itself never consults your shell's PATH,\n" +
			"and Windows gets no shim at all -- run `ccdad codex exec -- <args>` there.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCodexShimInstall(cmd)
		},
	}
}

func runCodexShimInstall(cmd *cobra.Command) error {
	// Windows first, before anything is created. A .cmd shim would put cmd.exe
	// in front of every prompt codex is handed, and this tree refuses that
	// launch rather than letting cmd.exe re-interpret an argument -- so the
	// shim would turn working invocations into usage errors. Windows PATH
	// registration also APPENDS where the Unix block prepends, so a shim there
	// would not come first anyway.
	//
	// Exit 4 and not 3: the user asked for something, it is blocked, and there
	// is a different command to run. 3 is the code operators are told to ignore.
	if shimOS == "windows" {
		return WithCode(errors.New(
			"ccdad does not install a codex shim on Windows. The shim would have to be a .cmd, which "+
				"puts cmd.exe in front of every prompt codex is given, and ccdad refuses that launch "+
				"rather than letting cmd.exe re-interpret an argument. Run `ccdad codex exec -- <args>`, "+
				"or `ccdad run <account> -- codex <args>`, both of which need no shim"), ExitBlocked)
	}
	if shimDir() == "" {
		return errors.New("ccdad cannot resolve its store directory, so it cannot say where to put the codex shim")
	}

	wrote, err := writeCodexShim()
	if err != nil {
		return err
	}
	// AFTER the record is written, never before: registeredDirs keys the shim
	// directory off the record's existence, so the order here is what decides
	// whether the block that gets written names the directory at all.
	dirs, err := registeredDirs()
	if err != nil {
		return err
	}
	perr := setupPathApply(cmd, dirs, setupPathOptions{})
	registered := perr == nil
	if perr != nil && CodeFor(perr) != ExitNothingToDo {
		// csh, an undetectable $SHELL, sudo, an unwritable startup file: all of
		// those are setup-path's own refusals and they are returned as they are.
		// The shim itself is on disk either way, which is what the message
		// setup-path already printed is about.
		return perr
	}
	out := cmd.ErrOrStderr()
	if !wrote && !registered {
		fmt.Fprintf(out, "%s is already the ccdad shim, and %s is already registered.\n", shimPath(), shimDir())
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(out, "Wrote %s. A new terminal runs `codex` through ccdad; this one does not until it "+
		"re-reads its startup file. `ccdad doctor` says which codex a bare `codex` resolves to.\n", shimPath())
	return nil
}

// writeCodexShim puts the script and the record on disk, and reports whether
// anything changed.
//
// "Already installed" is three things and not one: the body, the record, and
// the executable bit. A machine whose script survived and whose record was
// deleted has an unregistered directory, and one whose shim lost its executable
// bit has a codex that refuses to start -- treating either as installed would
// report nothing to do while the thing the user asked for stayed broken.
//
// A fourth thing is asked but never repaired: whether the shim can actually be
// run. The three above are all writes, and the answer to all three is a write
// this function makes; a shim held down by an ACL entry or by its ownership is
// past what any write from here can reach, and it is refused rather than
// reported either way.
func writeCodexShim() (bool, error) {
	dir, path := shimDir(), shimPath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("creating %s: %w", dir, err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	// The MODE is the third of those three. os.WriteFile applies its mode only
	// when it CREATES the file, so a shim somebody chmod'ed keeps whatever
	// mode it had -- and a shim that is not executable is a
	// `codex: Permission denied` with nothing to point at, from a codex that is
	// still first on PATH with the right body in it. Comparing the body alone
	// would answer "nothing to do" to exactly the machine that needs the chmod
	// below, and put the only repair there is out of reach.
	executable := false
	var mode fs.FileMode
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
		executable = mode&0o111 != 0
	}
	_, hadRecord := shimRecord()
	if string(existing) == codexShimBody && hadRecord && executable {
		// Nothing left to WRITE -- and a mode is not permission, so the last
		// question is the one the shell asks rather than the one the bits
		// answer. Without it this returns "already installed" to the machine
		// whose `codex` cannot start, and that user follows the advice, nothing
		// happens, and nothing on the machine says why.
		if rerr := shimRunnable(path); rerr != nil {
			return false, errShimUnrunnable(path, mode, rerr)
		}
		return false, nil
	}
	if err := os.WriteFile(path, []byte(codexShimBody), 0o755); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	// The repair the mode check above exists to reach.
	if err := os.Chmod(path, 0o755); err != nil {
		return false, fmt.Errorf("making %s executable: %w", path, err)
	}
	// Whether that repair worked, asked rather than assumed. The chmod lands on
	// the mode bits, a deny entry is not one of them, and the caller's next line
	// is "a new terminal runs `codex` through ccdad" -- measured on darwin, a
	// rewrite and a chmod over a shim carrying `deny execute` leave it exactly
	// as unrunnable as it was. The record is written only past this point, so
	// the record continues to mean ccdad believes this shim works.
	if rerr := shimRunnable(path); rerr != nil {
		return false, errShimUnrunnable(path, 0o755, rerr)
	}
	rec, err := json.Marshal(codexShimRecord{
		SchemaVersion: 1,
		Path:          path,
		InstalledAt:   time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return false, fmt.Errorf("encoding the codex shim record: %w", err)
	}
	if err := atomicfile.WriteFile(shimRecordPath(), rec, 0o600); err != nil {
		return false, fmt.Errorf("writing %s: %w", shimRecordPath(), err)
	}
	return true, nil
}
