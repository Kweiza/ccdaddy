package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/release"
	"github.com/Kweiza/ccdaddy/internal/relsign"
)

// releaseKeys is the trust root this build verifies against.
//
// It is a package var of the shape uninstall.go's executablePath established,
// and for the same reason: production never reassigns it, and the suite points
// it at a key generated in-process so that the update tests do not need the
// maintainer's secret. relsign holds the real root as a CONST — -ldflags -X
// patches string variables and build-release.sh already patches two of them, so
// a trust root a link line could swap would not be pinned at all.
var releaseKeys = relsign.Keys

const (
	// updateStagePattern is the staging directory beside the binary. The
	// .ccdad-update. prefix keeps a crashed update distinguishable from a
	// crashed install, whose scratch directory is .ccdad-install.XXXXXX in the
	// same place.
	updateStagePattern = ".ccdad-update.*"

	// maxSumsBytes: the real file is nine rows of about a hundred bytes. A
	// megabyte is three orders of magnitude of headroom and still refuses a
	// proxy that answers with a whole web page, which is the failure it is for.
	maxSumsBytes = 1 << 20
	// maxSigBytes sits ABOVE the verifier's own 16 KiB refusal on purpose, so
	// an over-long body reaches the verifier and is refused for its own stated
	// reason rather than arriving pre-truncated and failing on a line count.
	maxSigBytes = 64 << 10
	// maxAssetBytes: every released ccdad is under four megabytes on all six
	// targets. This is not a fit — it is the ceiling that stops a hostile
	// origin streaming into a directory on the user's PATH forever. It is far
	// above the measurement deliberately: the digest decides whether the bytes
	// are right, and a cap chosen from today's size would refuse a future
	// target that links cgo.
	maxAssetBytes = 256 << 20

	// metadataTimeout bounds the redirect and the two sub-kilobyte fetches. It
	// bounds connection setup and a stalled read, not a transfer.
	metadataTimeout = 30 * time.Second
	// assetTimeout bounds the asset: four megabytes in ten minutes is about
	// 7 KB/s, a bad tethered link rather than a stalled one. It is a CONTEXT
	// deadline and not an http.Client.Timeout, because a whole-request clock
	// kills a slow but healthy download.
	assetTimeout = 10 * time.Minute
	// smokeTimeout bounds `<staged> --version`. Cobra prints a version and
	// exits; anything that has not done so in fifteen seconds is not a ccdad.
	smokeTimeout = 15 * time.Second
)

type updateOptions struct {
	check   bool
	version string
	asJSON  bool
}

func newUpdateCmd() *cobra.Command {
	var opts updateOptions

	cmd := &cobra.Command{
		Use: "update",
		// The first alias in the tree. Cobra resolves it before the root's
		// unknown-command retag, and every table in the tree is keyed on
		// CommandPath(), so it costs no row anywhere.
		Aliases: []string{"upgrade"},
		Short:   "Replace this ccdad binary with the latest signed release",
		Long: "It resolves the latest release, downloads sha256sums.txt and its minisign\n" +
			"signature, verifies that signature against a key compiled into this binary, and\n" +
			"only THEN reads the checksum row for this platform. The asset is downloaded\n" +
			"beside the binary, checked for size and digest, run once, and renamed over this\n" +
			"file.\n\n" +
			"There is no --no-verify and no --insecure. A mirror that does not carry the\n" +
			"signature and an attacker who removed it are the same bytes on the wire.\n\n" +
			"There is no prompt and no --yes either, because nothing here is destroyed.\n" +
			"Naming a tag with --version IS the consent for a downgrade.\n\n" +
			"--check stops before the download, so it answers everything a full run answers\n" +
			"except the three things only the asset can tell you: its size, its checksum, and\n" +
			"whether it runs on this machine. It still creates and removes a directory beside\n" +
			"the binary, so its answer about writability is the real one.\n\n" +
			"A Homebrew or Scoop install is refused rather than replaced: that binary belongs\n" +
			"to the package manager, and replacing it in place leaves the manager believing\n" +
			"something else is installed.\n\n" +
			"The architecture is always the one this binary was built for, so an amd64 build\n" +
			"under Rosetta or Windows-on-ARM stays on amd64. Re-run the installer to move.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.check, "check", false, "report whether an update is available and replace nothing")
	cmd.Flags().StringVar(&opts.version, "version", "", "install this release tag instead of the latest, e.g. v1.2.3")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// updateReport accumulates what the run has learned, so that a refusal at any
// step writes exactly the facts that were true when it fired.
type updateReport struct {
	// current is the RUNNING process's version, and it does not change when
	// updated becomes true: the pair "currentVersion":"0.6.1","updated":true is
	// correct and means "0.6.1 replaced itself".
	current string

	hasPaths   bool
	target     string
	invokedDir string
	onPath     bool

	hasTag bool
	tag    string
	// targetVersion, never latestVersion: under --version v0.5.0 a field called
	// latestVersion would hold an older version than the one running.
	// resolvedLatest is what tells a consumer which question was answered.
	targetVersion   string
	resolvedLatest  bool
	updateAvailable bool

	updated bool

	daemonWasRunning bool
	daemonRestarted  bool
}

func (r *updateReport) payload(reason string) map[string]any {
	out := map[string]any{
		"schemaVersion":  1,
		"currentVersion": r.current,
		"updated":        r.updated,
	}
	if r.hasPaths {
		out["path"] = r.target
		out["installDir"] = r.invokedDir
		out["onPath"] = r.onPath
	}
	if r.hasTag {
		out["tag"] = r.tag
		out["targetVersion"] = r.targetVersion
		out["resolvedLatest"] = r.resolvedLatest
		out["updateAvailable"] = r.updateAvailable
	}
	// Only when there WAS a daemon: false here means the restart was skipped or
	// failed, and on a machine whose daemon was never up that would be a
	// sentence about nothing.
	if r.daemonWasRunning {
		out["daemonRestarted"] = r.daemonRestarted
	}
	if reason != "" {
		out["reason"] = reason
	}
	return out
}

// emit writes the run's answer and returns the error that carries its exit code.
//
// --json changes the representation and never the answer, so the exit code is
// identical with and without it, and the human words are suppressed rather than
// printed beside the payload.
//
// The returned error is deliberately the SILENT sentinel: the words are already
// on the screen. No caller may wrap it — CodeFor's errors.As unwraps through
// fmt.Errorf, so a wrapped sentinel keeps its own code and its own silence and
// the wrapping sentence is never printed.
func (r *updateReport) emit(cmd *cobra.Command, asJSON bool, code ExitCode, reason, human string) error {
	if asJSON {
		if err := writeJSON(cmd, r.payload(reason)); err != nil {
			return err
		}
	} else if human != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), human)
	}
	if code == ExitOK {
		return nil
	}
	return WithCode(errSilent, code)
}

// say prints one progress line, and nothing at all under --json.
//
// stopDaemon and startDaemon print their own lines and are not routed
// through here. Those go to stderr, which the --json contract leaves alone.
func say(cmd *cobra.Command, asJSON bool, format string, a ...any) {
	if asJSON {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
}

func runUpdate(cmd *cobra.Command, opts updateOptions) error {
	rep := &updateReport{current: buildinfo.Version}

	// Step 0. Normalized ONCE, here, so that `--version 1.2.3` and
	// `--version v1.2.3` are the same request all the way down to the trusted
	// comment. Without it a user's missing v fails later as "signed for a
	// different release", which is the tamper remedy, for a typo.
	//
	// Changed() rather than a non-empty string, so `--version ""` is the
	// argument error it is rather than silently meaning "latest".
	var want release.Version
	pinned := cmd.Flags().Changed("version")
	if pinned {
		v, ok := release.ParseTag(opts.version)
		if !ok {
			return UsageError("--version wants a release tag such as v1.2.3 or 1.2.3, not %q", opts.version)
		}
		want = v
	}

	// Step 1. Two paths, and they are not the same path.
	exe, err := executablePath()
	if err != nil {
		return rep.emit(cmd, opts.asJSON, ExitFailure, "no-executable-path",
			fmt.Sprintf("ccdad cannot tell where its own binary is (%v), so it cannot replace it.", err))
	}
	// invokedDir is UNRESOLVED, and it is the only correct input to the PATH
	// note: a real path under /opt is not on PATH even when the ~/.local/bin
	// symlink that found it is. Warning about the resolved directory would cry
	// wolf on exactly the symlinked installs this command supports.
	invokedDir := filepath.Dir(exe)
	// target is RESOLVED. os.Executable is already fully resolved on Linux and
	// is the invocation path on macOS, so without this an update on macOS
	// replaces a SYMLINK — orphaning the real binary and reverting on the
	// user's next command. ccdadDir deliberately does not resolve, and that is
	// right for the question IT answers; it is wrong here.
	target, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return rep.emit(cmd, opts.asJSON, ExitFailure, "no-executable-path",
			fmt.Sprintf("ccdad cannot resolve %s to a real file (%v), so it cannot replace it.", exe, err))
	}
	// The staging directory is the target's own directory, which is what makes
	// the final move a same-directory rename by construction.
	stageDir := filepath.Dir(target)
	rep.hasPaths, rep.target, rep.invokedDir = true, target, invokedDir
	rep.onPath = onPathList(os.Getenv("PATH"), invokedDir, livePathRules)

	// Step 2. An empty trust root refuses to self-update: fail closed. This
	// asks the seam rather than relsign.Enforced(), which answers the same
	// question about the compile-time constant — going through the seam is what
	// lets the suite describe a build with no key at all, and what stops every
	// update test depending on whether a key has been generated yet.
	keys := releaseKeys()
	if len(keys) == 0 {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "no-pinned-key",
			"This ccdad was built with no release key, so it cannot verify an update. "+
				"Re-run the installer from the project's README to get a build that can.")
	}

	// Step 3. A dev build has no released version to compare against, and
	// replacing it would throw away whatever it was built from.
	if buildinfo.Version == "dev" {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "dev-build",
			"This is a development build, so there is no released version to update from. "+
				"Re-run the installer from the project's README to get a released one.")
	}

	// Step 4. packageManagerOwning is uninstall's, unchanged. It knows Homebrew
	// and Scoop, matched segment-wise, and it does not know winget, nix, apt or
	// MacPorts — teaching it those is uninstall's decision as much as this
	// command's and belongs in its own change. It is asked about the RESOLVED
	// path, because a /usr/local/bin symlink resolves into the Cellar and the
	// Cellar is what names the manager.
	if owner := packageManagerOwning(target); owner != "" {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "package-manager",
			fmt.Sprintf("%s installed ccdad at %s and owns that file. Run %s instead: replacing it here "+
				"would leave %s believing something else is installed.", owner, target, upgradeHint(owner), owner))
	}

	// Step 5. The writability probe and the staging directory in ONE syscall,
	// which is the analogue of install.sh's mktemp -d inside the install
	// directory. It asks the right question: what is being predicted is a
	// rename WITHIN this directory, which needs rights on the directory and not
	// on the target file. It must come after step 4, because a Homebrew Cellar
	// is routinely writable by its owner and the reason to refuse there is not
	// permission.
	staging, err := os.MkdirTemp(stageDir, updateStagePattern)
	if err != nil {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "not-writable",
			fmt.Sprintf("ccdad cannot write to %s (%v), so it cannot stage a replacement there.", stageDir, err))
	}
	defer os.RemoveAll(staging)

	// ---- nothing above this line touches the network ----

	client := release.NewClient()
	base := release.BaseURL()

	// Step 6.
	if !pinned {
		ctx, cancel := context.WithTimeout(cmd.Context(), metadataTimeout)
		tag, err := client.Latest(ctx)
		cancel()
		if err != nil {
			return rep.emit(cmd, opts.asJSON, ExitFailure, "resolve-failed",
				fmt.Sprintf("ccdad could not work out which release is latest: %v", err))
		}
		// Latest already re-parsed and re-stringified this, and it is parsed
		// again rather than trusted so that a tag becomes a Version on ONE line
		// whichever of the two sources it came from.
		v, ok := release.ParseTag(tag)
		if !ok {
			return rep.emit(cmd, opts.asJSON, ExitFailure, "resolve-failed",
				fmt.Sprintf("ccdad could not read %q as a release tag.", tag))
		}
		want = v
	}
	rep.hasTag, rep.tag, rep.targetVersion = true, want.Tag(), want.String()
	rep.resolvedLatest = !pinned

	// Steps 7 and 8 compare PARSED versions, never strings. A hand-built stamp
	// of v0.6.1 makes the string form compute "vv0.6.1" and miss "already
	// current" — build-release.sh strips the v only in the branch that DERIVES
	// a version, so an explicit VERSION=v0.6.1 is stamped with it.
	//
	// An unparseable running version falls through to the download rather than
	// claiming currency: "I cannot compare these" is not "you are up to date".
	running, runningOK := release.ParseTag(buildinfo.Version)
	rep.updateAvailable = !runningOK || want.Compare(running) > 0
	if runningOK && !pinned {
		switch {
		case want.Compare(running) == 0:
			// Step 7. Exit 3 and not 5: 3 is "the world is already how you
			// asked", which is the same reading that gives `daemon stop` a 3
			// with nothing to stop, and it is what makes
			// `ccdad update --check && ccdad update` compose.
			//
			// Skipped when --version is explicit, because
			// `ccdad update --version <what I am on>` is how a user re-fetches
			// and re-verifies a binary they suspect.
			return rep.emit(cmd, opts.asJSON, ExitNothingToDo, "already-current",
				fmt.Sprintf("ccdad %s is the latest release, and it is what is running here.", want))
		case want.Compare(running) < 0:
			// Step 8. The tag arrived over an unauthenticated channel, and the
			// signature only binds CONTENT to a NAME — so an origin that
			// chooses what to serve can answer with an older release whose bugs
			// are public and pass every check. Naming the tag is the consent
			// that unlocks it; there is no prompt, because there is nothing
			// here to destroy.
			return rep.emit(cmd, opts.asJSON, ExitBlocked, "rollback",
				fmt.Sprintf("The origin says the latest release is %s, which is older than the %s running here. "+
					"Nothing was downloaded. Pass --version %s if that is really what you want.",
					want.Tag(), running, want.Tag()))
		}
	}

	dl := release.DownloadBase(base, want.Tag())

	// Step 9.
	ctx, cancel := context.WithTimeout(cmd.Context(), metadataTimeout)
	sums, err := client.Get(ctx, dl+"/sha256sums.txt", maxSumsBytes)
	cancel()
	if err != nil {
		return rep.emit(cmd, opts.asJSON, ExitFailure, "download-sums",
			fmt.Sprintf("ccdad could not download the checksum file for %s: %v", want.Tag(), err))
	}

	// Step 10. The two arms are DIFFERENT failures, and the split is the whole
	// reason StatusError carries a code. A 404 means the release genuinely
	// carries no signature, which is a refusal the user can act on and whose
	// remedy IS the installer. A timeout, a 500 or a DNS failure says nothing
	// about the release, and reporting it as a tamper verdict would let a flaky
	// origin manufacture a permanent-looking one.
	ctx, cancel = context.WithTimeout(cmd.Context(), metadataTimeout)
	sig, err := client.Get(ctx, dl+"/sha256sums.txt.minisig", maxSigBytes)
	cancel()
	if err != nil {
		var status *release.StatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return rep.emit(cmd, opts.asJSON, ExitBlocked, "unsigned-release",
				fmt.Sprintf("Release %s publishes no signature, so ccdad will not install it. "+
					"Re-run the installer from the project's README if you mean to move to it anyway.", want.Tag()))
		}
		return rep.emit(cmd, opts.asJSON, ExitFailure, "download-sums",
			fmt.Sprintf("ccdad could not download the signature for %s: %v", want.Tag(), err))
	}

	// Step 11 is the ordering this whole design rests on. A sums file whose
	// SHAPE has been inspected is still a sums file an attacker wrote, and
	// reading a row out of one before verifying the signature is not a smaller
	// version of verifying — it is trusting the file. Signature, then shape,
	// then row, in that order, with nothing between them.
	//
	// The wanted tag is mandatory rather than optional, and Verify treats the
	// empty string as an error rather than a skip. sha256sums.txt names no
	// version, so an old release's (sums, signature) pair stays a genuine,
	// correctly signed pair forever — and an origin that chooses what to serve
	// could answer "the latest is v9.9.9" and hand back the authentic v0.4.0
	// pair with every signature check passing. The trusted comment is the field
	// that closes that, because it is signed and it is ours to define.
	if err := relsign.Verify(keys, sums, sig, want.Tag()); err != nil {
		reason, remedy := updateVerifyFailure(err)
		return rep.emit(cmd, opts.asJSON, ExitBlocked, reason,
			fmt.Sprintf("The signature on %s's checksum file did not verify: %v\n%s", want.Tag(), err, remedy))
	}
	say(cmd, opts.asJSON, "Verified sha256sums.txt against ccdad's release key.")

	// Step 12. Verified, and still possibly an HTML page: a signature says who
	// wrote the bytes and never what they are.
	if !release.SumsLookLikeSums(sums) {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "shape",
			fmt.Sprintf("The checksum file for %s carries no checksums at all, so ccdad will not install it. "+
				"It came correctly signed, which makes this worse rather than better. "+
				"Check https://github.com/Kweiza/ccdaddy/releases, and read the file yourself with "+
				"`minisign -Vm sha256sums.txt`.", want.Tag()))
	}

	// Step 13. Not being listed is its own refusal and its own remedy: the
	// release simply does not carry this platform, which is a fact about the
	// release rather than a suspicion about it.
	asset := release.Asset()
	wantHash, listed := release.ExpectedHash(sums, asset)
	if !listed {
		return rep.emit(cmd, opts.asJSON, ExitBlocked, "not-listed",
			fmt.Sprintf("Release %s does not publish %s, so there is nothing here for this machine. "+
				"Check https://github.com/Kweiza/ccdaddy/releases for what it does carry.", want.Tag(), asset))
	}

	// Step 14, and its POSITION is the whole of it. A --check that answered
	// "available" for a release the run would refuse on signature, shape or
	// listing would be worse than no --check: it must fail everything the full
	// run fails except what only downloading can find. Stopping here means the
	// asset's size, its digest and whether it runs are genuinely out of reach,
	// which is what --help says rather than leaving the user to discover.
	//
	// It is not a read-only command: the staging directory above was created
	// and will be removed, which is what makes its answer about writability the
	// real one.
	//
	// The wording branches on rep.updateAvailable rather than being printed
	// unconditionally, because step 7 is SKIPPED when --version is explicit:
	// `ccdad update --check --version <the tag already running>` reaches here
	// with nothing newer to offer, and a fixed sentence would answer
	// "ccdad 0.7.0 is available; this is 0.7.0" and exit 0. That request is a
	// legitimate one — it is how a user asks whether a specific release still
	// verifies — so the answer is re-worded rather than refused.
	if opts.check {
		if rep.updateAvailable {
			say(cmd, opts.asJSON, "ccdad %s is available; this is %s.", want, rep.current)
			say(cmd, opts.asJSON, "Run `ccdad update` to install it. Its size, its checksum and whether it "+
				"runs on this machine are only known once it has been downloaded.")
		} else {
			say(cmd, opts.asJSON, "ccdad %s verifies, and it is not newer than the %s running here.",
				want, rep.current)
			say(cmd, opts.asJSON, "Run `ccdad update --version %s` to fetch and re-verify it anyway. Its size, "+
				"its checksum and whether it runs on this machine are only known once it has been "+
				"downloaded.", want.Tag())
		}
		return rep.emit(cmd, opts.asJSON, ExitOK, "", "")
	}

	// ---------------------------- BEGIN PLACEHOLDER ----------------------------
	// Everything from this comment down to END PLACEHOLDER is scaffolding, and
	// it is meant to be deleted WHOLE — comment, blank assignments and return
	// together. Whoever writes the rest of the algorithm (downloading the asset
	// into staging, checking its size and digest against wantHash, running it
	// once and moving it over target) replaces this block; leaving the comment
	// behind would leave it sitting above live code, describing something that
	// is no longer true.
	//
	// It exists so this command RUNS while the rest is still being written: it
	// reports what it has verified and stops. The blank assignments are what
	// keep the compiler quiet about values only the later steps read. Nothing
	// asserts on any of it.
	_, _ = asset, wantHash
	_ = staging
	return rep.emit(cmd, opts.asJSON, ExitOK, "", "")
	// ----------------------------- END PLACEHOLDER -----------------------------
}

// upgradeHint is the package manager's own upgrade command. uninstallHint is
// the same shape for the other verb; the two are separate because a user
// reading "run brew uninstall" when they asked to upgrade would do it.
func upgradeHint(owner string) string {
	if owner == "Scoop" {
		return "'scoop update ccdad'"
	}
	return "'brew upgrade ccdad'"
}

// updateVerifyFailure maps a verification failure to its reason and the remedy
// the user is given.
//
// The SPLIT is a security decision rather than a wording one. Neither installer
// performs a signature check, so routing a TAMPER failure to "re-run the
// installer" would send the user to the one path that will happily accept the
// altered release, on checksums the same attacker controls.
//
// An unsigned release, one signed by a key this build predates, and one using
// an algorithm this build cannot read are all a deliberate choice somewhere
// rather than an attack — for those the installer IS the way forward.
func updateVerifyFailure(err error) (reason, remedy string) {
	const reinstall = "Re-run the installer from the project's README to move to a release this build can verify."
	const distrust = "This release cannot be trusted, and re-running the installer would not help: " +
		"the installers check checksums and not signatures. Take it up at " +
		"https://github.com/Kweiza/ccdaddy/releases, and check the file yourself with `minisign -Vm sha256sums.txt`."

	switch {
	case errors.Is(err, relsign.ErrKeyID):
		return "key-id", reinstall
	case errors.Is(err, relsign.ErrAlgorithm):
		return "algorithm", reinstall
	case errors.Is(err, relsign.ErrRelease):
		return "wrong-release", distrust
	case errors.Is(err, relsign.ErrMalformed):
		return "malformed", distrust
	}
	// Anything this build does not recognise — including ErrSignature itself —
	// takes the distrust remedy. That is the safe half of the split, and it
	// means a sentinel added to relsign later cannot silently arrive with the
	// "re-run the installer" advice attached to it.
	return "signature", distrust
}
