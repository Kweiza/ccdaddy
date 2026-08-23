package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// bootstrapEnvVar names the document to import. It is an environment variable
// rather than an argument because the caller is a container entrypoint that
// runs the same line whether or not there is anything to import: a shell that
// had to test for the document first would be a second copy of that rule, in a
// language with no tests here.
const bootstrapEnvVar = "CCDAD_IMPORT"

func newBootstrapCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Import the accounts named by CCDAD_IMPORT, if any",
		Long: "Import the accounts named by CCDAD_IMPORT: a path to a document written by\n" +
			"'ccdad export', or '-' to read stdin.\n\n" +
			"With the variable unset or empty this does nothing and exits 0, so a container\n" +
			"entrypoint can run it unconditionally. It is idempotent: an account already\n" +
			"here at that uuid is updated, one that is not is added, and an account's age is\n" +
			"not moved by a re-run.\n\n" +
			"Credentials that are newer here than in the document are kept. That is the rule\n" +
			"worth knowing before you run this on every start: the document is a snapshot,\n" +
			"and the tokens in it go stale while the ones in the store are refreshed.\n" +
			"--force overwrites them anyway.\n\n" +
			"It never answers 3. The store carrying the document's accounts is the outcome\n" +
			"this command was asked for, whether this run put them there or an earlier one\n" +
			"did, and an entrypoint under 'set -e' would refuse to start the container on\n" +
			"every restart after the first.\n\n" +
			"Nothing from the document reaches this command's output. It holds live refresh\n" +
			"tokens, and this output is a container log.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrap(cmd, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite credentials that are newer here than in the document")
	return cmd
}

func runBootstrap(cmd *cobra.Command, force bool) error {
	// Empty is treated as unset, and both are silent. `docker run -e
	// CCDAD_IMPORT` with no value sets the variable to the empty string, and an
	// os.Open("") would report "no such file or directory" — a container that
	// refuses to start over a variable the operator meant to leave out.
	// ccpath.StoreHome reads CCDAD_HOME the same way.
	path := os.Getenv(bootstrapEnvVar)
	if path == "" {
		return nil
	}

	payload, err := readExport(cmd, path)
	if err != nil {
		return err
	}
	if payload.Machine != nil {
		// A fixed sentence, carrying nothing out of the document. It is worth
		// saying because it describes something that did NOT happen and that a
		// reader restoring a backup will otherwise go looking for.
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: that document carries MCP logins. They are not being installed — "+
				"they belong to the machine they were taken from, not to any account here.")
	}
	if err := validateExport(payload); err != nil {
		return refuseBootstrapDocument()
	}

	imported, skipped, err := applyImport(payload, force)
	if IsUsageError(err) {
		// applyImport's only usage error is the alias collision, and its message
		// names the alias out of the document together with the local account
		// already holding it. Everything else it can return is a store write
		// failing, and those name a path under CCDAD_HOME rather than anything
		// in the file.
		return refuseBootstrapDocument()
	}
	if err != nil {
		return err
	}

	// 0, never 3. `ccdad import`'s "Nothing was imported." is 3 because a person
	// asked for something and got nothing. Here the store carries the document's
	// accounts either way — a second start of the same container is the ordinary
	// case, not an anomaly — and an entrypoint under `set -e` treats any non-zero
	// as a reason not to run the container's own command.
	fmt.Fprintf(cmd.ErrOrStderr(), "Bootstrapped %d account(s) from %s; %d left as they are.\n",
		len(imported), bootstrapEnvVar, len(skipped))
	return nil
}

// refuseBootstrapDocument is the one refusal this command has for a document it
// read and would not apply.
//
// The reason is deliberately not repeated. validateExport and
// checkAliasCollisions quote the uuid, the alias or the email address they
// refused, straight out of a file that also holds live refresh tokens — and
// this command's stderr is a container log. `ccdad import` says the same thing
// to whoever is at the terminal, which is where a document like that should be
// described.
func refuseBootstrapDocument() error {
	return UsageError("%s does not name a usable ccdad export. Run `ccdad import` against that "+
		"document from a shell to see what is wrong with it; this command will not describe a "+
		"file holding refresh tokens into a log", bootstrapEnvVar)
}
