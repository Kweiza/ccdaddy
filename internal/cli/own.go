package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// `ccdad own` declares which accounts THIS machine drives.
//
// It exists because two ccdad installs cannot see each other. Ranking is a pure
// function of readings the server shares between them, and every comparator ends
// in a uuid tie-break, so two machines given the same pool pick the same target
// and stack two sessions' inference onto one five-hour window while the rest of
// the pool sits idle. No lock in this tree crosses a machine boundary, and none
// does in either reference project either, so the partition cannot be discovered
// at runtime -- it has to be declared.
//
// Why this is not `ccdad disable` run N times. Three differences, and the third
// is the one that matters. Disabled accounts are still POLLED, because a named
// `ccdad switch` wants a fresh reading; an account another machine owns is never
// polled on a cadence, because the reading spends a budget shared with whoever is
// driving it and this machine can never rank it anyway. Disabled says something
// about the account, so `ccdad status` explains it as a policy; Elsewhere says
// something about this machine. And a partition is DECLARATIVE: an account added
// later is somebody else's by default, where a machine partitioned by disabling
// would silently start sharing every account added after the last disable.

func newOwnCmd() *cobra.Command {
	var clear bool

	cmd := &cobra.Command{
		Use:   "own [ACCOUNT...]",
		Short: "Declare which accounts this machine drives",
		Long: "Declare which accounts this machine drives.\n\n" +
			"Run this on every machine when you use ccdad on more than one, giving each\n" +
			"a set that does not overlap. Two machines sharing an account rank it the same\n" +
			"way and switch to it at the same moment, so both sessions burn one five-hour\n" +
			"window while the others sit idle — and nothing in ccdad can see that happening,\n" +
			"because no lock it holds crosses a machine.\n\n" +
			"Accounts this machine does not own are neither rotated into nor polled, and an\n" +
			"account added later belongs to another machine by default. 'ccdad switch' still\n" +
			"activates one by name, and '--refresh' still reads one by name: naming it by\n" +
			"hand says what you want more clearly than the declaration does.\n\n" +
			"With no arguments it prints the current split. '--clear' gives every account\n" +
			"back to this machine.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOwn(cmd, args, clear)
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false,
		"Own every account again, undoing any split")
	return cmd
}

func runOwn(cmd *cobra.Command, args []string, clear bool) error {
	if clear && len(args) > 0 {
		return UsageError("own takes either --clear or a list of accounts, not both")
	}
	s, err := store.Open()
	if err != nil {
		return err
	}
	accounts := s.Accounts()
	if len(accounts) == 0 {
		// Ownership is a fact about the whole store, so this refusal knows
		// nothing about providers and offers both rather than picking one.
		return UsageError("there are no accounts yet; run 'ccdad add claude' or 'ccdad add codex' first")
	}

	// No arguments is a QUESTION, not "own nothing". Owning nothing would park
	// the engine with no target at all, and it is not a state worth making easy
	// to type by accident -- there is no spelling of this command that reaches it.
	if !clear && len(args) == 0 {
		printOwnership(cmd, accounts)
		return nil
	}

	uuids := make([]string, 0, len(args))
	// Resolved BEFORE anything is written, all of them, because SetOwned is one
	// statement about the whole list: a half-applied partition leaves the machine
	// owning a set nobody asked for.
	for _, ref := range args {
		target, err := store.Resolve(accounts, ref)
		if err != nil {
			return UsageError("%s", err.Error())
		}
		uuids = append(uuids, target.UUID)
	}

	changed, err := s.SetOwned(uuids)
	if err != nil {
		return err
	}
	if changed == 0 {
		// Exit 3 is "the world is already as you asked".
		fmt.Fprintln(cmd.ErrOrStderr(), "That is already the split on this machine.")
		return WithCode(errSilent, ExitNothingToDo)
	}
	printOwnership(cmd, s.Accounts())

	// Owning an account here says nothing about whether another machine has
	// stopped owning it, and that is the half this command cannot check.
	if !clear {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Run 'ccdad own' on the other machines too. This one cannot tell whether any of "+
				"them still drives an account it just claimed, and an account driven from two "+
				"machines spends its window twice as fast.")
	}
	return nil
}

// printOwnership writes the split to stderr, the same stream every other verb in
// this file reports on, so a script reading stdout is unaffected.
func printOwnership(cmd *cobra.Command, accounts []store.Account) {
	var here, there []string
	for _, a := range accounts {
		if a.Elsewhere {
			there = append(there, a.Label())
			continue
		}
		here = append(here, a.Label())
	}
	sort.Strings(here)
	sort.Strings(there)

	w := cmd.ErrOrStderr()
	if len(there) == 0 {
		fmt.Fprintf(w, "This machine owns all %d accounts (no split).\n", len(here))
		return
	}
	fmt.Fprintf(w, "This machine drives: %s\n", strings.Join(here, ", "))
	fmt.Fprintf(w, "Another machine drives: %s\n", strings.Join(there, ", "))
	if len(here) == 0 {
		fmt.Fprintln(w,
			"Note: this machine owns nothing, so automatic switching has nothing to rotate to.")
	}
}
