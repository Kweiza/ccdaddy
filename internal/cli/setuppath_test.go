package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pure half of `ccdad setup-path`, which is everything that decides and
// nothing that writes. It lives in an untagged file for one reason: the
// decision is where duplicate PATH entries and missed removals come from, and
// this suite runs on Linux while half the decisions it makes are Windows's.
// install.ps1 made the same call first and said so — Get-CcdadUpdatedPath is
// "kept separate from the registry access so the decision can be tested off
// Windows" (install.ps1:157-163).
//
// Nothing in here may reach for path/filepath or os.PathListSeparator. Under
// `go test` on Linux those answer '/' and ':' for BOTH rule sets, so a Windows
// bug would pass here and ship.
//
// Two kinds of test are NOT here, and are in setuppath_unix_test.go behind
// `!windows`, because they are not decisions at all. The POSIX-block tests
// hand a real bash and dash a file to source, and a Windows ccdad never
// sources one — it registers PATH in the registry. The XDG_CONFIG_HOME test
// asserts that an absolute POSIX path is absolute, which filepath.IsAbs quite
// correctly denies on Windows.
//
// They were here at first, and the Windows leg is what said so. Their guard
// was an exec.LookPath on the shell, which reads as "skip where there is no
// shell" and is not one on that runner: Git for Windows ships BOTH bash and
// /usr/bin/dash, so the lookup succeeded for each, and the shell was then
// handed a `t.TempDir()` spelled C:\Users\RUNNER~1\... , whose backslashes it
// ate as escapes — `. C:UsersRUNNER~1AppData...: not found`, measured, in both
// shells. A guard that cannot fire is worse than none: it reads as though the
// case had been considered.

// The brief names the trap: `/home/u/.local/bin` is a substring of
// `/home/u/.local/bin2`, so matching must be by whole component. These are the
// cases a strings.Contains implementation gets wrong, plus the ones a naive
// split gets wrong.
func TestOnPathListMatchesWholeComponentsOnly(t *testing.T) {
	const dir = "/home/u/.local/bin"
	cases := []struct {
		name string
		list string
		want bool
		why  string
	}{
		{"exact single", "/home/u/.local/bin", true, "the only entry is the directory"},
		{"first of many", "/home/u/.local/bin:/usr/bin", true, ""},
		{"last of many", "/usr/bin:/home/u/.local/bin", true, ""},
		{"middle", "/usr/bin:/home/u/.local/bin:/bin", true, ""},
		{"longer sibling", "/home/u/.local/bin2", false,
			"the brief's trap: a substring match says yes here and a component match says no"},
		{"longer sibling among many", "/usr/bin:/home/u/.local/bin2:/bin", false, ""},
		{"prefix of the entry", "/home/u/.local", false, "a parent directory is not the directory"},
		{"entry is a prefix of ours", "/home/u/.local/bin/deeper", false, ""},
		{"empty list", "", false, "an empty PATH holds nothing, and must not be read as one empty component"},
		{"empty component", "::", false, "an empty component means the working directory, never a named one"},
		{"empty component beside ours", ":/home/u/.local/bin:", true, ""},
		{"trailing slash in the list", "/home/u/.local/bin/", true,
			"the shell resolves both spellings to the same directory, so a second run must not add a duplicate"},
		{"the root directory is not our directory", "/", false, ""},
		{"case differs", "/home/u/.LOCAL/bin", false, "Unix paths are case-sensitive; this is the one rule Windows inverts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := onPathList(tc.list, dir, unixPathRules); got != tc.want {
				t.Errorf("onPathList(%q, %q, unix) = %v, want %v%s", tc.list, dir, got, tc.want, because(tc.why))
			}
		})
	}
}

// Windows inverts two of those rules and adds a third. Running this table on
// Linux is the entire point: windowsPathRules must not consult the host.
func TestOnPathListUsesWindowsRulesForWindowsLists(t *testing.T) {
	const dir = `C:\Users\u\AppData\Local\Programs\ccdad`
	cases := []struct {
		name string
		list string
		want bool
		why  string
	}{
		{"exact", `C:\Users\u\AppData\Local\Programs\ccdad`, true, ""},
		{"among many", `C:\Windows;C:\Users\u\AppData\Local\Programs\ccdad;C:\Windows\System32`, true, ""},
		{"case differs", `c:\users\u\appdata\local\programs\CCDAD`, true,
			"Windows paths are case-insensitive, so a case-sensitive compare appends a duplicate on every install"},
		{"trailing backslash", `C:\Users\u\AppData\Local\Programs\ccdad\`, true,
			"a trailing separator is not part of the name Windows stores"},
		{"longer sibling", `C:\Users\u\AppData\Local\Programs\ccdad2`, false,
			"the substring trap survives the switch to ';' and to case folding"},
		{"colon is not a separator here", `C:\Windows:C:\Users\u\AppData\Local\Programs\ccdad`, false,
			"splitting a Windows PATH on ':' cuts every entry in half at the drive letter"},
		{"empty component", `;;`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := onPathList(tc.list, dir, windowsPathRules); got != tc.want {
				t.Errorf("onPathList(%q, %q, windows) = %v, want %v%s", tc.list, dir, got, tc.want, because(tc.why))
			}
		})
	}
}

func because(why string) string {
	if why == "" {
		return ""
	}
	return ": " + why
}

// expandFake stands in for registry.ExpandString, which exists only on
// Windows. The one it models is the reason these tests exist at all: the user
// PATH is read UNEXPANDED, so a component can be spelled `%LOCALAPPDATA%\...`
// while the directory ccdad resolved is the expanded form.
func expandFake(s string) string {
	return strings.ReplaceAll(s, `%LOCALAPPDATA%`, `C:\Users\u\AppData\Local`)
}

func TestUserPathWithDirAppendsOnlyWhenTheEntryIsMissing(t *testing.T) {
	const dir = `C:\Users\u\AppData\Local\Programs\ccdad`
	cases := []struct {
		name        string
		current     string
		wantUpdated string
		wantChanged bool
		why         string
	}{{
		name: "absent", current: `C:\Windows`,
		wantUpdated: `C:\Windows;` + dir, wantChanged: true,
		why: "appended, not prepended — install.ps1 appends and the two must agree",
	}, {
		name: "no value at all", current: "",
		wantUpdated: dir, wantChanged: true,
		why: "a single entry must not be glued onto an empty string with a separator",
	}, {
		name: "already there", current: `C:\Windows;` + dir,
		wantUpdated: "", wantChanged: false,
		why: "idempotence IS the feature",
	}, {
		name: "already there, different case", current: `C:\Windows;c:\users\u\appdata\local\programs\ccdad`,
		wantUpdated: "", wantChanged: false,
	}, {
		name: "already there, trailing backslash", current: dir + `\`,
		wantUpdated: "", wantChanged: false,
	}, {
		name: "already there, spelled with %LOCALAPPDATA%", current: `%LOCALAPPDATA%\Programs\ccdad`,
		wantUpdated: "", wantChanged: false,
		why: "install.ps1 shipped reading the value unexpanded and comparing it against an expanded " +
			"directory, so it appended a second, expanded copy on every install. Both apply this rule now, " +
			"and they must not drift apart",
	}, {
		name: "surviving %VAR% entries are written back unexpanded", current: `%SystemRoot%\system32;C:\Windows`,
		wantUpdated: `%SystemRoot%\system32;C:\Windows;` + dir, wantChanged: true,
		why: "expanding what we did not have to touch bakes today's values into the user's PATH forever",
	}, {
		name: "a longer sibling is not this entry", current: dir + `2`,
		wantUpdated: dir + `2;` + dir, wantChanged: true,
	}, {
		name: "an empty component is dropped on a run that writes", current: `C:\Windows;;C:\Windows\System32`,
		wantUpdated: `C:\Windows;C:\Windows\System32;` + dir, wantChanged: true,
		why: "an empty component means the working directory, and install.ps1 drops them too " +
			"(`Where-Object { $_ -ne '' }`); the two must agree or the same machine gets a different " +
			"PATH depending on which one ran",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, changed := userPathWithDir(tc.current, dir, expandFake)
			if changed != tc.wantChanged {
				t.Fatalf("userPathWithDir(%q) changed = %v, want %v%s", tc.current, changed, tc.wantChanged, because(tc.why))
			}
			if updated != tc.wantUpdated {
				t.Errorf("userPathWithDir(%q) = %q, want %q%s", tc.current, updated, tc.wantUpdated, because(tc.why))
			}
		})
	}
}

func TestUserPathWithoutDirRemovesEveryCopyAndNothingElse(t *testing.T) {
	const dir = `C:\Users\u\AppData\Local\Programs\ccdad`
	cases := []struct {
		name        string
		current     string
		wantUpdated string
		wantRemoved bool
		why         string
	}{{
		name: "one copy among others", current: `C:\Windows;` + dir + `;C:\Windows\System32`,
		wantUpdated: `C:\Windows;C:\Windows\System32`, wantRemoved: true,
	}, {
		name: "two copies", current: dir + `;C:\Windows;` + dir + `\`,
		wantUpdated: `C:\Windows`, wantRemoved: true,
		why: "a re-install that duplicated the entry must not survive one uninstall",
	}, {
		name: "spelled with %LOCALAPPDATA%", current: `C:\Windows;%LOCALAPPDATA%\Programs\ccdad`,
		wantUpdated: `C:\Windows`, wantRemoved: true,
		why: "the entry install.ps1's own bug creates is still ours to remove",
	}, {
		name: "not there", current: `C:\Windows`,
		wantUpdated: "", wantRemoved: false,
		why: "an unchanged PATH must not be rewritten — the empty-component drop would edit it for no reason",
	}, {
		name: "not there, and holding an empty component", current: `C:\Windows;;C:\Windows\System32`,
		wantUpdated: "", wantRemoved: false,
		why: "the same rule, on the input that makes a rewrite visible",
	}, {
		name: "the only entry", current: dir,
		wantUpdated: "", wantRemoved: true,
		why: "an empty result is written as the empty string, so the value keeps the kind we preserved",
	}, {
		name: "a longer sibling is left alone", current: dir + `2`,
		wantUpdated: "", wantRemoved: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, removed := userPathWithoutDir(tc.current, dir, expandFake)
			if removed != tc.wantRemoved {
				t.Fatalf("userPathWithoutDir(%q) removed = %v, want %v%s", tc.current, removed, tc.wantRemoved, because(tc.why))
			}
			if updated != tc.wantUpdated {
				t.Errorf("userPathWithoutDir(%q) = %q, want %q%s", tc.current, updated, tc.wantUpdated, because(tc.why))
			}
		})
	}
}

// blockFor is the block these splice tests write, kept short so the fixtures
// below read as file shapes rather than as shell.
func blockFor(dir string) string { return renderBlock(dir, shellPOSIX) }

func TestSpliceBlockAppendsWithoutDamagingWhatWasThere(t *testing.T) {
	block := blockFor("/opt/ccdad/bin")
	cases := []struct {
		name     string
		existing string
		want     string
		why      string
	}{{
		name: "empty file", existing: "", want: block,
		why: "a file ccdad creates starts with the block, not with a blank line",
	}, {
		name: "ordinary file", existing: "export EDITOR=vi\n",
		want: "export EDITOR=vi\n\n" + block,
		why:  "one blank line separates the block from the user's last line",
	}, {
		name: "no trailing newline", existing: "export EDITOR=vi",
		want: "export EDITOR=vi\n\n" + block,
		why: "appending to a file that does not end in a newline glues the fence onto the user's " +
			"last command, which both breaks that command and hides the fence from the next run",
	}, {
		name: "file that is only a newline", existing: "\n", want: "\n\n" + block,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := spliceBlock([]byte(tc.existing), block)
			if err != nil {
				t.Fatalf("spliceBlock: %v", err)
			}
			if !changed {
				t.Fatal("spliceBlock reported no change while appending a block")
			}
			if string(got) != tc.want {
				t.Errorf("spliceBlock(%q) =\n%q\nwant\n%q%s", tc.existing, got, tc.want, because(tc.why))
			}
		})
	}
}

func TestSpliceBlockRewritesInPlaceAndReportsAnUnchangedFile(t *testing.T) {
	const before = "export EDITOR=vi\n\n"
	const after = "\n# the user's own last word\nalias ll='ls -l'\n"
	old := blockFor("/old/bin")
	fresh := blockFor("/opt/ccdad/bin")

	// A second run with a MOVED install directory rewrites the block where it
	// stands. Content on both sides has to survive, and the after-content is
	// the half a replace-to-EOF implementation destroys.
	got, changed, err := spliceBlock([]byte(before+old+after), fresh)
	if err != nil {
		t.Fatalf("spliceBlock: %v", err)
	}
	if !changed {
		t.Fatal("spliceBlock reported no change while replacing a block that named a different directory")
	}
	if want := before + fresh + after; string(got) != want {
		t.Fatalf("spliceBlock =\n%q\nwant\n%q", got, want)
	}

	// The idempotence the brief calls the feature: run it twice, and the second
	// run writes nothing at all.
	again, changed, err := spliceBlock(got, fresh)
	if err != nil {
		t.Fatalf("spliceBlock (second run): %v", err)
	}
	if changed {
		t.Errorf("the second run reported a change; it would rewrite the file for nothing")
	}
	if string(again) != string(got) {
		t.Errorf("the second run altered the file:\n%q\nwant\n%q", again, got)
	}
}

func TestSpliceBlockCollapsesDuplicatedBlocks(t *testing.T) {
	// Two complete pairs is what an older ccdad, or a user pasting `--print`
	// output twice, leaves behind. Rewriting the first and leaving the second
	// would make every subsequent run report "unchanged" while a stale block
	// still names the old directory.
	block := blockFor("/opt/ccdad/bin")
	old := blockFor("/old/bin")
	existing := "a=1\n" + old + "b=2\n" + old + "c=3\n"
	got, changed, err := spliceBlock([]byte(existing), block)
	if err != nil {
		t.Fatalf("spliceBlock: %v", err)
	}
	if !changed {
		t.Fatal("spliceBlock reported no change on a file holding two stale blocks")
	}
	if want := "a=1\n" + block + "b=2\nc=3\n"; string(got) != want {
		t.Errorf("spliceBlock =\n%q\nwant\n%q", got, want)
	}
	if n := strings.Count(string(got), setupPathBegin); n != 1 {
		t.Errorf("the file holds %d ccdad blocks, want exactly 1", n)
	}
}

// A marker is a WHOLE line. A user's own script that mentions one — a grep, an
// echo, a comment about the block — is not a fence, and reading it as one
// rewrites everything from there to the next marker.
func TestSpliceBlockDoesNotMistakeAMentionOfTheMarkerForAFence(t *testing.T) {
	block := blockFor("/opt/ccdad/bin")
	existing := "grep '" + setupPathBegin + "' ~/.bashrc\n" +
		"echo \"" + setupPathEnd + "\"\n"
	got, changed, err := spliceBlock([]byte(existing), block)
	if err != nil {
		t.Fatalf("spliceBlock: %v", err)
	}
	if !changed {
		t.Fatal("spliceBlock found a fence in two lines that only MENTION the markers")
	}
	if want := existing + "\n" + block; string(got) != want {
		t.Errorf("spliceBlock =\n%q\nwant the user's lines untouched and the block appended\n%q", got, want)
	}
}

func TestSpliceBlockRefusesAnUnterminatedFence(t *testing.T) {
	// A user who half-deleted the block by hand. Treating EOF as the closing
	// fence would delete the rest of their rc file; appending instead would
	// leave the stray marker to swallow the new block on the run after.
	existing := "a=1\n" + setupPathBegin + "\nexport PATH=/old/bin:$PATH\nb=2\n"
	if _, _, err := spliceBlock([]byte(existing), blockFor("/opt/ccdad/bin")); err == nil {
		t.Fatal("spliceBlock accepted a file whose opening fence has no closing fence")
	} else if !strings.Contains(err.Error(), setupPathEnd) {
		t.Errorf("the error does not name the missing marker, so the user cannot fix it: %v", err)
	}
}

func TestSpliceBlockFindsAFenceInACRLFFile(t *testing.T) {
	// An rc file edited on Windows, or synced from one. A fence match that does
	// not tolerate the \r appends a duplicate on every run; and our own lines
	// must stay LF, because a \r at the end of a PATH element breaks every
	// command in that directory.
	block := blockFor("/opt/ccdad/bin")
	existing := "a=1\r\n" + strings.ReplaceAll(blockFor("/old/bin"), "\n", "\r\n") + "b=2\r\n"
	got, changed, err := spliceBlock([]byte(existing), block)
	if err != nil {
		t.Fatalf("spliceBlock: %v", err)
	}
	if !changed {
		t.Fatal("spliceBlock did not find the fence in a CRLF file, so a second run would duplicate the block")
	}
	if want := "a=1\r\n" + block + "b=2\r\n"; string(got) != want {
		t.Errorf("spliceBlock =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(strings.TrimSuffix(block, "\n"), "\r") {
		t.Error("the block ccdad writes carries a carriage return")
	}
}

func TestRemoveBlockRoundTripsToTheOriginalBytes(t *testing.T) {
	// The property that makes `ccdad uninstall` safe to run: what setup-path
	// added is exactly what removal takes away, byte for byte, including the
	// blank line it inserted.
	for _, existing := range []string{
		"",
		"export EDITOR=vi\n",
		"export EDITOR=vi",
		"a=1\n\n\nb=2\n",
		"a=1\r\nb=2\r\n",
	} {
		t.Run(strings.ReplaceAll(existing, "\n", "\\n"), func(t *testing.T) {
			spliced, _, err := spliceBlock([]byte(existing), blockFor("/opt/ccdad/bin"))
			if err != nil {
				t.Fatalf("spliceBlock: %v", err)
			}
			got, removed, err := removeBlockFrom(spliced)
			if err != nil {
				t.Fatalf("removeBlockFrom: %v", err)
			}
			if !removed {
				t.Fatal("removeBlockFrom found no block in a file spliceBlock had just written one into")
			}
			// The one licensed difference: a file that did not end in a newline
			// gets one, because appending had to add it and removal cannot know
			// it was not the user's.
			want := existing
			if want != "" && !strings.HasSuffix(want, "\n") {
				want += "\n"
			}
			if string(got) != want {
				t.Errorf("round trip left\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestRemoveBlockLeavesAFileWithNoBlockAlone(t *testing.T) {
	existing := []byte("export EDITOR=vi\n# ccdad is great\n")
	got, removed, err := removeBlockFrom(existing)
	if err != nil {
		t.Fatalf("removeBlockFrom: %v", err)
	}
	if removed {
		t.Error("removeBlockFrom reported a removal from a file with no ccdad block; " +
			"uninstall would rewrite a file it never wrote")
	}
	if string(got) != string(existing) {
		t.Errorf("removeBlockFrom altered a file with no block:\n%q", got)
	}
}

// The rc-file choice is where these break. Every row is a real machine shape,
// and the two that matter most are the bash ones: bash reads the FIRST EXISTING
// of ~/.bash_profile, ~/.bash_login and ~/.profile and skips ~/.bashrc entirely
// in a login shell, while a Linux terminal emulator starts a non-login
// interactive shell that reads ~/.bashrc and none of the other three. macOS
// Terminal starts a login shell every time. Writing one file is broken on one
// of those platforms whichever file is chosen.
func TestTargetFilesPicksTheFilesTheShellActuallyReads(t *testing.T) {
	cases := []struct {
		name    string
		shell   shellKind
		seed    []string
		env     map[string]string
		want    []string
		wantErr string
		why     string
	}{{
		name: "bash with no login file at all", shell: shellBash,
		want: []string{".bashrc", ".profile"},
		why: "creating ~/.bash_profile would silently stop every bash login shell from ever reading " +
			"~/.profile again, which on a stock Debian or Ubuntu home disables ~/bin, ~/.local/bin and " +
			"the ~/.bashrc chain in one write",
	}, {
		name: "bash with a .bash_profile", shell: shellBash, seed: []string{".bash_profile", ".profile"},
		want: []string{".bashrc", ".bash_profile"},
		why:  "bash reads the first that EXISTS, and .profile is never reached once .bash_profile is there",
	}, {
		name: "bash with a .bash_login but no .bash_profile", shell: shellBash, seed: []string{".bash_login"},
		want: []string{".bashrc", ".bash_login"},
	}, {
		name: "bash with only a .profile", shell: shellBash, seed: []string{".profile"},
		want: []string{".bashrc", ".profile"},
	}, {
		name: "zsh", shell: shellZsh, want: []string{".zshrc"},
		why: "read by every INTERACTIVE zsh, login or not — the union of macOS Terminal and a Linux " +
			"terminal emulator. .zshenv would also run for every `zsh -c` and every zsh script",
	}, {
		name: "zsh with ZDOTDIR set", shell: shellZsh, env: map[string]string{"ZDOTDIR": "@/zdot"},
		want: []string{"@/zdot/.zshrc"},
	}, {
		name: "sh", shell: shellPOSIX, want: []string{".profile"},
	}, {
		name: "fish", shell: shellFish, want: []string{".config/fish/config.fish"},
	}, {
		name: "fish with XDG_CONFIG_HOME", shell: shellFish, env: map[string]string{"XDG_CONFIG_HOME": "@/xdg"},
		want: []string{"@/xdg/fish/config.fish"},
	}, {
		name: "bash with both a .bash_profile and a .bash_login", shell: shellBash,
		seed: []string{".bash_profile", ".bash_login", ".profile"},
		want: []string{".bashrc", ".bash_profile"},
		why: "bash reads the FIRST of the three that exists, so the order is the whole rule; with the scan " +
			"reversed the block lands in a file bash never reaches while .bash_profile is there",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := setupPathHome(t, tc.seed...)
			for k, v := range tc.env {
				t.Setenv(k, strings.ReplaceAll(v, "@", home))
			}
			got, err := targetFiles(tc.shell)
			if err != nil {
				t.Fatalf("targetFiles(%v): %v", tc.shell, err)
			}
			want := make([]string, len(tc.want))
			for i, w := range tc.want {
				if strings.HasPrefix(w, "@") {
					want[i] = filepath.Join(home, strings.TrimPrefix(w, "@/"))
					continue
				}
				want[i] = filepath.Join(home, w)
			}
			if strings.Join(got, string(filepath.ListSeparator)) != strings.Join(want, string(filepath.ListSeparator)) {
				t.Errorf("targetFiles(%v) = %v, want %v%s", tc.shell, got, want, because(tc.why))
			}
		})
	}
}

// A ZDOTDIR that lives inside ~/.zshenv is invisible to a ccdad started from
// bash, from a Makefile or from an installer, and writing ~/.zshrc for such a
// user creates a file zsh will never read — while reporting success.
func TestTargetFilesRefusesWhenZshConfigHasBeenRelocatedOutOfSight(t *testing.T) {
	home := setupPathHome(t)
	if err := os.WriteFile(filepath.Join(home, ".zshenv"),
		[]byte("export ZDOTDIR=$HOME/.config/zsh\n"), 0o644); err != nil {
		t.Fatalf("seeding .zshenv: %v", err)
	}
	if _, err := targetFiles(shellZsh); err == nil {
		t.Fatal("targetFiles wrote ~/.zshrc for a user whose .zshenv relocates ZDOTDIR; " +
			"zsh would never read that file and the command would report success")
	} else if !strings.Contains(err.Error(), "ZDOTDIR") {
		t.Errorf("the refusal does not name ZDOTDIR, so the user cannot act on it: %v", err)
	}

	// With ZDOTDIR actually in the environment there is nothing to guess at.
	zdot := filepath.Join(home, ".config", "zsh")
	t.Setenv("ZDOTDIR", zdot)
	got, err := targetFiles(shellZsh)
	if err != nil {
		t.Fatalf("targetFiles with ZDOTDIR set: %v", err)
	}
	if want := filepath.Join(zdot, ".zshrc"); len(got) != 1 || got[0] != want {
		t.Errorf("targetFiles = %v, want [%s]", got, want)
	}
}

// setupPathHome points the home directory at a temp dir and seeds the named
// files in it. isolate() already sandboxes HOME for the whole package; this is
// for the tests that do not build a command and still must not touch the
// developer's own dotfiles.
func setupPathHome(t *testing.T, seed ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, name := range seed {
		if err := os.WriteFile(filepath.Join(home, name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	return home
}

// The fish block, executed. It is skipped on the machine this was written on —
// fish is not installed there, and /etc/shells lists only sh, bash, rbash and
// dash — so the fish leg's syntax is asserted as text by
// TestSetupPathWritesFishSyntaxIntoConfigFish and PROVEN only where fish
// exists. That gap is real and is disclosed rather than papered over with a
// mock that would pass on any machine.
func TestFishBlockRunsUnderFish(t *testing.T) {
	bin, err := exec.LookPath("fish")
	if err != nil {
		// CI installs fish on the Linux leg and sets this, so that the step
		// silently breaking — a renamed package, a changed image — turns this
		// back into a skip that still reports green. A skip is the one test
		// result that looks like coverage and is not.
		if os.Getenv("CCDAD_REQUIRE_FISH") != "" {
			t.Fatal("CCDAD_REQUIRE_FISH is set but fish is not on PATH; the fish block would go back to " +
				"being asserted as text and never executed")
		}
		t.Skip("fish is not installed; the fish block is asserted as text and never executed here")
	}
	file := filepath.Join(t.TempDir(), "block.fish")
	if err := os.WriteFile(file, []byte(renderBlock("/opt/ccdad/bin", shellFish)), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "set -gx PATH /usr/bin /bin\nsource " + file + "\nsource " + file + "\nsource " + file +
		"\nprintf '%s' (string join ':' $PATH)\n"
	out, err := exec.Command(bin, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("fish rejected the block: %v\n%s", err, out)
	}
	if want := "/opt/ccdad/bin:/usr/bin:/bin"; string(out) != want {
		t.Errorf("PATH after sourcing the fish block three times = %q, want %q", out, want)
	}
}

// shellFor's whole domain. Without this the POSIX aliases, plain csh and both
// name-stripping rules can be deleted with the suite still green — and a
// /bin/dash login shell (most Debian service accounts, and `docker exec` in a
// slim image) would be told ccdad does not know how to write a startup file for
// it, while --shell sh, which the flag's own help advertises, became a usage
// error.
func TestShellForReadsEveryNameItAdvertises(t *testing.T) {
	cases := []struct {
		name string
		want shellKind
		why  string
	}{
		{"bash", shellBash, ""},
		{"/bin/bash", shellBash, "a $SHELL is a path, not a bare name"},
		{"-bash", shellBash, "a login shell's argv[0] is conventionally the name with a leading hyphen"},
		{"/usr/local/bin/BASH", shellBash, "case is not part of which shell this is"},
		{"zsh", shellZsh, ""},
		{"/usr/bin/zsh", shellZsh, ""},
		{"fish", shellFish, ""},
		{"/opt/homebrew/bin/fish", shellFish, "Homebrew's fish is not in /usr/bin"},
		{"sh", shellPOSIX, ""},
		{"dash", shellPOSIX, "Debian's /bin/sh, and the login shell of most service accounts"},
		{"ash", shellPOSIX, ""},
		{"ksh", shellPOSIX, ""},
		{"ksh93", shellPOSIX, ""},
		{"mksh", shellPOSIX, ""},
		{"pdksh", shellPOSIX, ""},
		{"busybox", shellPOSIX, ""},
		{"csh", shellCsh, "csh shares no PATH syntax with the block, so it must not fall into the POSIX arm"},
		{"tcsh", shellCsh, ""},
		{"", shellUnknown, "an unset $SHELL is not a shell to guess at"},
		{"nu", shellUnknown, ""},
		{"pwsh.exe", shellUnknown, "the .exe is stripped, and pwsh is still not a shell this writes files for"},
		{"powershell", shellUnknown, ""},
	}
	for _, tc := range cases {
		if got := shellFor(tc.name); got != tc.want {
			t.Errorf("shellFor(%q) = %v, want %v%s", tc.name, got, tc.want, because(tc.why))
		}
	}
}

// A commented-out or negated ZDOTDIR must not refuse: the refusal has no way
// through — from a zsh prompt the environment is identical — so a bare-word
// match locks a perfectly serveable user out of the write path forever.
func TestAssignsZDOTDIRWantsAnAssignmentNotTheWord(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"an export", "export ZDOTDIR=$HOME/.config/zsh\n", true},
		{"a bare assignment", "ZDOTDIR=$HOME/.config/zsh\n", true},
		{"indented", "\t  export ZDOTDIR=/x\n", true},
		{"among other lines", "setopt no_beep\nZDOTDIR=/x\nexport LANG=C\n", true},
		{"commented out", "# export ZDOTDIR=\"$HOME/.config/zsh\"   # I turned this off\n", false},
		{"a comment about it", "# ZDOTDIR is not used here; my config lives in ~/.zshrc\n", false},
		{"unset", "unset ZDOTDIR\n", false},
		{"only mentioned in a conditional test", "[[ -n $ZDOTDIR ]] && echo relocated\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := assignsZDOTDIR([]byte(tc.body)); got != tc.want {
			t.Errorf("assignsZDOTDIR(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// A stray opening marker ABOVE a real block. Taking the real block's END as the
// stray's close makes the "block" span every line between them — and removal
// deletes that span, so the user's aliases and `source ~/.secrets` go with it.
func TestSpliceBlockRefusesAStrayMarkerAboveARealBlock(t *testing.T) {
	block := blockFor("/opt/ccdad/bin")
	existing := "export EDITOR=vim\n" +
		setupPathBegin + "\n" +
		"export AWS_PROFILE=work\n" +
		"alias k=kubectl\n" +
		block
	for _, tc := range []struct {
		name string
		run  func() ([]byte, bool, error)
	}{
		{"splice", func() ([]byte, bool, error) { return spliceBlock([]byte(existing), block) }},
		{"remove", func() ([]byte, bool, error) { return removeBlockFrom([]byte(existing)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := tc.run()
			if err == nil {
				t.Fatalf("accepted a file with a stray opening marker above a real block, and returned:\n%q\n"+
					"the user's own lines between the two markers are inside what removal deletes", got)
			}
			if changed {
				t.Error("reported a change alongside the refusal")
			}
			if !strings.Contains(err.Error(), setupPathBegin) {
				t.Errorf("the error does not name the marker the user has to find: %v", err)
			}
		})
	}
}
