//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The Unix half of `ccdad setup-path`: a marker-fenced block in the startup
// files the user's shell actually reads. Windows has no such file and does this
// in the registry instead; the two halves share the decisions in setuppath.go
// and nothing else.

// livePathRules reads $PATH as this platform's shell does.
var livePathRules = unixPathRules

// setupPathPlatformHelp is the half of --help that is only true here. The
// Windows binary has no startup file and no markers, so a shared paragraph
// describing them would send a Windows user looking for a fenced block that
// does not exist.
const setupPathPlatformHelp = "It writes a marker-fenced block into the startup files your shell reads, and\n" +
	"nothing outside those markers is ever touched. Editing inside the block is\n" +
	"not durable: a later run rewrites what is between the markers.\n"

// setupPathFlags adds the flags that only exist here. --shell exists because
// shell detection genuinely can fail — $SHELL is unset under some daemons,
// containers and CI — and without it such a user has no way to proceed but to
// paste. On Windows there is no shell to choose, so the flag is not offered
// there rather than being accepted and ignored.
func setupPathFlags(cmd *cobra.Command, opts *setupPathOptions) {
	cmd.Flags().StringVar(&opts.shell, "shell", "",
		"shell to set up: bash, zsh, fish, sh (default: $SHELL)")
	_ = cmd.RegisterFlagCompletionFunc("shell",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return []string{"bash", "zsh", "fish", "sh"}, cobra.ShellCompDirectiveNoFileComp
		})
}

// setupShells is the set --shell accepts, spelled once so the flag's help, its
// completion and its error message cannot drift apart.
var setupShells = []string{"bash", "zsh", "fish", "sh"}

// resolveShell decides which dialect to write, and says where the answer came
// from so the report can name it.
//
// $SHELL is the only automatic signal, and the parent process deliberately is
// not consulted. The block has to serve the shell the user's NEXT terminal
// starts, which is the login shell $SHELL names; the parent process is whatever
// they happen to be inside right now — a zsh user running one bash command, a
// Makefile, an agent session — and writing that shell's file would report
// success for a registration their real shell never reads.
func resolveShell(opts setupPathOptions) (shellKind, string, error) {
	if opts.shell != "" {
		k := shellFor(opts.shell)
		if k == shellUnknown {
			return k, "", UsageError("ccdad cannot set up %q; --shell takes one of %s",
				opts.shell, strings.Join(setupShells, ", "))
		}
		return k, "--shell " + opts.shell, nil
	}
	value := os.Getenv("SHELL")
	k := shellFor(value)
	if k == shellUnknown {
		if value == "" {
			return k, "", WithCode(errors.New(
				"ccdad cannot tell which shell to set up: $SHELL is unset. "+
					"Re-run with --shell ("+strings.Join(setupShells, ", ")+"), "+
					"or use --print and paste the block into your own startup file"), ExitBlocked)
		}
		return k, "", WithCode(fmt.Errorf(
			"ccdad does not know how to write a startup file for %s ($SHELL). "+
				"Re-run with --shell (%s), or use --print and paste the block into your own startup file",
			value, strings.Join(setupShells, ", ")), ExitBlocked)
	}
	return k, "$SHELL", nil
}

// setupPathPrint is --print: the exact bytes the writer would append, on
// stdout, and every human word on stderr. Same renderer as the writer, because
// a preview that shows something other than what gets written has stopped
// previewing — and because `ccdad setup-path --print >> ~/.zshrc` has to
// produce a block a later run can find and rewrite in place.
func setupPathPrint(cmd *cobra.Command, dir string, opts setupPathOptions) error {
	k, source, err := resolveShell(opts)
	if err != nil {
		// --print is total for a machine ccdad cannot READ: an undetectable
		// $SHELL still gets the portable form, which is the one install.sh has
		// always printed, because refusing there leaves that user with nothing.
		//
		// A value the user TYPED is the opposite case and must not be
		// swallowed. `--print --shell fsh` falling back to sh hands a fish user
		// POSIX syntax to paste into config.fish, which fish then errors on at
		// every shell start — the exact outcome the closed shell table exists
		// to prevent — and it exits 0 while the same typo without --print is a
		// usage error.
		if IsUsageError(err) {
			return err
		}
		k, source = shellPOSIX, "no shell detected, so this is the portable sh form"
	}
	if k == shellCsh {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", cshLine(dir))
		fmt.Fprintf(cmd.ErrOrStderr(), "For csh and tcsh (%s). Add it to ~/.cshrc or ~/.tcshrc.\n", source)
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), renderBlock(dir, k))
	fmt.Fprintf(cmd.ErrOrStderr(), "The block above is for %s (%s). It writes nothing on its own. "+
		"Run `ccdad setup-path` to have ccdad place it, or paste it at the END of a startup file — "+
		"appending it to a file that does not end in a newline glues the first marker onto the last "+
		"line, which breaks that line and hides the block from a later run.\n", k, source)
	return nil
}

// cshLine is csh and tcsh's PATH syntax, which shares nothing with the POSIX
// block. ccdad prints it rather than writing it: ~/.cshrc has no `case`, no
// `export` and no fence convention, and a startup file written in the wrong
// dialect prints an error on every shell start forever.
func cshLine(dir string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(dir)
	return `if ("$path" !~ *"` + q + `"*) set path = ("` + q + `" $path)`
}

// setupPathApply is the write path.
func setupPathApply(cmd *cobra.Command, dir string, opts setupPathOptions) error {
	out := cmd.ErrOrStderr()

	// Under sudo the home directory is whatever the sudoers policy leaves —
	// /root, where the user will never look, or their own home with euid 0,
	// where a newly created rc file ends up owned by root and the user can no
	// longer edit their own dotfile. Neither is recoverable by re-running.
	if underSudo(os.Geteuid(), os.Getenv("SUDO_USER")) {
		return UsageError("run `ccdad setup-path` as yourself, not under sudo: it writes YOUR shell " +
			"startup file, and under sudo it would write root's (or leave yours owned by root)")
	}

	k, source, err := resolveShell(opts)
	if err != nil {
		return err
	}
	if k == shellCsh {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", cshLine(dir))
		fmt.Fprintf(out, "ccdad does not write csh startup files — they share no syntax with the block it "+
			"manages, and a wrong dialect errors on every shell start. Add the line above to ~/.cshrc.\n")
		return WithCode(errSilent, ExitBlocked)
	}

	files, err := targetFiles(k)
	if err != nil {
		return err
	}
	block := renderBlock(dir, k)

	wrote := false
	for _, file := range files {
		created, changed, err := writeRC(file, block)
		if err != nil {
			return err
		}
		switch {
		case created:
			fmt.Fprintf(out, "Created %s and put %s on your PATH there.\n", file, dir)
		case changed:
			fmt.Fprintf(out, "Updated the ccdad block in %s to put %s on your PATH.\n", file, dir)
		default:
			fmt.Fprintf(out, "%s already registers %s.\n", file, dir)
		}
		wrote = wrote || changed
	}

	onPath := onPathList(os.Getenv("PATH"), dir, livePathRules)
	if !wrote {
		// Exit 3 keys off what is REGISTERED, never off the live $PATH. The two
		// are independent, and a $PATH-keyed 3 is the failure this command
		// exists to prevent: the user who pasted an export line into their
		// running shell would be told "already on PATH" and get no durable
		// registration at all, so the next terminal says `command not found`
		// again. The exit contract settles it — 3 is "the world is already
		// as you asked", and a user who asked for durable registration and
		// has none is not in that world.
		if !onPath {
			fmt.Fprintf(out, "Nothing to do. This shell has not read that file yet — "+
				"open a new terminal, or run `. %s`.\n", files[len(files)-1])
		}
		return WithCode(errSilent, ExitNothingToDo)
	}
	if onPath {
		fmt.Fprintf(out, "%s was already on THIS shell's PATH; the block is what makes a new terminal find it too.\n", dir)
	} else {
		fmt.Fprintf(out, "Open a new terminal to pick it up, or run `. %s` now (shell: %s, from %s).\n",
			files[len(files)-1], k, source)
	}
	return nil
}

// writeRC puts the block into one startup file, and reports whether the file
// had to be created and whether anything changed.
func writeRC(path, block string) (created, changed bool, err error) {
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		created, existing = true, nil
	case err != nil:
		return false, false, fmt.Errorf("reading %s: %w", path, err)
	}

	updated, changed, err := spliceBlock(existing, block)
	if err != nil {
		return false, false, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return false, false, nil
	}

	if err := replaceFile(path, updated); err != nil {
		return false, false, err
	}
	return created, true, nil
}

// pathCandidates is every startup file setup-path could have written, for
// every shell it knows — not just the one $SHELL names today. `ccdad uninstall`
// scans them all, because by then the shell that ran setup-path may not be the
// shell running uninstall.
func pathCandidates() ([]string, error) {
	home, err := ccpath.Home()
	if err != nil {
		return nil, err
	}
	config := configHome(home)
	candidates := []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".bash_login"),
		filepath.Join(home, ".profile"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".zshenv"),
		// The XDG location a ZDOTDIR user almost always points at,
		// UNCONDITIONALLY. $ZDOTDIR is normally exported from ~/.zshenv, which
		// an uninstall run from bash, from an installer or from a Makefile
		// never sees — so scanning only when the variable happens to be in the
		// environment leaves the block setup-path wrote there behind, unnamed
		// in the preview and unremoved, still prepending a directory whose
		// binary has just been deleted.
		filepath.Join(config, "zsh", ".zshrc"),
		filepath.Join(config, "fish", "config.fish"),
	}
	if zdotdir := os.Getenv("ZDOTDIR"); zdotdir != "" {
		candidates = append(candidates, filepath.Join(zdotdir, ".zshrc"))
	}
	return candidates, nil
}

// isStartupFile reports whether a candidate is something ccdad may read and
// rewrite: a regular file, or a symlink resolving to one.
//
// os.Stat is deliberate — it follows the link, so a dotfiles repository is
// still a startup file, and it does NOT open, so a named pipe answers here
// instead of blocking a later os.ReadFile forever. A directory, a FIFO, a
// device and a dangling symlink are all "not a startup file", which is a skip
// rather than the error that would otherwise abort the scan.
func isStartupFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// pathRegistrations names the startup files that currently carry a ccdad
// block, without changing any of them. `ccdad uninstall` enumerates before it
// asks, because a confirmation prompt with nothing above it is a prompt people
// say yes to.
//
// dir is unused here and used on Windows, and the asymmetry is real: the fence
// identifies the block whatever directory it names, while a registry PATH entry
// can only be recognised by the directory it holds. A block naming some other
// directory is still ccdad's to remove — one block exists per file by
// construction, and leaving one that points at a binary that has just been
// deleted is worse than removing it.
func pathRegistrations(_ string) ([]string, error) {
	candidates, err := pathCandidates()
	if err != nil {
		return nil, err
	}
	var found []string
	var failures []error
	for _, path := range candidates {
		if !isStartupFile(path) {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		blocks, err := findBlocks(splitKeep(body))
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if len(blocks) > 0 {
			found = append(found, path)
		}
	}
	// Every candidate is looked at, and the failures are joined rather than
	// returned at the first one. Stopping early is how one stray marker in
	// ~/.bashrc hides the real block in a file that sorts after it — which
	// uninstall then reports as a clean removal.
	return found, errors.Join(failures...)
}

// unregisterPath takes ccdad's block back out of every startup file that has
// one, and reports which files changed.
func unregisterPath(_ string) (removed []string, err error) {
	candidates, err := pathCandidates()
	if err != nil {
		return nil, err
	}
	var failures []error
	for _, path := range candidates {
		gone, err := removeRC(path)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if gone {
			removed = append(removed, path)
		}
	}
	return removed, errors.Join(failures...)
}

// removeRC takes ccdad's block out of one startup file, leaving every other
// byte as it was. A file with no block of ours is not rewritten at all --
// rewriting a file ccdad never wrote is the one thing removal must not do.
func removeRC(path string) (bool, error) {
	if !isStartupFile(path) {
		return false, nil
	}
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	updated, removed, err := removeBlockFrom(existing)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !removed {
		return false, nil
	}
	if err := replaceFile(path, updated); err != nil {
		return false, err
	}
	return true, nil
}

// replaceFile writes content over path, atomically and without destroying what
// the path is.
//
// Two properties, each of which a simpler write gets wrong. A symlinked rc file
// is a dotfiles repository, and replacing the LINK with a regular file detaches
// the user from theirs -- so the link is resolved first and the replacement
// happens beside the real file. And an in-place truncate that dies between the
// truncate and the write leaves an EMPTY ~/.bashrc, silently taking the user's
// whole shell setup with it -- so the new bytes are complete on disk before the
// old name points at them.
func replaceFile(path string, content []byte) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		// A symlink whose target does not exist — a dotfiles repository that
		// has not been cloned or stowed yet, or a volume that is not mounted.
		// Renaming over it would replace the LINK with a regular file and
		// report "Created", and the user's next `stow` or `chezmoi apply`
		// fails on a target that is no longer a symlink. Refuse instead: what
		// is broken is the link, and ccdad cannot know where it was meant to
		// point.
		dest, _ := os.Readlink(path)
		return fmt.Errorf("%s is a symlink to %s, which does not exist; "+
			"ccdad will not replace the link with a file — restore the target (or remove the link) and run this again",
			path, dest)
	}

	mode := fs.FileMode(0o644)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
		// A read-only startup file is a deliberate signal from its owner. An
		// atomic replace would overwrite it without a murmur, where an ordinary
		// append would have been refused by the kernel.
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("%s is not writable: %w", target, err)
		}
		_ = f.Close()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".ccdad-setup-path-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("replacing %s: %w", target, err)
	}
	return nil
}

// underSudo reports whether this process is root standing in for someone else.
//
// A predicate rather than an inline condition because it cannot be reached any
// other way from a test: a suite that does not run as root can never make
// os.Geteuid() answer 0, and one that does run as root (a container image, a CI
// box) has no SUDO_USER, so the inline form would be exercised on neither.
func underSudo(euid int, sudoUser string) bool { return euid == 0 && sudoUser != "" }
