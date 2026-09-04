//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// setupPathWorld is a machine with a home directory, a shell and a ccdad
// binary in a directory of its own. The binary's directory is returned because
// it is what every assertion below is about.
func setupPathWorld(t *testing.T, shell string) (home, dir string) {
	t.Helper()
	isolate(t)
	home = os.Getenv("HOME")
	dir = filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, filepath.Join(dir, "ccdad"))
	t.Setenv("SHELL", shell)
	// A PATH that does NOT hold the directory, which is the machine this
	// command exists for.
	t.Setenv("PATH", "/usr/bin:/bin")
	// packageManagerOwning consults these, and a developer running the suite
	// under Homebrew would otherwise get a refusal instead of a write.
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	return home, dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// bash needs two files and must never create the third. Creating
// ~/.bash_profile stops every bash login shell from reading ~/.profile again,
// which on a stock Debian or Ubuntu home disables ~/bin, ~/.local/bin and the
// ~/.bashrc chain in one write.
func TestSetupPathWritesBothBashFilesAndNeverCreatesBashProfile(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitOK {
		t.Fatalf("setup-path = %d, want %d\n%s", code, ExitOK, errOut)
	}
	for _, name := range []string{".bashrc", ".profile"} {
		body := read(t, filepath.Join(home, name))
		if !strings.Contains(body, dir) {
			t.Errorf("~/%s does not register %s:\n%s", name, dir, body)
		}
	}
	if exists(filepath.Join(home, ".bash_profile")) {
		t.Error("setup-path created ~/.bash_profile, which permanently stops bash login shells " +
			"from reading ~/.profile — every PATH entry that file adds is gone")
	}
	// A login shell reads exactly one of the three, so writing only ~/.bashrc
	// would be dead on macOS Terminal; writing only the login file would be
	// dead in a Linux terminal emulator.
	if !strings.Contains(errOut, filepath.Join(home, ".bashrc")) ||
		!strings.Contains(errOut, filepath.Join(home, ".profile")) {
		t.Errorf("the report does not name both files it wrote:\n%s", errOut)
	}
}

func TestSetupPathUsesAnExistingBashLoginFileRatherThanMakingAnother(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if body := read(t, filepath.Join(home, ".bash_profile")); !strings.Contains(body, dir) {
		t.Errorf("~/.bash_profile exists and is the file bash reads, but the block went elsewhere:\n%s", body)
	}
	if exists(filepath.Join(home, ".profile")) {
		t.Error("setup-path wrote ~/.profile, which bash will never read while ~/.bash_profile exists")
	}
}

// The idempotence that is the feature, and the exit code that reports it.
func TestSetupPathRunTwiceLeavesOneBlockPerFileAndExitsThree(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/bash")

	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("first run = %d, want %d\n%s", code, ExitOK, errOut)
	}
	first := read(t, filepath.Join(home, ".bashrc"))

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitNothingToDo {
		t.Fatalf("second run = %d, want %d (nothing was written)\n%s", code, ExitNothingToDo, errOut)
	}
	after := read(t, filepath.Join(home, ".bashrc"))
	if after != first {
		t.Errorf("the second run changed ~/.bashrc:\n%s\nwas\n%s", after, first)
	}
	if n := strings.Count(after, setupPathBegin); n != 1 {
		t.Errorf("~/.bashrc holds %d ccdad blocks after two runs, want 1", n)
	}
	if !strings.Contains(errOut, "open a new terminal") {
		t.Errorf("exit 3 without telling the user why nothing happened:\n%s", errOut)
	}
}

// The amendment this task made to its own brief, pinned. Exit 3 keys off what
// is REGISTERED, never off the live $PATH: the user who pasted an export line
// into their running shell has a live PATH that says yes and no durable
// registration at all, and a $PATH-keyed exit 3 sends them away with the next
// terminal still answering `ccdad: command not found`.
//
// The fixture makes the two implementations produce DIFFERENT results — a
// $PATH-keyed one writes nothing and exits 3, this one writes and exits 0 —
// rather than the same exit code for opposite reasons.
func TestSetupPathRegistersEvenWhenTheDirectoryIsAlreadyOnTheLivePATH(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")
	t.Setenv("PATH", dir+":/usr/bin:/bin")

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitOK {
		t.Fatalf("setup-path with the directory already on $PATH = %d, want %d: a live-$PATH-keyed "+
			"exit 3 leaves the user with no durable registration\n%s", code, ExitOK, errOut)
	}
	if body := read(t, filepath.Join(home, ".bashrc")); !strings.Contains(body, dir) {
		t.Errorf("nothing was registered:\n%s", body)
	}
	if !strings.Contains(errOut, "already on THIS shell's PATH") {
		t.Errorf("the report does not say the directory was already on this shell's PATH:\n%s", errOut)
	}
}

func TestSetupPathLeavesAPackageManagerToManageItsOwnPATH(t *testing.T) {
	// Registering a Homebrew install would pin a versioned Cellar path that the
	// next upgrade moves, and it would paper over a shell that has never run
	// `brew shellenv` at all.
	home, dir := setupPathWorld(t, "/bin/bash")
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "bin", "ccdad"))

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitBlocked {
		t.Fatalf("setup-path on a Homebrew install = %d, want %d\n%s", code, ExitBlocked, errOut)
	}
	if !strings.Contains(errOut, "brew shellenv") {
		t.Errorf("the refusal does not give Homebrew's own instruction:\n%s", errOut)
	}
	if exists(filepath.Join(home, ".bashrc")) {
		t.Error("setup-path wrote a startup file for a binary Homebrew owns")
	}
	_ = dir

	// The same install, once brew's own directory IS on PATH, is nothing to do
	// rather than something to alert on.
	t.Setenv("PATH", filepath.Join(brew, "bin")+":/usr/bin")
	if code, _, _, _ := runRoot(t, "setup-path"); code != ExitNothingToDo {
		t.Errorf("setup-path on a Homebrew install already on PATH = %d, want %d", code, ExitNothingToDo)
	}
}

func TestSetupPathRefusesCshAndHandsOverTheLineInstead(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/tcsh")

	code, stdout, errOut, _ := runRoot(t, "setup-path")
	if code != ExitBlocked {
		t.Fatalf("setup-path under tcsh = %d, want %d\n%s", code, ExitBlocked, errOut)
	}
	if !strings.Contains(stdout, "set path") || !strings.Contains(stdout, dir) {
		t.Errorf("no csh line on stdout to paste:\n%s", stdout)
	}
	if strings.Contains(stdout, "export ") {
		t.Errorf("csh was handed POSIX syntax, which errors on every shell start:\n%s", stdout)
	}
	for _, name := range []string{".bashrc", ".profile", ".cshrc", ".tcshrc"} {
		if exists(filepath.Join(home, name)) {
			t.Errorf("setup-path wrote ~/%s for a csh user", name)
		}
	}
}

func TestSetupPathRefusesAShellItDoesNotKnow(t *testing.T) {
	for _, tc := range []struct{ name, shell string }{
		{"unset", ""},
		{"unknown", "/usr/bin/nonesuch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setupPathWorld(t, tc.shell)
			// The reason rides back as an error, so ExecuteWith gives it the
			// same `ccdad: ` prefix every other error in this binary gets.
			code, _, _, top := runRoot(t, "setup-path")
			if code != ExitBlocked {
				t.Fatalf("setup-path with $SHELL=%q = %d, want %d\n%s", tc.shell, code, ExitBlocked, top)
			}
			if !strings.Contains(top, "--shell") || !strings.Contains(top, "--print") {
				t.Errorf("the refusal names neither way forward:\n%s", top)
			}
			if exists(filepath.Join(home, ".profile")) {
				t.Error("setup-path guessed at a startup file for a shell it could not identify")
			}
		})
	}
}

func TestSetupPathShellFlagOverridesTheEnvironment(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")

	if code, _, errOut, _ := runRoot(t, "setup-path", "--shell", "zsh"); code != ExitOK {
		t.Fatalf("setup-path --shell zsh = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if body := read(t, filepath.Join(home, ".zshrc")); !strings.Contains(body, dir) {
		t.Errorf("~/.zshrc does not register %s:\n%s", dir, body)
	}
	for _, name := range []string{".bashrc", ".profile"} {
		if exists(filepath.Join(home, name)) {
			t.Errorf("--shell zsh also wrote ~/%s; the flag chose one shell", name)
		}
	}
}

func TestSetupPathRejectsAShellFlagValueItCannotPlace(t *testing.T) {
	// A value the user typed is a usage error; a $SHELL ccdad cannot place is
	// not. The exit contract keeps 2 for usage alone so a script can tell a
	// typo from a machine it cannot serve.
	setupPathWorld(t, "/bin/bash")
	code, _, _, top := runRoot(t, "setup-path", "--shell", "nonesuch")
	if code != ExitUsage {
		t.Fatalf("setup-path --shell nonesuch = %d, want %d\n%s", code, ExitUsage, top)
	}
	for _, want := range []string{"bash", "zsh", "fish", "sh"} {
		if !strings.Contains(top, want) {
			t.Errorf("the usage error does not name %q as an accepted value:\n%s", want, top)
		}
	}
}

func TestSetupPathPrintWritesNothingAtAll(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")

	code, stdout, errOut, _ := runRoot(t, "setup-path", "--print")
	if code != ExitOK {
		t.Fatalf("setup-path --print = %d, want %d\n%s", code, ExitOK, errOut)
	}
	if stdout != renderBlock([]string{dir}, shellBash) {
		t.Errorf("--print emitted\n%q\nwant the exact bytes the writer writes\n%q", stdout, renderBlock([]string{dir}, shellBash))
	}
	if strings.Contains(stdout, "ccdad setup-path`") && !strings.Contains(stdout, setupPathBegin) {
		t.Error("--print put a human notice on stdout, where the block a user pipes has to stand alone")
	}
	for _, name := range []string{".bashrc", ".profile", ".zshrc"} {
		if exists(filepath.Join(home, name)) {
			t.Errorf("--print wrote ~/%s", name)
		}
	}

	// Total, even where the write path refuses: a user whose shell cannot be
	// identified still gets something to paste.
	t.Setenv("SHELL", "")
	code, stdout, _, _ = runRoot(t, "setup-path", "--print")
	if code != ExitOK || !strings.Contains(stdout, dir) {
		t.Errorf("--print with no $SHELL = %d with stdout %q; it must still hand over the portable form",
			code, stdout)
	}
}

func TestSetupPathKeepsASymlinkedStartupFileASymlink(t *testing.T) {
	// A symlinked rc file is a dotfiles repository. Replacing the link with a
	// regular file detaches the user from theirs, silently, and their next
	// `git status` in that repo shows nothing wrong.
	home, dir := setupPathWorld(t, "/bin/zsh")
	repo := filepath.Join(t.TempDir(), "dotfiles")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(repo, "zshrc")
	if err := os.WriteFile(real, []byte("# from the repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".zshrc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d, want %d\n%s", code, ExitOK, errOut)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("setup-path replaced the symlinked ~/.zshrc with a regular file")
	}
	body := read(t, real)
	if !strings.Contains(body, dir) || !strings.Contains(body, "# from the repo") {
		t.Errorf("the linked file lost its content or never got the block:\n%s", body)
	}
}

func TestSetupPathRefusesAReadOnlyStartupFile(t *testing.T) {
	// A read-only rc file is a deliberate signal from its owner. An atomic
	// replace overwrites it without a murmur, where an ordinary append would
	// have been refused by the kernel.
	home, _ := setupPathWorld(t, "/bin/zsh")
	path := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(path, []byte("# locked\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit, so this machine cannot exercise the refusal")
	}

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitFailure {
		t.Fatalf("setup-path on a read-only ~/.zshrc = %d, want %d\n%s", code, ExitFailure, errOut)
	}
	if body := read(t, path); body != "# locked\n" {
		t.Errorf("the read-only file was rewritten anyway:\n%s", body)
	}
}

func TestSetupPathRefusesAnUnterminatedFenceRatherThanGuessing(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/zsh")
	path := filepath.Join(home, ".zshrc")
	original := "a=1\n" + setupPathBegin + "\nexport PATH=/old:$PATH\nb=2\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitFailure {
		t.Fatalf("setup-path on a half-deleted block = %d, want %d\n%s", code, ExitFailure, errOut)
	}
	if body := read(t, path); body != original {
		t.Errorf("the file was rewritten despite the refusal:\n%s", body)
	}
}

func TestSetupPathWritesFishSyntaxIntoConfigFish(t *testing.T) {
	// POSIX `export PATH=...` is a syntax error in fish, printed on every
	// single shell start, forever — strictly worse than not being on PATH.
	//
	// Disclosed: fish is not installed on the machine this suite runs on, so
	// unlike the bash and dash blocks the fish block is asserted as text and
	// never executed. TestFishBlockRunsUnderFish runs it where fish exists.
	home, dir := setupPathWorld(t, "/usr/bin/fish")

	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path under fish = %d, want %d\n%s", code, ExitOK, errOut)
	}
	body := read(t, filepath.Join(home, ".config", "fish", "config.fish"))
	if strings.Contains(body, "export ") {
		t.Errorf("POSIX syntax went into config.fish:\n%s", body)
	}
	for _, want := range []string{"contains --", "set -gx PATH", dir} {
		if !strings.Contains(body, want) {
			t.Errorf("config.fish does not carry %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "fish_add_path") {
		t.Error("fish_add_path stores $fish_user_paths as a UNIVERSAL variable outside config.fish, " +
			"so the entry would survive deleting this block and neither a rerun nor uninstall could remove it")
	}
}

func TestUnderSudoIsRootStandingInForSomeoneElse(t *testing.T) {
	// The predicate exists because neither half can be reached otherwise: a
	// suite that does not run as root can never make Geteuid answer 0, and one
	// that does has no SUDO_USER.
	cases := []struct {
		euid     int
		sudoUser string
		want     bool
		why      string
	}{
		{euid: 1000, sudoUser: "", want: false, why: "an ordinary user"},
		{euid: 1000, sudoUser: "kweiza", want: false,
			why: "SUDO_USER survives into a nested non-root shell; only root writing for someone else is the hazard"},
		{euid: 0, sudoUser: "", want: false, why: "a genuine root login owns /root and may set it up"},
		{euid: 0, sudoUser: "kweiza", want: true, why: "the block would land in /root, or leave the user's own file owned by root"},
	}
	for _, tc := range cases {
		if got := underSudo(tc.euid, tc.sudoUser); got != tc.want {
			t.Errorf("underSudo(%d, %q) = %v, want %v: %s", tc.euid, tc.sudoUser, got, tc.want, tc.why)
		}
	}
}

// What setup-path writes, uninstall takes back. Without this the command
// writes into a file that no command in the tree can clean up, and after the
// uninstall the binary is gone — so no `setup-path` of any kind could ever run
// to undo it.
func TestUninstallRemovesExactlyWhatSetupPathWrote(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	rc := filepath.Join(home, ".zshrc")
	const original = "export EDITOR=vi\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d\n%s", code, errOut)
	}
	if body := read(t, rc); !strings.Contains(body, dir) {
		t.Fatalf("setup-path wrote nothing to ~/.zshrc:\n%s", body)
	}

	cmd := newUninstallCmd()
	err, _, errOut := runCmd(t, cmd, "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	if body := read(t, rc); body != original {
		t.Errorf("~/.zshrc after uninstall:\n%q\nwant the bytes it had before setup-path ran:\n%q", body, original)
	}
	if !strings.Contains(errOut, rc) {
		t.Errorf("uninstall did not say it had edited %s:\n%s", rc, errOut)
	}
}

func TestUninstallSaysItWillRemoveThePathEntryBeforeAsking(t *testing.T) {
	// A confirmation prompt with nothing above it is a prompt people say yes
	// to, and this is the one line that says a startup file is about to change.
	home, _ := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, true, false)
	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d\n%s", code, errOut)
	}
	rc := filepath.Join(home, ".zshrc")
	before := read(t, rc)

	cmd := newUninstallCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	err, _, errOut := runCmd(t, cmd)

	if CodeFor(err) != ExitNothingToDo {
		t.Fatalf("answering no = %d, want %d", CodeFor(err), ExitNothingToDo)
	}
	if !strings.Contains(errOut, rc) || !strings.Contains(errOut, "PATH") {
		t.Errorf("the enumeration does not warn that %s will be edited:\n%s", rc, errOut)
	}
	if after := read(t, rc); after != before {
		t.Errorf("answering no still edited ~/.zshrc:\n%s", after)
	}
}

func TestUninstallLeavesAStartupFileItNeverWrote(t *testing.T) {
	// The rule that makes this safe at all: the fence marks what ccdad wrote,
	// and a user's own PATH line — even one naming the very same directory —
	// is not ccdad's to delete.
	home, dir := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	rc := filepath.Join(home, ".zshrc")
	original := "export PATH=\"" + dir + ":$PATH\"  # I wrote this myself\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newUninstallCmd()
	if err, _, errOut := runCmd(t, cmd, "--yes"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	if body := read(t, rc); body != original {
		t.Errorf("uninstall edited a startup file ccdad never wrote:\n%q\nwant\n%q", body, original)
	}
}

func TestUninstallWithNothingButAPathEntryIsStillSomethingToDo(t *testing.T) {
	// The exit-3 branch has to count the PATH entry, or a machine whose store
	// is already gone reports "nothing to uninstall" while a block still names
	// a directory the binary has left.
	home, _ := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d\n%s", code, errOut)
	}
	rc := filepath.Join(home, ".zshrc")

	// A Homebrew-owned binary is one uninstall may not delete, and the store
	// was never created — so the PATH entry is the only thing left.
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "bin", "ccdad"))

	cmd := newUninstallCmd()
	err, _, errOut := runCmd(t, cmd, "--yes")
	if err != nil {
		t.Fatalf("uninstall = %v, want it to proceed\n%s", err, errOut)
	}
	if body := read(t, rc); strings.Contains(body, setupPathBegin) {
		t.Errorf("the ccdad block survived an uninstall that had nothing else to do:\n%s", body)
	}
}

// The startup file keeps the permissions it had. Both directions are wrong and
// both are easy to write: widening a 0600 dotfile exposes a file its owner
// deliberately closed, and narrowing a 0644 one to the temp file's own 0600 is
// what happens when the chmod before the rename is simply forgotten.
func TestSetupPathKeepsAStartupFilesPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this machine's umask and root's mode handling would not show the difference")
	}
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			home, dir := setupPathWorld(t, "/bin/zsh")
			rc := filepath.Join(home, ".zshrc")
			if err := os.WriteFile(rc, []byte("export EDITOR=vi\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(rc, mode); err != nil {
				t.Fatal(err)
			}

			if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
				t.Fatalf("setup-path = %d\n%s", code, errOut)
			}
			info, err := os.Stat(rc)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Errorf("~/.zshrc is %v after setup-path, want %v", got, mode)
			}
			if body := read(t, rc); !strings.Contains(body, dir) {
				t.Errorf("the block never arrived:\n%s", body)
			}
		})
	}
}

// --print's dialect, over the shells. Asserting only bash left the branch that
// matters unpinned: the two refusal messages both send the user here, and a
// fish user who is handed the POSIX block pastes syntax fish rejects on every
// shell start forever.
func TestSetupPathPrintEmitsTheDialectOfTheShellItNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shell string
		flag  []string
		want  func(dir string) string
	}{
		{name: "bash from $SHELL", shell: "/bin/bash", want: func(d string) string { return renderBlock([]string{d}, shellBash) }},
		{name: "fish from $SHELL", shell: "/usr/bin/fish", want: func(d string) string { return renderBlock([]string{d}, shellFish) }},
		{name: "sh from $SHELL", shell: "/bin/dash", want: func(d string) string { return renderBlock([]string{d}, shellPOSIX) }},
		{name: "csh from $SHELL", shell: "/bin/tcsh", want: func(d string) string { return cshLine(d) + "\n" }},
		{name: "--shell wins", shell: "/bin/bash", flag: []string{"--shell", "fish"},
			want: func(d string) string { return renderBlock([]string{d}, shellFish) }},
		{name: "no $SHELL falls back to the portable form", shell: "",
			want: func(d string) string { return renderBlock([]string{d}, shellPOSIX) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dir := setupPathWorld(t, tc.shell)
			code, stdout, _, top := runRoot(t, append([]string{"setup-path", "--print"}, tc.flag...)...)
			if code != ExitOK {
				t.Fatalf("--print = %d, want %d\n%s", code, ExitOK, top)
			}
			if want := tc.want(dir); stdout != want {
				t.Errorf("--print emitted\n%q\nwant\n%q", stdout, want)
			}
		})
	}
}

// A shell the USER named is a typo, not an undetectable machine. Falling back
// to the portable form here exits 0 and hands a fish user POSIX syntax, while
// the same typo without --print is a usage error.
func TestSetupPathPrintRejectsAShellFlagValueItCannotPlace(t *testing.T) {
	setupPathWorld(t, "/usr/bin/fish")
	code, stdout, _, top := runRoot(t, "setup-path", "--print", "--shell", "fsh")
	if code != ExitUsage {
		t.Fatalf("--print --shell fsh = %d, want %d (the same as without --print)\n%s", code, ExitUsage, top)
	}
	if stdout != "" {
		t.Errorf("a rejected --shell still produced something to paste:\n%s", stdout)
	}
}

// The recipe both refusals point at: paste what --print emits, and a later run
// must find it rather than appending a second registration.
func TestSetupPathPrintOutputIsABlockALaterRunOwns(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	_, stdout, _, _ := runRoot(t, "setup-path", "--print")
	if err := os.WriteFile(rc, []byte("export EDITOR=vi\n"+stdout), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitNothingToDo {
		t.Fatalf("setup-path after pasting --print's output = %d, want %d: the pasted block was not "+
			"recognised, so this run added a second registration\n%s", code, ExitNothingToDo, errOut)
	}
	if n := strings.Count(read(t, rc), setupPathBegin); n != 1 {
		t.Errorf("~/.zshrc holds %d blocks, want 1", n)
	}
	if !strings.Contains(read(t, rc), dir) {
		t.Error("the pasted block does not name the directory")
	}
}

func TestSetupPathExitsThreeQuietlyWhenTheShellHasAlreadyReadTheFile(t *testing.T) {
	// The fourth cell of the exit table: registered AND live. Telling this user
	// "this shell has not read that file yet" sends them round a loop they have
	// already completed.
	_, dir := setupPathWorld(t, "/bin/zsh")
	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("first run = %d\n%s", code, errOut)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitNothingToDo {
		t.Fatalf("second run with the directory on PATH = %d, want %d\n%s", code, ExitNothingToDo, errOut)
	}
	if strings.Contains(errOut, "has not read that file yet") {
		t.Errorf("told a shell that demonstrably has the directory on PATH that it has not read the file:\n%s", errOut)
	}
	if !strings.Contains(errOut, "already registers") {
		t.Errorf("exit 3 with nothing said about why:\n%s", errOut)
	}
}

func TestSetupPathRefusesToReplaceADanglingSymlink(t *testing.T) {
	// ~/.zshrc points into a dotfiles repository that has not been cloned or
	// stowed yet. Renaming over the link replaces it with a regular file and
	// reports "Created"; the user's next `stow`/`chezmoi apply` then fails on a
	// target that is no longer a symlink, and their real zshrc never lands.
	home, _ := setupPathWorld(t, "/bin/zsh")
	link := filepath.Join(home, ".zshrc")
	missing := filepath.Join(t.TempDir(), "dotfiles", "zshrc")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatal(err)
	}

	code, _, _, top := runRoot(t, "setup-path")
	if code != ExitFailure {
		t.Fatalf("setup-path on a dangling symlink = %d, want %d\n%s", code, ExitFailure, top)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the dangling symlink was replaced by a regular file")
	}
	if !strings.Contains(top, missing) {
		t.Errorf("the refusal does not name the missing target, so the user cannot fix it:\n%s", top)
	}
}

// One bad candidate must not stop the others being cleaned, and uninstall must
// not report a clean machine while a live block remains.
func TestUninstallCleansEveryOtherStartupFileWhenOneIsUnusable(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d\n%s", code, errOut)
	}
	zshrc := filepath.Join(home, ".zshrc")

	// ~/.bashrc holds a half-deleted fence and sorts BEFORE ~/.zshrc in the
	// candidate list, so an early return hides the real block entirely.
	bashrc := filepath.Join(home, ".bashrc")
	stray := "a=1\n" + setupPathBegin + "\nb=2\n"
	if err := os.WriteFile(bashrc, []byte(stray), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory and a FIFO among the candidates: the first used to abort the
	// scan with EISDIR, the second used to block os.ReadFile forever.
	if err := os.Mkdir(filepath.Join(home, ".profile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, ".bash_profile"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newUninstallCmd()
	err, _, errOut := runCmd(t, cmd, "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	if body := read(t, zshrc); strings.Contains(body, setupPathBegin) {
		t.Errorf("the ~/.zshrc block survived because an earlier candidate failed:\n%s", body)
	}
	// Both lines by their own words. Asserting only that the path appears
	// anywhere in the output cannot fail: the pre-prompt enumeration names the
	// same file, so a run that removed the block and never said so still
	// matched.
	if want := "Removed ccdad's PATH entry from " + zshrc; !strings.Contains(errOut, want) {
		t.Errorf("uninstall never reported the removal it performed (%q):\n%s", want, errOut)
	}
	if want := "It will also remove ccdad's PATH entry from " + zshrc; !strings.Contains(errOut, want) {
		t.Errorf("the pre-prompt enumeration never named %s, so a prompted user would confirm without "+
			"being told this file changes — the scan stopped at the earlier bad candidate:\n%s", zshrc, errOut)
	}
	if !strings.Contains(errOut, bashrc) {
		t.Errorf("uninstall did not name the file it could not clean:\n%s", errOut)
	}
	if body := read(t, bashrc); body != stray {
		t.Errorf("the file with the stray marker was rewritten anyway:\n%s", body)
	}
}

func TestUninstallWithAnUnreadableStartupFileIsNotNothingToDo(t *testing.T) {
	// A scan that could not finish is not evidence of an empty machine. Exit 3
	// here tells a script the machine is clean while a live block remains.
	home, _ := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	// The ONLY ccdad-shaped thing on this machine is a file the scan cannot
	// read. That is the fixture that separates the two implementations: with
	// the scan's error ignored, the list of places is empty and uninstall
	// answers "nothing to uninstall" with exit 3 — telling a script the machine
	// is clean when ccdad has no idea whether it is.
	bashrc := filepath.Join(home, ".bashrc")
	stray := "a=1\n" + setupPathBegin + "\nb=2\n"
	if err := os.WriteFile(bashrc, []byte(stray), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nothing else to uninstall: no store, and a binary Homebrew owns.
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "bin", "ccdad"))

	cmd := newUninstallCmd()
	err, _, errOut := runCmd(t, cmd, "--yes")
	if CodeFor(err) == ExitNothingToDo {
		t.Fatalf("uninstall answered %d (nothing to do) while a startup file could not be scanned "+
			"for a ccdad block:\n%s", ExitNothingToDo, errOut)
	}
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, bashrc) {
		t.Errorf("the scan failure was swallowed; the user is never told which file to look at:\n%s", errOut)
	}
	if body := read(t, bashrc); body != stray {
		t.Errorf("the unreadable file was rewritten anyway:\n%s", body)
	}
}

// The fast, deterministic half of the non-regular-file rule. The FIFO in
// TestUninstallCleansEveryOtherStartupFileWhenOneIsUnusable is the real-world
// proof, but it kills a wrong implementation by HANGING, which is a bad way for
// a suite to fail. This one answers in microseconds.
func TestIsStartupFileAcceptsOnlyFilesItCanSafelyRewrite(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want bool
		why  string
	}{
		{"a regular file", regular, true, ""},
		{"a symlink to one", link, true, "a dotfiles repository is still a startup file"},
		{"a directory", dir, false, "os.ReadFile answers EISDIR, which used to abort the whole scan"},
		{"a fifo", fifo, false, "os.ReadFile on a named pipe blocks forever, with no output and no way out but Ctrl-C"},
		{"a dangling symlink", dangling, false, ""},
		{"nothing at all", filepath.Join(dir, "absent"), false, ""},
	}
	for _, tc := range cases {
		if got := isStartupFile(tc.path); got != tc.want {
			t.Errorf("isStartupFile(%s) = %v, want %v%s", tc.name, got, tc.want, because(tc.why))
		}
	}
}

// --print must not quietly hand over the block the write path refuses. On Linux
// os.Executable is fully resolved, so a Homebrew binary's directory is a
// VERSIONED one: a user who pastes it has a dead PATH entry the next time the
// package manager upgrades ccdad.
func TestSetupPathPrintWarnsForAPackageManagerInstall(t *testing.T) {
	setupPathWorld(t, "/bin/bash")
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "Cellar", "ccdad", "0.3.1", "bin", "ccdad"))

	code, stdout, errOut, _ := runRoot(t, "setup-path", "--print")
	if code != ExitOK {
		t.Fatalf("--print = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(errOut, "brew shellenv") {
		t.Errorf("--print handed over a block for a Homebrew install without naming Homebrew's own "+
			"instruction, while `ccdad setup-path` refuses the same install outright:\n%s", errOut)
	}
	if stdout == "" {
		t.Error("--print stopped printing; the note is a note, not a refusal")
	}
}

// setup-path writes the files of the shell it was told about; uninstall may run
// under a different one. Without the cross-shell scan the blocks survive and
// the command still reports a clean uninstall.
func TestUninstallFindsBlocksWrittenUnderOtherShells(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/bash")
	stubDaemonWorld(t, &fakeDaemon{})
	for _, shell := range []string{"bash", "fish"} {
		if code, _, errOut, _ := runRoot(t, "setup-path", "--shell", shell); code != ExitOK {
			t.Fatalf("setup-path --shell %s = %d\n%s", shell, code, errOut)
		}
	}
	written := []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".profile"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}

	// Uninstalling from a zsh prompt, which wrote none of them.
	t.Setenv("SHELL", "/bin/zsh")
	cmd := newUninstallCmd()
	err, _, errOut := runCmd(t, cmd, "--yes")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	for _, path := range written {
		if body := read(t, path); strings.Contains(body, setupPathBegin) {
			t.Errorf("%s still holds a ccdad block after uninstall:\n%s", path, body)
		}
		if !strings.Contains(errOut, path) {
			t.Errorf("uninstall never named %s:\n%s", path, errOut)
		}
	}
}

// A ZDOTDIR user's real .zshrc, which uninstall only reaches because the XDG
// default is scanned unconditionally: $ZDOTDIR is normally exported from
// ~/.zshenv, which an uninstall run from bash never sees.
func TestUninstallFindsAZdotdirBlockWithoutTheVariable(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/zsh")
	stubDaemonWorld(t, &fakeDaemon{})
	zdot := filepath.Join(home, ".config", "zsh")
	t.Setenv("ZDOTDIR", zdot)
	if code, _, errOut, _ := runRoot(t, "setup-path"); code != ExitOK {
		t.Fatalf("setup-path = %d\n%s", code, errOut)
	}
	rc := filepath.Join(zdot, ".zshrc")
	if !strings.Contains(read(t, rc), setupPathBegin) {
		t.Fatalf("setup-path did not write %s", rc)
	}

	// Uninstall from a shell that never exported ZDOTDIR.
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/bash")
	cmd := newUninstallCmd()
	if err, _, errOut := runCmd(t, cmd, "--yes"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, errOut)
	}
	if body := read(t, rc); strings.Contains(body, setupPathBegin) {
		t.Errorf("%s still holds a ccdad block; every new zsh terminal keeps prepending a directory "+
			"whose binary was just deleted:\n%s", rc, body)
	}
}

// Everything below ran in setuppath_test.go until the Windows leg failed on
// it. It is here because it does not describe a decision ccdad makes on two
// platforms — it describes what a POSIX shell does with the block, and what
// XDG_CONFIG_HOME means. Neither exists on Windows, where PATH is registered
// in the registry and there is no startup file to source.
//
// A build tag rather than a runtime skip, deliberately. This file's sibling
// says it of the fish leg: "a skip is the one test result that looks like
// coverage and is not". A skip would claim these could have run here. They
// could not, and nothing is lost by saying so: the block's TEXT is still
// asserted on every platform by the splice and render tests next door.

// The block is executed rather than merely spelled. Four of the defects this
// shape exists to prevent — a duplicate on every source, the working directory
// landing on PATH, an abort under `set -u`, and a directory with a `$` in it
// corrupting the line — are invisible to a string comparison and obvious to a
// shell. bash and dash are both here; every one of these runs in both.
func sourceBlock(t *testing.T, shell, dir, initialPATH string, opts ...string) string {
	t.Helper()
	return sourceBlockDirs(t, shell, []string{dir}, initialPATH, opts...)
}

// sourceBlockDirs is the same, for the block's other shape: a set of two
// directories, which is what a machine with the codex shim installed gets.
func sourceBlockDirs(t *testing.T, shell string, dirs []string, initialPATH string, opts ...string) string {
	t.Helper()
	bin, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s is not installed", shell)
	}
	file := filepath.Join(t.TempDir(), "block.sh")
	if err := os.WriteFile(file, []byte(renderBlock(dirs, shellPOSIX)), 0o644); err != nil {
		t.Fatalf("writing the block: %v", err)
	}
	script := strings.Join(opts, "\n") + "\n. " + file + "\n. " + file + "\n. " + file + "\nprintf %s \"$PATH\"\n"
	cmd := exec.Command(bin, "-c", script)
	// env -i: the block's behaviour must not depend on what the developer
	// exports. The PATH under test is passed in explicitly, or left unset.
	cmd.Env = []string{"PATH=" + initialPATH}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s rejected the block: %v\n%s\n--- block ---\n%s", shell, err, out, renderBlock(dirs, shellPOSIX))
	}
	return string(out)
}

func eachPOSIXShell(t *testing.T, run func(t *testing.T, shell string)) {
	t.Helper()
	for _, shell := range []string{"bash", "dash"} {
		t.Run(shell, func(t *testing.T) { run(t, shell) })
	}
}

func TestPOSIXBlockPrependsExactlyOnceHoweverOftenItIsSourced(t *testing.T) {
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		got := sourceBlock(t, shell, "/opt/ccdad/bin", "/usr/bin:/bin")
		if want := "/opt/ccdad/bin:/usr/bin:/bin"; got != want {
			t.Errorf("PATH after sourcing the block three times = %q, want %q: an unguarded "+
				"`export PATH=\"DIR:$PATH\"` compounds on every login and on every `. ~/.bashrc`", got, want)
		}
	})
}

func TestPOSIXBlockLeavesAnAlreadyRegisteredPATHExactlyAsItWas(t *testing.T) {
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		got := sourceBlock(t, shell, "/opt/ccdad/bin", "/usr/bin:/opt/ccdad/bin:/bin")
		if want := "/usr/bin:/opt/ccdad/bin:/bin"; got != want {
			t.Errorf("PATH = %q, want %q: the guard must not reorder a PATH that already has the directory", got, want)
		}
	})
}

func TestPOSIXBlockNeverPutsTheWorkingDirectoryOnPATH(t *testing.T) {
	// `export PATH="DIR:$PATH"` with PATH empty yields `DIR:`, and a trailing
	// empty component means the WORKING DIRECTORY — so every rc file written
	// that way runs `./ls` in any directory the user cd's into.
	//
	// The unset case is `unset PATH` INSIDE the script rather than an
	// environment with no PATH, and the difference is not cosmetic: both bash
	// and dash substitute a compiled-in default PATH at startup when the
	// variable is absent, so an env-level fixture never reaches the branch
	// under test. Verified — it produced the shell's default PATH, not the
	// empty one this pins.
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		for _, tc := range []struct {
			name    string
			initial string
			opts    []string
		}{
			{name: "PATH empty", initial: ""},
			{name: "PATH unset in the file", initial: "/usr/bin", opts: []string{"unset PATH"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := sourceBlock(t, shell, "/opt/ccdad/bin", tc.initial, tc.opts...)
				if want := "/opt/ccdad/bin"; got != want {
					t.Errorf("PATH = %q, want %q — a trailing separator is an empty component, which means the "+
						"working directory", got, want)
				}
			})
		}
	})
}

func TestPOSIXBlockSurvivesSetU(t *testing.T) {
	// A user's own `set -u` earlier in the rc file turns an unguarded $PATH
	// reference into an abort, which stops the rest of their startup file. The
	// two cases differ in which expansion is exercised: with PATH set it is the
	// `case` subject, with PATH unset it is both that and the value.
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		for _, tc := range []struct {
			name string
			opts []string
			want string
		}{
			{name: "PATH set", opts: []string{"set -u"}, want: "/opt/ccdad/bin:/usr/bin"},
			{name: "PATH unset", opts: []string{"set -u", "unset PATH"}, want: "/opt/ccdad/bin"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := sourceBlock(t, shell, "/opt/ccdad/bin", "/usr/bin", tc.opts...)
				if got != tc.want {
					t.Errorf("PATH under `set -u` = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

func TestPOSIXBlockQuotesADirectoryTheShellWouldOtherwiseInterpret(t *testing.T) {
	// CCDAD_INSTALL_DIR is a free string. A `$` in it silently truncates the
	// entry; a backtick executes a command at every shell start.
	dir := "/opt/we$ird/`touch " + t.TempDir() + "/pwned`/bin"
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		got := sourceBlock(t, shell, dir, "/usr/bin")
		if want := dir + ":/usr/bin"; got != want {
			t.Errorf("PATH = %q, want %q: the directory must survive interpolation verbatim", got, want)
		}
	})
}

func TestConfigHomeIgnoresARelativeXDGConfigHome(t *testing.T) {
	// `XDG_CONFIG_HOME=.config` and a quoted `~/.config` the shell never
	// expanded are both common. Honouring either makes setup-path create
	// ./.config/fish/config.fish under whatever directory the user was standing
	// in, report success, and leave a file fish never reads and uninstall can
	// never find.
	home := t.TempDir()
	for _, tc := range []struct{ name, value, want string }{
		{"unset", "", filepath.Join(home, ".config")},
		{"relative", ".config", filepath.Join(home, ".config")},
		{"unexpanded tilde", "~/.config", filepath.Join(home, ".config")},
		{"absolute", "/somewhere/cfg", "/somewhere/cfg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", tc.value)
			if got := configHome(home); got != tc.want {
				t.Errorf("configHome with XDG_CONFIG_HOME=%q = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// writeShimRecord puts the shim install record where registeredDirs looks for
// it, without running the install command. These tests are about the DERIVED
// SET, not about the writer, and coupling them to it would make one failure
// read as two.
func writeShimRecord(t *testing.T) {
	t.Helper()
	root := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schemaVersion":1,"path":"` + filepath.Join(shimDir(), "codex") +
		`","installedAt":"2026-09-02T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(root, shimRecordName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The block's directory set is derived on EVERY run, so a plain `ccdad
// setup-path` after a shim install keeps the shim directory instead of quietly
// dropping it. A set that was remembered rather than derived would be dropped
// by exactly the command a user runs to fix an unrelated PATH problem, and the
// symptom -- codex silently stops being routed through ccdad -- names neither
// command.
func TestSetupPathKeepsTheShimDirectoryOnALaterPlainRun(t *testing.T) {
	home, dir := setupPathWorld(t, "/bin/bash")
	writeShimRecord(t)

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitOK {
		t.Fatalf("setup-path = %d, want %d\n%s", code, ExitOK, errOut)
	}
	body := read(t, filepath.Join(home, ".bashrc"))
	if !strings.Contains(body, shimDir()) {
		t.Errorf("the block does not register the shim directory %s:\n%s", shimDir(), body)
	}
	if !strings.Contains(body, dir) {
		t.Errorf("the block dropped ccdad's own directory %s:\n%s", dir, body)
	}

	// And a second run changes nothing, which is what "derived" has to mean:
	// the same machine has to produce the same block.
	if code, _, _, _ := runRoot(t, "setup-path"); code != ExitNothingToDo {
		t.Errorf("a second setup-path = %d, want %d", code, ExitNothingToDo)
	}
	if again := read(t, filepath.Join(home, ".bashrc")); again != body {
		t.Errorf("the second run rewrote the file:\n--- first ---\n%s\n--- second ---\n%s", body, again)
	}
}

// The package-manager refusal applies to ccdad's OWN directory and to nothing
// else. Registering a versioned Cellar path is what that refusal exists to
// prevent; <CCDAD_HOME>/bin is ccdad's own directory whoever installed the
// binary, so a Homebrew ccdad still registers the shim -- otherwise every
// Homebrew user's codex silently bypasses ccdad and nothing says so.
func TestSetupPathRegistersTheShimDirectoryForAPackageManagerInstall(t *testing.T) {
	home, _ := setupPathWorld(t, "/bin/bash")
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "bin", "ccdad"))
	writeShimRecord(t)

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitOK {
		t.Fatalf("setup-path on a Homebrew ccdad with a shim installed = %d, want %d\n%s", code, ExitOK, errOut)
	}
	body := read(t, filepath.Join(home, ".bashrc"))
	if !strings.Contains(body, shimDir()) {
		t.Errorf("a Homebrew ccdad did not register the shim directory %s:\n%s", shimDir(), body)
	}
	if strings.Contains(body, filepath.Join(brew, "bin")) {
		t.Errorf("the block registered %s, which Homebrew manages:\n%s", filepath.Join(brew, "bin"), body)
	}
	if !strings.Contains(errOut, "Homebrew") {
		t.Errorf("the report does not say Homebrew still manages ccdad's own PATH:\n%s", errOut)
	}
}

// The control for both tests above: with no shim record the set is what it has
// always been, and a Homebrew install is still refused outright.
func TestSetupPathWithNoShimRecordStillRefusesAPackageManagerInstall(t *testing.T) {
	setupPathWorld(t, "/bin/bash")
	brew := filepath.Join(t.TempDir(), "homebrew")
	t.Setenv("HOMEBREW_PREFIX", brew)
	stubExecutable(t, filepath.Join(brew, "bin", "ccdad"))

	code, _, errOut, _ := runRoot(t, "setup-path")
	if code != ExitBlocked {
		t.Fatalf("setup-path on a Homebrew ccdad with no shim = %d, want %d\n%s", code, ExitBlocked, errOut)
	}
}

// The derived set is a LIST, and its order on PATH is the order it was given
// rather than the order the block writes. Each entry PREPENDS, so the renderer
// walks the set backwards; a forward walk reverses it silently.
func TestPOSIXBlockPutsTwoDirectoriesOnPATHInTheOrderTheyWereGiven(t *testing.T) {
	eachPOSIXShell(t, func(t *testing.T, shell string) {
		got := sourceBlockDirs(t, shell, []string{"/opt/ccdad", "/opt/ccdad/bin"}, "/usr/bin:/bin")
		if want := "/opt/ccdad:/opt/ccdad/bin:/usr/bin:/bin"; got != want {
			t.Errorf("PATH after sourcing a two-directory block three times = %q, want %q", got, want)
		}
	})
}
