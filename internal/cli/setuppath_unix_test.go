//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// The idempotence the brief calls the feature, and the exit code that reports
// it.
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
	// not. §9.3 keeps 2 for usage alone so a script can tell a typo from a
	// machine it cannot serve.
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
	if stdout != renderBlock(dir, shellBash) {
		t.Errorf("--print emitted\n%q\nwant the exact bytes the writer writes\n%q", stdout, renderBlock(dir, shellBash))
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
