package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// tokenRecordOf reports the ccdad token record a blob carries, if any.
//
// A token account is not installable as a Claude Code login: Claude Code reads
// an API key from ~/.claude.json and a setup token from an environment
// variable, never from the credentials file.
func tokenRecordOf(b cclink.Blob) (tokenRecord, bool) {
	raw, ok := b[tokenCredentialKey]
	if !ok {
		return tokenRecord{}, false
	}
	var rec tokenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return tokenRecord{}, false
	}
	return rec, true
}

// envVarFor names the mechanism Claude Code actually reads a token kind from.
func envVarFor(kind string) string {
	if kind == "api-key" {
		return "ANTHROPIC_API_KEY"
	}
	return "CLAUDE_CODE_OAUTH_TOKEN"
}

// exactlyOneAccount is spelled out rather than delegated to cobra.ExactArgs so
// the violation carries this binary's exit code and a message that says what to
// do next. Cobra's own Args errors are plain errors, which would exit 1.
func exactlyOneAccount(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return UsageError("%s needs exactly one account; run 'ccdad list' to see them", verb)
		}
		return nil
	}
}

func newSwitchCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:           "switch <ACCOUNT>",
		Short:         "Make an account the live Claude Code login",
		Long:          "ACCOUNT may be a display index, an alias, an email address, or a uuid prefix.",
		Args:          exactlyOneAccount("switch"),
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
				// Every Resolve failure is the caller naming something that does
				// not exist, which is a usage error under the exit contract.
				return UsageError("%s", err.Error())
			}

			creds, err := s.Credentials(target.UUID)
			if err != nil {
				return err
			}
			// An account can hold both a browser login and a token. The OAuth
			// record is what goes in the credentials file, so a token sitting
			// beside it must not make the account look uninstallable — only an
			// account with NO OAuth record is unswitchable.
			if _, hasOAuth := creds["claudeAiOauth"]; !hasOAuth {
				if rec, isToken := tokenRecordOf(creds); isToken {
					return UsageError("%s is an %s account; Claude Code reads that credential from %s, so there is nothing to install in the credentials file",
						target.Label(), rec.Kind, envVarFor(rec.Kind))
				}
			}

			// A live file we cannot read is not a reason to refuse the switch:
			// attribution here is only the already-on optimization, and Activate
			// re-reads under the lock anyway. This is the state where switching
			// to a known-good account matters most.
			live, err := cclink.Load()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: could not read the current login (%v); switching anyway\n", err)
				live = nil
			}
			// attributeWith, not attributeLogin, and deliberately: this asks
			// "is the FILE already this account", because the file is what a
			// switch rewrites. attributeLogin would answer about the
			// environment token instead, which switching does not touch — and
			// the override is reported separately below.
			if current, ok := attributeWith(live, accounts, s.Credentials); ok && current.UUID == target.UUID && !force {
				fmt.Fprintf(cmd.ErrOrStderr(), "Already on %s.\n", target.Label())
				return WithCode(errSilent, ExitNothingToDo)
			}

			// Spec §4.3: drift in the credentials file is demonstrated, not
			// hypothetical — six machine keys appeared after clauth's carry list
			// was written. Merge preserves what it does not recognize, but the
			// operator still needs to know a new key exists.
			if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
					strings.Join(unknown, ", "))
			}

			if err := cclink.Activate(creds); err != nil {
				return err
			}
			if err := s.SetActive(target.UUID); err != nil {
				return err
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Switched to %s.\n", target.Label())
			// Claude Code reads CLAUDE_CODE_OAUTH_TOKEN in preference to the
			// credentials file, so with that variable set the switch has done its
			// work and still changed nothing about what Claude Code uses.
			if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: CLAUDE_CODE_OAUTH_TOKEN is set, and Claude Code reads it in preference to the credentials file. "+
						"Unset it for this switch to take effect.")
			}
			// Claude Code re-reads the credentials file on its next request, so a
			// running session picks this up without a restart.
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "activate even when this account is already live")
	return cmd
}
