package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The four Accounts verbs that bring already-stored state to life: Disabled,
// which `list --all` has always filtered on with nothing able to set it;
// SetAlias, which was fully implemented, validated and collision-checked with
// exactly one caller (`add --alias`), so an account added without one could
// never get one and a typo could never be fixed; and idx, which is stored
// rather than derived but which nothing except arrival order ever assigned.
//
// All four go through the store's mutators, so each runs its read-modify-write
// under the cross-process lock.

func newDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <ACCOUNT>",
		Short: "Hold an account out of automatic rotation",
		Long: "Hold an account out of automatic rotation.\n\n" +
			"This is a policy for the auto engine, not a lock: 'ccdad switch <account>'\n" +
			"still activates a disabled account, because naming one by hand says what you\n" +
			"want more clearly than the flag does. 'ccdad list' hides it; 'ccdad list --all'\n" +
			"shows it.",
		Args:          exactlyOneAccount("disable"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return setDisabled(cmd, args[0], true) },
	}
}

func newEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "enable <ACCOUNT>",
		Short:         "Return an account to automatic rotation",
		Args:          exactlyOneAccount("enable"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return setDisabled(cmd, args[0], false) },
	}
}

func setDisabled(cmd *cobra.Command, ref string, disabled bool) error {
	s, err := store.Open()
	if err != nil {
		return err
	}
	accounts := s.Accounts()
	target, err := store.Resolve(accounts, ref)
	if err != nil {
		return UsageError("%s", err.Error())
	}

	changed, err := s.SetDisabled(target.UUID, disabled)
	if err != nil {
		return err
	}

	verb := "enabled"
	if disabled {
		verb = "disabled"
	}
	if !changed {
		// Exit 3 is "the world is already as you asked". Reporting 0 here
		// would tell a cron job it changed something it did not.
		fmt.Fprintf(cmd.ErrOrStderr(), "%s is already %s.\n", target.Label(), verb)
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s is now %s.\n", target.Label(), verb)

	// Disabling the account Claude Code is logged in as right now does not log
	// anyone out — the credentials file is untouched — but it does mean the
	// engine will rotate away from it and never rotate back, which is not
	// something to discover later. remove.go says its own version of this out
	// loud for the same reason.
	if disabled {
		live, _ := cclink.Load()
		if current, ok := attributeLive(live, accounts, s.Credentials); ok && current.UUID == target.UUID {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Note: that is the live Claude Code login. It stays live — disabling only holds it out of "+
					"automatic rotation, so nothing will switch back to it once something switches away.")
		}
		// The codex half of the same sentence, and the same rule: disabled is a
		// ROTATION policy, not a per-request gate. The proxy goes on serving a
		// disabled account that the pointer names -- exactly as Claude Code goes
		// on using a disabled login -- and the lane rotates away on its next
		// decision.
		if serving, ok := codexServingAccount(accounts); ok && serving.UUID == target.UUID {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Note: that is the account codex is served from. It keeps serving — disabling only "+
					"holds it out of automatic rotation, so nothing will switch back to it once "+
					"something switches away.")
		}
	}

	// Disabling the last enabled account is still a completed action, not the
	// blocked exit 4: nothing was blocked, and the state is exactly what was
	// asked for. The engine reports having no viable target when it next looks.
	if disabled && countEnabled(s.Accounts()) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: every account is now disabled, so automatic switching has nothing to rotate to.")
	}
	return nil
}

func countEnabled(accounts []store.Account) int {
	n := 0
	for _, a := range accounts {
		if !a.Disabled {
			n++
		}
	}
	return n
}

func newAliasCmd() *cobra.Command {
	var clear bool

	cmd := &cobra.Command{
		Use:   "alias <ACCOUNT> <ALIAS>",
		Short: "Give an account a short unique handle, or clear it",
		Long: "Give an account a short unique handle.\n\n" +
			"An alias is lowercased and trimmed, may contain a-z, 0-9, '.', '_' and '-',\n" +
			"may not be purely numeric (that is the display index), and may not start with\n" +
			"'-' (it would be read as a flag). Use --clear to remove one.",
		Args:          usageArgs(cobra.RangeArgs(1, 2)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case clear && len(args) == 2:
				return UsageError("--clear takes only the account; drop the alias argument")
			case !clear && len(args) == 1:
				return UsageError("alias needs an alias to set, or --clear to remove the one it has")
			case !clear && strings.TrimSpace(args[1]) == "":
				// An empty second argument is indistinguishable from a shell
				// that dropped a word, so it can never mean "clear". Clearing
				// has its own spelling precisely so this can be refused.
				return UsageError("an empty alias is not a way to clear one; pass --clear")
			}

			s, err := store.Open()
			if err != nil {
				return err
			}
			target, err := store.Resolve(s.Accounts(), args[0])
			if err != nil {
				return UsageError("%s", err.Error())
			}

			if clear {
				if target.Alias == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s has no alias.\n", target.Label())
					return WithCode(errSilent, ExitNothingToDo)
				}
				previous := target.Alias
				if err := s.SetAlias(target.UUID, ""); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Cleared the alias %q from %s.\n", previous, target.Label())
				return nil
			}

			// The stored form is the normalized one, so the collision check and
			// the echo-back both have to speak in it: telling the user "Work"
			// was set when "work" is what any later reference must use would
			// make the next `ccdad switch Work` look like the thing that broke.
			normalized := store.NormalizeAlias(args[1])
			if target.Alias == normalized {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s is already %q.\n", target.Label(), normalized)
				return WithCode(errSilent, ExitNothingToDo)
			}
			if err := s.SetAlias(target.UUID, normalized); err != nil {
				// A rejected alias and a taken one are both the caller naming
				// something unusable, which is exit 2, the usage error.
				// SetAlias's own collision message names the other account by
				// label and uuid and never by idx, which is what makes it
				// still true after a removal recompacts the ordinals.
				if errors.Is(err, store.ErrBadAlias) || errors.Is(err, store.ErrAliasTaken) {
					return UsageError("%s", err.Error())
				}
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s is now %q.\n", target.Label(), normalized)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the account's alias")

	// An alias may not start with '-', so it can never be read as a flag — but
	// pflag parses before Args ever runs, so `ccdad alias work -foo` fails with
	// "unknown shorthand flag: 'f' in -foo", which names neither the rule nor
	// the argument the user typed. Only the shorthand form is rewritten: a
	// mistyped long flag is a mistyped long flag.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		if strings.Contains(err.Error(), "shorthand flag") {
			return UsageError("%s. An alias may not start with '-' — it would be read as a flag; "+
				"pick another, or put '--' before it if you meant something else", err.Error())
		}
		return UsageError("%s", err.Error())
	})
	return cmd
}

func newMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "move <ACCOUNT> <POSITION>",
		Short: "Put an account at a display position",
		Long: "Put an account at a 1-based display position and renumber the rest.\n\n" +
			"A position past the end means last. Every account between the old and new\n" +
			"position changes number: idx is a display ordinal, not a key, so scripts must\n" +
			"reference accounts by uuid or alias.",
		Args:          usageArgs(cobra.ExactArgs(2)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A position is an ordinal, so 0 and anything negative are not a
			// position at all — unlike a number past the end, which is "last"
			// said imprecisely and clamps rather than failing.
			position, err := strconv.Atoi(args[1])
			if err != nil || position < 1 {
				return UsageError("a position is a whole number from 1 up, not %q", args[1])
			}

			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()
			target, err := store.Resolve(accounts, args[0])
			if err != nil {
				return UsageError("%s", err.Error())
			}

			moved, err := s.Move(target.UUID, position)
			if err != nil {
				return err
			}
			if !moved {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s is already at %d.\n", target.Label(), target.Idx)
				return WithCode(errSilent, ExitNothingToDo)
			}

			// Read the position back rather than echoing what was asked for: a
			// number past the end clamped, and saying "moved to 99" would be a
			// lie the very first time someone used it that way.
			landed, _ := s.Get(target.UUID)
			fmt.Fprintf(cmd.ErrOrStderr(), "%s is now at %d.\n", target.Label(), landed.Idx)
			return nil
		},
	}
}
