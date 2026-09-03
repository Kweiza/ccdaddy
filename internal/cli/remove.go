package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// profilePathFor is where `ccdad run --full-profile` keeps an account's config
// home, or "" when the store root cannot be resolved.
//
// A store that cannot be resolved answers "" rather than an error: every
// caller is on a path that has already done its real work, and turning a
// removal that succeeded into a failure over a directory that may not exist is
// the wrong trade.
func profilePathFor(uuid string) string {
	root, err := ccpath.StoreHome()
	if err != nil {
		return ""
	}
	return filepath.Join(root, ProfilesDirName, uuid)
}

// profileExists reports whether there is anything at path.
//
// Existence, not "is a directory". An earlier version checked IsDir and
// mutation testing showed the check could be deleted with nothing failing:
// ccdad only ever creates that path with MkdirAll, so a plain file there is a
// state nothing produces, and if one somehow appeared it is inside ccdad's own
// store under the account's uuid and RemoveAll is still the right answer. The
// distinction bought a branch no test could reach.
func profileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func newRemoveCmd() *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:           "remove <ACCOUNT>",
		Short:         "Stop managing an account and delete its stored credentials",
		Args:          exactlyOneAccount("remove"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()
			target, err := store.Resolve(accounts, args[0])
			if err != nil {
				return UsageError("%s", err.Error())
			}

			// Attribute before removing: afterwards the stored credentials are
			// gone and the question can no longer be answered.
			live, _ := cclink.Load()
			current, hasLive := attributeLive(live, accounts, s.Credentials)
			wasLive := hasLive && current.UUID == target.UUID
			// The same question on the codex side, asked here for the same
			// reason: the pointer is resolved against the account list, and in
			// a moment this account will not be in it.
			serving, hasServing := codexServingAccount(accounts)
			wasServing := hasServing && serving.UUID == target.UUID

			// The profile is named in the confirmation because it is not the
			// same thing as the stored credentials: it is what the account
			// accumulated across `--full-profile` runs, its MCP logins and
			// trust answers among them. Deleting it unannounced would be a
			// larger loss than the sentence promised.
			profile := profilePathFor(target.UUID)
			hasProfile := profile != "" && profileExists(profile)
			alsoProfile := ""
			if hasProfile {
				alsoProfile = ", and its --full-profile session profile at " + profile
			}

			// A destructive command with no terminal to confirm at must be told
			// explicitly, or a script deletes credentials nobody meant to lose.
			if !assumeYes {
				if !stdinIsTTY() {
					return UsageError("removing %s deletes its stored credentials%s; pass --yes to confirm", target.Label(), alsoProfile)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Remove %s and delete its stored credentials%s? [y/N] ", target.Label(), alsoProfile)
				var answer string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
				if answer != "y" && answer != "Y" {
					fmt.Fprintln(cmd.ErrOrStderr(), "Left alone.")
					return WithCode(errSilent, ExitNothingToDo)
				}
			}

			if err := s.Remove(target.UUID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Removed %s.\n", target.Label())

			// The profile is a SECOND place this account's credentials live,
			// and since `ccdad run --full-profile` began writing an API-key
			// account's primaryApiKey into it, an orphaned profile is an
			// orphaned CREDENTIAL rather than stale configuration. Nothing
			// else cleans it: uninstall removes the whole store, and doctor
			// scans sessions only.
			//
			// Reported as a warning rather than returned, because it happens
			// after the account is already gone from the store: failing here
			// would report a completed removal as a failure, and the sentence
			// a user needs is where the file still is.
			if hasProfile {
				if err := os.RemoveAll(profile); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: %s's profile could not be removed (%v). It is at %s and may hold that "+
							"account's API key; delete it by hand.\n", target.Label(), err, profile)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "Removed its session profile at %s.\n", profile)
				}
			}
			if wasLive {
				// The credentials are gone from ccdad but still installed in
				// Claude Code, so the user stays logged in as an account ccdad
				// can no longer switch back to. That must not be silent.
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: that account is still the live Claude Code login, and ccdad no longer has its credentials. "+
						"Run 'ccdad switch' to move to a managed account.")
			}
			// The codex pointer is CLEARED rather than left to read as nothing.
			// Both end with the proxy choosing the top-ranked account, so the
			// behaviour is the same either way -- but a pointer naming an
			// account nobody can find is a document that lies about what this
			// machine is spending, and `ccdad which` would go on reading it.
			//
			// Reported as a warning rather than returned, for the reason the
			// profile removal above is: the account is already gone from the
			// store, so failing here would report a completed removal as a
			// failure.
			if wasServing {
				root, rerr := codexRoot()
				if rerr == nil {
					rerr = codexswitch.Clear(root)
				}
				if rerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: that account was the one codex was served from, and the pointer at %s could "+
							"not be cleared (%v). Run 'ccdad switch --provider codex' to repoint it.\n",
						namePath(codexPointerPath()), rerr)
				} else {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"Note: that was the account codex was served from, so nothing is serving codex now. "+
							"Run 'ccdad switch --provider codex' to pick another one.")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not prompt for confirmation")
	return cmd
}
