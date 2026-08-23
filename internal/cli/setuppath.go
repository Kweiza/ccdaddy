package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// `ccdad setup-path` is the other half of the installer's rule against editing
// a shell profile. `curl | bash` puts the script itself on stdin, so install.sh
// cannot prompt and must never guess at a startup file — which leaves "ccdad:
// command not found" as the last thing a successful install can produce. This
// command is the whole answer to it, and both installers point at it.
//
// Everything that DECIDES lives in this file and everything that WRITES lives
// in the build-tagged pair beside it. That split is not tidiness: the decision
// is where duplicate PATH entries and missed removals come from, and this
// package's tests run on Linux while half the decisions are Windows's.
// install.ps1 made the same call first, and Get-CcdadUpdatedPath's own
// .DESCRIPTION says why. (Names, not line numbers: the citations in this file
// were stale within one branch.)
//
// The rule that keeps the PATH-list half honest: pathRules and everything that
// consults it are plain `strings` work over ':' , ';' and '\\' as literals.
// Never path/filepath, never os.PathListSeparator — under `go test` on Linux
// those answer '/' and ':' for BOTH rule sets, so a Windows bug written with
// them passes here and ships. The file's other half, which resolves startup
// files under a home directory, is ordinary os and filepath code.

// pathRules is how one platform reads a PATH list. Two values of it exist and
// neither is chosen by the host: the tagged files pick one, and the tests
// exercise both on whatever machine they run on.
type pathRules struct {
	// sep separates components: ':' on Unix, ';' on Windows. It is a byte
	// rather than os.PathListSeparator for the reason in the file comment.
	sep byte
	// foldCase compares components case-insensitively. Windows only —
	// case-folding a Unix PATH would make /home/u/Bin and /home/u/bin the same
	// directory, which they are not.
	foldCase bool
	// trailing lists the separators a component may end in without that
	// character being part of the name. The shell resolves `/a/bin` and
	// `/a/bin/` to one directory, so a second run that did not trim would
	// append a duplicate.
	trailing string
}

var (
	unixPathRules    = pathRules{sep: ':', trailing: "/"}
	windowsPathRules = pathRules{sep: ';', foldCase: true, trailing: `\/`}
)

// split cuts a PATH list into its named components, dropping the empty ones.
//
// An empty component is not a directory that failed to match — it MEANS the
// working directory, which is why it must never compare equal to anything
// named. Dropping them here also keeps `A;;B` from being silently rewritten to
// `A;B` by a run that was supposed to change nothing: the callers that write
// only do so when something actually changed.
func (r pathRules) split(list string) []string {
	if list == "" {
		return nil
	}
	parts := strings.Split(list, string(r.sep))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalize trims a trailing separator, without ever emptying the component:
// "/" is the root directory and not a spelling of "".
//
// Disclosed: no input can currently tell that guard apart from its absence.
// Both sides of every comparison go through here, so an entry that reduces to
// "" is only ever compared with a dir that reduces the same way, and split()
// drops the empty components that would otherwise arrive on their own. It is
// kept because it stops being unreachable the moment either of those two facts
// changes, and a root directory silently equal to "" is a bad way to find out.
func (r pathRules) normalize(entry string) string {
	trimmed := strings.TrimRight(entry, r.trailing)
	if trimmed == "" {
		return entry
	}
	return trimmed
}

// same reports whether two PATH components name the same directory under these
// rules.
func (r pathRules) same(a, b string) bool {
	a, b = r.normalize(a), r.normalize(b)
	if r.foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// onPathList reports whether dir is one of the components of list.
//
// Whole components, never a substring. The trap is a real directory pair —
// `/home/u/.local/bin` is a substring of `/home/u/.local/bin2`, so a
// strings.Contains here answers "already on PATH" for a machine where ccdad
// is not on PATH at all.
func onPathList(list, dir string, r pathRules) bool {
	for _, entry := range r.split(list) {
		if r.same(entry, dir) {
			return true
		}
	}
	return false
}

// matchesEntry reports whether one stored user-PATH component names dir, under
// r's rules, comparing the component BOTH as stored and as expanded.
//
// The second comparison is the one install.ps1 shipped without. Add-CcdadToUserPath
// reads the value with DoNotExpandEnvironmentNames — correctly, because writing
// back an expanded PATH would bake today's values in forever — and
// Get-CcdadUpdatedPath then compared each component against a fully expanded
// install directory. A user whose Path held `%LOCALAPPDATA%\Programs\ccdad`
// therefore failed the equality test and got a second, expanded copy appended
// on every install. Both now apply this rule; they must not drift apart, since
// either may be the one that registered the entry uninstall later removes.
func matchesEntry(entry, dir string, r pathRules, expand func(string) string) bool {
	if r.same(entry, dir) {
		return true
	}
	if expand == nil {
		return false
	}
	return r.same(expand(entry), dir)
}

// userPathWithDir is the Windows user-PATH value with dir added, and whether
// anything changed. An unchanged value comes back as "" so that no caller can
// write one by accident.
//
// It APPENDS, while the Unix block prepends, and the asymmetry is deliberate
// rather than an oversight in one of the two. install.ps1 has appended since it
// shipped and matching it is what keeps a `setup-path` after an installer run
// from disagreeing with the installer; on Unix the line ccdad prints is the one
// install.sh has always printed, which prepends. The visible consequence is
// stated in the report: on Windows an earlier PATH entry still wins.
func userPathWithDir(current, dir string, expand func(string) string) (string, bool) {
	r := windowsPathRules
	entries := r.split(current)
	for _, entry := range entries {
		if matchesEntry(entry, dir, r, expand) {
			return "", false
		}
	}
	return strings.Join(append(entries, dir), string(r.sep)), true
}

// userPathWithoutDir is the Windows user-PATH value with every copy of dir
// removed, and whether any were. It is a separate function from its opposite
// rather than one function with a mode, because the two disagree at both ends:
// adding to an absent value creates one, and removing the last entry leaves the
// empty string rather than deleting the value.
//
// EVERY copy, not the first: a re-install that duplicated the entry — which is
// exactly what install.ps1's missing expansion match produced — must not
// survive one uninstall.
func userPathWithoutDir(current, dir string, expand func(string) string) (string, bool) {
	r := windowsPathRules
	entries := r.split(current)
	kept := make([]string, 0, len(entries))
	for _, entry := range entries {
		if matchesEntry(entry, dir, r, expand) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == len(entries) {
		// Nothing of ours was there. Returning "" and false is what stops the
		// caller writing: split() drops empty components, so a rewrite here
		// would silently turn `A;;B` into `A;B` on a run that had nothing to do.
		return "", false
	}
	return strings.Join(kept, string(r.sep)), true
}

// The marker fence. Conda's shape, because it is the one users have already
// seen in their own rc files, and because two distinct lines make the pair
// findable without a regexp over the interior.
//
// Both are matched on the WHOLE line — never with strings.Contains, which would
// find a marker inside a comment a user wrote about ccdad and then rewrite from
// there.
const (
	setupPathBegin = "# >>> ccdad setup-path >>>"
	setupPathEnd   = "# <<< ccdad setup-path <<<"
)

// shellKind is the dialect a startup file is written in. It is a closed set
// with an explicit unknown, and that is the point: a default-to-POSIX fallback
// writes `export PATH=...` into config.fish, which is a syntax error fish
// prints on EVERY start, forever — strictly worse than not being on PATH.
type shellKind int

const (
	shellUnknown shellKind = iota
	shellPOSIX             // sh, dash, ksh and friends: the plain `case` guard
	shellBash
	shellZsh
	shellFish
	shellCsh
)

// shellFor reads a $SHELL value, or a --shell argument, as a dialect.
//
// The leading '-' is stripped because a login shell's argv[0] is conventionally
// "-bash", and a user reading their own process list and passing that back is
// asking for bash.
func shellFor(name string) shellKind {
	name = strings.ToLower(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "-")
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "bash":
		return shellBash
	case "zsh":
		return shellZsh
	case "fish":
		return shellFish
	case "sh", "dash", "ash", "ksh", "ksh93", "mksh", "pdksh", "busybox":
		return shellPOSIX
	case "csh", "tcsh":
		return shellCsh
	}
	return shellUnknown
}

func (k shellKind) String() string {
	switch k {
	case shellPOSIX:
		return "sh"
	case shellBash:
		return "bash"
	case shellZsh:
		return "zsh"
	case shellFish:
		return "fish"
	case shellCsh:
		return "csh"
	}
	return "unknown"
}

// quoteDouble escapes the four characters a POSIX shell still interprets
// inside double quotes. All four occur in real install directories:
// CCDAD_INSTALL_DIR is a free string, a `$` silently truncates the entry, and a
// backtick runs a command at every shell start.
//
// Double quotes are kept rather than switched to single quotes because the
// right-hand side must still expand `$PATH`.
func quoteDouble(s string, chars string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if r < 128 && strings.ContainsRune(chars, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderBlock is the exact bytes `setup-path` writes into a startup file, and
// the exact bytes `--print` emits. One function, because a preview that shows
// something other than what the writer writes has stopped previewing — and
// because a block pasted from --print has to be one a later run can find and
// rewrite in place. (Pasted at the END of the file: a `>>` onto a file with no
// trailing newline glues the first marker onto the last line, which breaks that
// line and hides the block. The stderr notice says so.)
//
// It returns "" for a dialect that has no block: csh and unknown are refusals,
// decided by the caller, not silently-empty writes.
func renderBlock(dir string, k shellKind) string {
	header := setupPathBegin + "\n" +
		"# Managed by `ccdad setup-path`. Edits inside this block are overwritten;\n" +
		"# `ccdad uninstall` removes it. The guard makes a second source a no-op.\n"
	switch k {
	case shellFish:
		// `contains` + `set -gx`, never `fish_add_path`: that helper stores
		// $fish_user_paths as a UNIVERSAL variable, in fish_variables, outside
		// this file — so the entry would survive deleting this block and
		// neither a rerun nor `ccdad uninstall` could un-register it.
		q := quoteDouble(dir, `\"$`)
		return header +
			"if not contains -- \"" + q + "\" $PATH\n" +
			"    set -gx PATH \"" + q + "\" $PATH\n" +
			"end\n" +
			setupPathEnd + "\n"
	case shellPOSIX, shellBash, shellZsh:
		q := quoteDouble(dir, "\\\"$`")
		// Three things here are load-bearing and each was verified by running
		// it, not by reading it:
		//
		//   `":${PATH:-}:"`      survives a user's own `set -u` earlier in the
		//                        file, which otherwise aborts their startup.
		//   the case guard       makes a second source a no-op. Unguarded,
		//                        sourcing three times gives DIR:DIR:DIR:...
		//   `${PATH:+:$PATH}`    keeps a trailing empty component off PATH when
		//                        PATH is unset. An empty component means the
		//                        WORKING DIRECTORY, so the naive form puts
		//                        every directory the user cd's into on PATH.
		return header +
			"case \":${PATH:-}:\" in\n" +
			"\t*\":" + q + ":\"*) ;;\n" +
			"\t*) export PATH=\"" + q + "${PATH:+:$PATH}\" ;;\n" +
			"esac\n" +
			setupPathEnd + "\n"
	}
	return ""
}

// splitKeep cuts a file into lines WITH their terminators, so that every byte
// this package does not deliberately change survives untouched — a CRLF file
// stays CRLF outside our own block, and a final line without a newline stays
// that way.
func splitKeep(b []byte) []string {
	s := string(b)
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out, s = append(out, s[:i+1]), s[i+1:]
	}
	return out
}

// isMarker reports whether a whole line is one of the fence markers.
//
// Whole line, and never strings.Contains: a user's own comment mentioning the
// marker would otherwise be read as a fence, and everything from it to the next
// one rewritten. Trailing whitespace and the CR of a CRLF file are trimmed,
// because an editor may add or remove either and neither changes what the line
// says.
func isMarker(line, marker string) bool {
	return strings.TrimRight(line, " \t\r\n") == marker
}

// blockRange is one ccdad block, as line indices, end inclusive.
type blockRange struct{ begin, end int }

// findBlocks locates every ccdad block in a file.
//
// An opening fence with no closing fence is refused rather than guessed at.
// The two available guesses are both destructive: treating EOF as the close
// deletes the rest of the user's rc file, and appending a fresh block leaves
// the stray marker to swallow it on the run after.
func findBlocks(lines []string) ([]blockRange, error) {
	var found []blockRange
	for i := 0; i < len(lines); i++ {
		if !isMarker(lines[i], setupPathBegin) {
			continue
		}
		end := -1
		for j := i + 1; j < len(lines); j++ {
			// A second opening marker before the first one closes. Taking the
			// later END as this block's close would make the "block" span every
			// line between the two openers — and removal DELETES that span, so
			// a stray marker a user left above their own aliases takes those
			// aliases with it. Refuse, like the unterminated case below.
			if isMarker(lines[j], setupPathBegin) {
				return nil, fmt.Errorf(
					"line %d opens a ccdad block with %q and line %d opens another before it closes with %q; "+
						"delete whichever marker is stray and run this again",
					i+1, setupPathBegin, j+1, setupPathEnd)
			}
			if isMarker(lines[j], setupPathEnd) {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf(
				"line %d opens a ccdad block with %q and nothing closes it with %q; "+
					"delete the stray marker (and anything below it that is not yours) and run this again",
				i+1, setupPathBegin, setupPathEnd)
		}
		found = append(found, blockRange{begin: i, end: end})
		i = end
	}
	return found, nil
}

// isBlank reports whether a line holds nothing but its terminator.
func isBlank(line string) bool { return strings.TrimRight(line, " \t\r\n") == "" }

// cut removes the given block ranges from lines, taking with each one the blank
// line that separates it from what came before — the one spliceBlock inserted.
// Dropping it is what makes removal a byte-exact inverse of writing.
func cut(lines []string, ranges []blockRange) []string {
	drop := make(map[int]bool, len(ranges)*8)
	for _, r := range ranges {
		for i := r.begin; i <= r.end; i++ {
			drop[i] = true
		}
		if r.begin > 0 && isBlank(lines[r.begin-1]) {
			drop[r.begin-1] = true
		}
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			out = append(out, line)
		}
	}
	return out
}

// spliceBlock returns the file with exactly one ccdad block in it, and whether
// that differs from what was there.
//
// Rewrite in place when a block exists, append when none does, and collapse the
// duplicates an older ccdad or a pasted `--print` could have left: rewriting the
// first and leaving a second would make every later run report "unchanged"
// while a stale block still named the old directory.
func spliceBlock(existing []byte, block string) ([]byte, bool, error) {
	lines := splitKeep(existing)
	found, err := findBlocks(lines)
	if err != nil {
		return nil, false, err
	}

	var out []string
	if len(found) == 0 {
		out = append(out, lines...)
		if len(existing) > 0 {
			// A file that does not end in a newline would otherwise have the
			// opening fence glued onto its last command — which breaks that
			// command AND hides the fence from the next run, so the run after
			// appends a second block.
			if !strings.HasSuffix(string(existing), "\n") {
				out = append(out, "\n")
			}
			out = append(out, "\n")
		}
		out = append(out, block)
	} else {
		// The first block's position is kept, so a user who moved it above
		// something that depends on PATH keeps that ordering. No index shift is
		// needed: findBlocks returns disjoint ranges in ascending order, so
		// every range cut here begins after found[0] ends, and cut's blank-line
		// drop cannot reach found[0] either — the line before a later block is
		// at the earliest found[0]'s END marker, which is not blank.
		kept := cut(lines, found[1:])
		out = append(out, kept[:found[0].begin]...)
		out = append(out, block)
		out = append(out, kept[found[0].end+1:]...)
	}

	joined := []byte(strings.Join(out, ""))
	return joined, !bytes.Equal(joined, existing), nil
}

// removeBlockFrom is spliceBlock's inverse: every ccdad block goes, and every
// other byte stays — with one licensed exception, which the round-trip test
// pins: a file that did not end in a newline keeps the one appending had to
// add, because removal cannot tell it from the user's own. It is what `ccdad uninstall` calls, which is why "no block
// here" has to come back as false rather than as an unchanged rewrite — a
// rewrite of a file ccdad never wrote is exactly what it must not do.
func removeBlockFrom(existing []byte) ([]byte, bool, error) {
	lines := splitKeep(existing)
	found, err := findBlocks(lines)
	if err != nil {
		return nil, false, err
	}
	if len(found) == 0 {
		return existing, false, nil
	}
	joined := []byte(strings.Join(cut(lines, found), ""))
	return joined, !bytes.Equal(joined, existing), nil
}

// targetFiles is the set of startup files a shell will actually read, in the
// order they are reported.
//
// bash gets TWO, and that is the one decision here that a reader will want to
// simplify. It cannot be simplified: a login shell reads the first EXISTING of
// ~/.bash_profile, ~/.bash_login and ~/.profile and skips ~/.bashrc entirely,
// while a Linux terminal emulator starts a non-login interactive shell that
// reads ~/.bashrc and none of the other three — and macOS Terminal starts a
// login shell every time. Whichever single file is chosen is dead on one of
// those platforms. Writing both is safe only because the block guards itself,
// so a login file that does source ~/.bashrc gets a second no-op rather than a
// duplicated PATH entry.
//
// The file that is never created is ~/.bash_profile. Creating it silently stops
// every bash login shell from reading ~/.profile again, which on a stock Debian
// or Ubuntu home disables ~/bin, ~/.local/bin and the ~/.bashrc chain in one
// write. When no login file exists, ~/.profile is the one made — it is also
// what dash, ksh and a plain sh login shell read.
func targetFiles(k shellKind) ([]string, error) {
	home, err := ccpath.Home()
	if err != nil {
		return nil, err
	}
	switch k {
	case shellBash:
		login := filepath.Join(home, ".profile")
		for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
			if _, err := os.Stat(filepath.Join(home, name)); err == nil {
				login = filepath.Join(home, name)
				break
			}
		}
		return []string{filepath.Join(home, ".bashrc"), login}, nil
	case shellZsh:
		zdotdir := os.Getenv("ZDOTDIR")
		if zdotdir == "" {
			// The dominant way ZDOTDIR is set is `export ZDOTDIR=...` inside
			// ~/.zshenv, which a ccdad started from bash, from a Makefile or
			// from an installer never sees. Writing ~/.zshrc for such a user
			// makes a file zsh will never read and reports success for it, so
			// this refuses instead — and the way through is to run it from a
			// zsh prompt, where ZDOTDIR is exported.
			zshenv := filepath.Join(home, ".zshenv")
			if body, err := os.ReadFile(zshenv); err == nil && assignsZDOTDIR(body) {
				return nil, WithCode(fmt.Errorf(
					"%s assigns ZDOTDIR, so zsh may read its configuration from somewhere other than %s, "+
						"and ccdad cannot evaluate that file to find out; re-run as "+
						"`ZDOTDIR=<that directory> ccdad setup-path`, or use `ccdad setup-path --print` "+
						"and paste the block into the .zshrc zsh actually reads",
					zshenv, home), ExitBlocked)
			}
			zdotdir = home
		}
		return []string{filepath.Join(zdotdir, ".zshrc")}, nil
	case shellFish:
		return []string{filepath.Join(configHome(home), "fish", "config.fish")}, nil
	case shellPOSIX:
		return []string{filepath.Join(home, ".profile")}, nil
	}
	return nil, fmt.Errorf("no startup file is known for %s", k)
}

// setupPathOptions is the command's whole surface. It is deliberately small:
// no --dir (ccdad cannot verify that a directory a user names holds a ccdad,
// and putting an arbitrary one on PATH is a bigger promise than this command
// should make), no --force (once exit 3 keys off what is REGISTERED rather
// than off the live $PATH, no state needs one), and no user-facing --remove
// (`ccdad uninstall` owns removal, and it is the command that knows the binary
// is going away).
type setupPathOptions struct {
	print bool
	shell string
}

func newSetupPathCmd() *cobra.Command {
	var opts setupPathOptions

	cmd := &cobra.Command{
		Use:   "setup-path",
		Short: "Put the directory holding ccdad on your PATH, durably",
		Long: "setup-path registers the directory holding this binary on your PATH so that a\n" +
			"new terminal finds `ccdad`. It is the answer to `ccdad: command not found`\n" +
			"right after an install: `curl | bash` has the installer's own script on stdin,\n" +
			"so the installer cannot ask, and a startup file it guessed at is a file it can\n" +
			"corrupt.\n\n" +
			"Running it twice changes nothing the second time, and `ccdad uninstall` takes\n" +
			"back exactly what it registered.\n\n" + setupPathPlatformHelp +
			"\nUse --print to see what registration looks like on this platform, without\n" +
			"writing anything.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetupPath(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.print, "print", false,
		"print what would be registered, and write nothing")
	setupPathFlags(cmd, &opts)
	return cmd
}

// ccdadDir is the directory this command registers: the one holding the running
// binary.
//
// os.Executable is used raw, and filepath.EvalSymlinks deliberately is not. The
// path a user invoked is the one their PATH should hold; resolving a symlink
// registers wherever the real file happens to live, which for a hand-linked
// build is a source tree. The platforms already disagree about this and there
// is no fixing it from here — Linux's os.Executable reads /proc/self/exe and is
// fully resolved before this function sees it, while macOS reports the
// invocation path — so the directory is named in the report instead, where a
// surprising answer is visible before the user acts on it.
func ccdadDir() (exe, dir string, err error) {
	exe, err = executablePath()
	if err != nil {
		return "", "", fmt.Errorf("ccdad cannot tell where its own binary is (%w), "+
			"so it cannot say which directory to put on your PATH", err)
	}
	return exe, filepath.Dir(exe), nil
}

func runSetupPath(cmd *cobra.Command, opts setupPathOptions) error {
	exe, dir, err := ccdadDir()
	if err != nil {
		return err
	}
	if opts.print {
		if owner := packageManagerOwning(exe); owner != "" {
			// The write path refuses this install; the print path must not
			// quietly hand over the block it refused. On Linux os.Executable is
			// fully resolved, so the directory below is a VERSIONED one — a
			// user who pastes it has a dead PATH entry the next time the
			// package manager upgrades ccdad.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Note: %s installed ccdad at %s and manages its PATH — run %s instead of pasting this.\n",
				owner, exe, shellenvHint(owner))
		}
		return setupPathPrint(cmd, dir, opts)
	}

	// A package manager that installed ccdad also owns its PATH, and its own
	// directory layout: registering it here would pin a versioned Cellar path
	// that the next upgrade moves. uninstall.go refuses to delete such a binary
	// for the same reason, through the same predicate.
	if owner := packageManagerOwning(exe); owner != "" {
		out := cmd.ErrOrStderr()
		if onPathList(os.Getenv("PATH"), dir, livePathRules) {
			// The ONE place exit 3 is decided by the live PATH, and it is not
			// an exception to the registration rule but a case the rule has no
			// reading for: ccdad registers nothing for a package-manager
			// install in either branch, so there is no registration state to
			// key on. The message says that out loud, because a user whose
			// shell has `brew shellenv` only in the session they are standing
			// in would otherwise read this as "you are set up".
			fmt.Fprintf(out, "%s installed ccdad at %s, and %s is already on this shell's PATH, "+
				"so ccdad registered nothing. If a new terminal cannot find ccdad, %s puts it there — "+
				"run %s.\n", owner, exe, dir, owner, shellenvHint(owner))
			return WithCode(errSilent, ExitNothingToDo)
		}
		fmt.Fprintf(out, "%s installed ccdad at %s, so %s manages its PATH — run %s.\n",
			owner, exe, owner, shellenvHint(owner))
		return WithCode(errSilent, ExitBlocked)
	}
	return setupPathApply(cmd, dir, opts)
}

// shellenvHint is the package manager's own instruction for putting its
// directory on PATH. Naming ccdad's block instead would tell the user to fix
// one symptom of a shell that has not run `brew shellenv` at all.
func shellenvHint(owner string) string {
	if owner == "Scoop" {
		return "`scoop reset ccdad`"
	}
	return "`eval \"$(brew shellenv)\"` from your shell startup file"
}

// configHome is $XDG_CONFIG_HOME, or ~/.config.
//
// A RELATIVE value is ignored. The XDG spec says the variable must hold an
// absolute path, and the common typos (`XDG_CONFIG_HOME=.config`, or a quoted
// `~/.config` the shell never expanded) would otherwise make setup-path create
// `./.config/fish/config.fish` under whatever directory the user happened to be
// standing in, report success, and leave a file fish never reads and uninstall
// can never find again.
func configHome(home string) string {
	if config := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(config) {
		return config
	}
	return filepath.Join(home, ".config")
}

// assignsZDOTDIR reports whether a .zshenv contains a line that ASSIGNS
// ZDOTDIR, rather than merely containing the word.
//
// The distinction is the difference between a refusal that protects someone and
// one that locks them out: a commented-out `# export ZDOTDIR=...`, an `unset
// ZDOTDIR`, or a comment saying ZDOTDIR is deliberately unused would all match
// the bare word, and the refusal it triggers has no way through — from a zsh
// prompt the environment is identical, so the command would refuse forever for
// a user whose ~/.zshrc was the right answer all along.
func assignsZDOTDIR(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimLeft(line, " \t")
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimLeft(line, " \t")
		if strings.HasPrefix(line, "ZDOTDIR=") {
			return true
		}
	}
	return false
}
