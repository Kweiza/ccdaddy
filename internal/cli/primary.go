package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// `ccdad primary` is the one per-account verb that changes what the engine may
// SPEND, which is why it is the one verb in this tree that says what it means
// before it acts.
//
// Everywhere else, credits are OVERAGE. A subscription's quota is already paid
// for, so it is spent first, and reaching for credits at all needs two
// independent opt-ins: the account's own extra-usage switch, and a
// max_auto_spend ceiling a human wrote down. A seat billed ONLY in credits
// breaks that premise -- there is no subscription quota to prefer, the credits
// are the seat's ordinary metering rather than an overage of it, and a ceiling
// whose default is 0 means such a seat can never be used at all.
//
// So this flag turns that ceiling off for one account, and the command IS the
// second opt-in: a human typed it, naming the account. That is the whole
// argument, and it is why the notice is printed before the write rather than
// after it.

func newPrimaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "primary <ACCOUNT> on|off",
		Short: "Rank a credit-metered seat with the accounts whose quota is paid for",
		Long: "Rank a credit-metered seat with the accounts whose quota is paid for.\n\n" +
			"A credit account is normally a last resort: the engine reaches it only once\n" +
			"every other account is spent, and only under the max_auto_spend ceiling. A\n" +
			"seat billed only in credits has no other quota to prefer, so 'primary on'\n" +
			"ranks it alongside the subscription accounts -- on credit.threshold minus its\n" +
			"credit utilization -- and max_auto_spend stops applying to that account.\n\n" +
			"It is a fact about the account rather than a tuning value, and it survives a\n" +
			"reclassification.",
		Args:          usageArgs(cobra.ExactArgs(2)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			on, err := primaryVerb(args[1])
			if err != nil {
				return err
			}
			return setPrimary(cmd, args[0], on)
		},
	}
}

// primaryVerb reads the on|off argument.
//
// strconv.ParseBool is deliberately not used: it accepts "1", "t" and "TRUE"
// and REJECTS "on", which is the one spelling the usage line asks for. A verb
// this cannot read is a usage error rather than a guess, because the two
// guesses are "spend nothing" and "spend automatically".
func primaryVerb(arg string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, UsageError("primary takes 'on' or 'off', not %q", arg)
}

func setPrimary(cmd *cobra.Command, ref string, primary bool) error {
	s, err := store.Open()
	if err != nil {
		return err
	}
	accounts := s.Accounts()
	target, err := store.Resolve(accounts, ref)
	if err != nil {
		return UsageError("%s", err.Error())
	}

	// Before the write, and only on the way ON.
	//
	// The account this reads is the pre-lock copy, so a racing writer could
	// make the line redundant. That is the harmless direction. The other
	// direction is a spending ceiling removed by a command that never said so,
	// which is why this is not deferred until the setter reports a change.
	if primary && !target.Primary {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Turning primary on for %s removes the max_auto_spend ceiling for that account: "+
				"the engine may spend its credits automatically, with no second opt-in.\n",
			target.Label())
	}

	changed, err := s.SetPrimary(target.UUID, primary)
	if err != nil {
		return err
	}

	state := "primary"
	if !primary {
		state = "not primary"
	}
	if !changed {
		// Exit 3 is "the world is already as you asked". Reporting 0 here would
		// tell a cron job it changed something it did not.
		fmt.Fprintf(cmd.ErrOrStderr(), "%s is already %s.\n", target.Label(), state)
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s is now %s.\n", target.Label(), state)

	// The engine reads this flag only for a credit-metered account, so on any
	// other kind it is stored and inert. It is stored anyway rather than
	// refused: store.ApplyUsage revises Kind from every successful poll, so a
	// seat added before its first reading is classified as a subscription and
	// becomes a credit account later, without the user touching this again.
	if primary && target.Kind != identity.KindCredit {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Note: %s is classified as %s right now, and the flag is read only for a credit-metered "+
				"account. It is stored either way, and every usage reading re-checks the classification.\n",
			target.Label(), target.Kind)
	}
	return nil
}
