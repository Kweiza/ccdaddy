package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/config"
	engine "github.com/Kweiza/ccdaddy/internal/strategy"
)

var selectableStrategies = []string{"hover", "manual", "headroom", "consume-first"}

// selectedStrategy is the single user-facing policy represented by the three
// compatibility fields in config.toml. Manual wins an old file that enabled
// both booleans: preserving the no-switch promise is safer than silently
// resuming automatic changes. Every write made by `ccdad strategy` clears that
// ambiguous state.
func selectedStrategy(cfg config.Config) string {
	if cfg.Manual {
		return "manual"
	}
	if cfg.Hover {
		return "hover"
	}
	return cfg.Strategy.String()
}

func newStrategyCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "strategy hover|manual|headroom|consume-first",
		Short:     "Choose how ccdad switches accounts",
		ValidArgs: selectableStrategies,
		Long: "Choose exactly one switching policy. hover derives thresholds from each\n" +
			"window's pace; manual keeps every reading current but never switches;\n" +
			"headroom prefers the account with the most room; consume-first spends\n" +
			"perishable weekly quota before it expires.\n\n" +
			"The selected policy is shown by `ccdad status` and in the dashboard.",
		Args:          usageArgs(cobra.ExactArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !isSelectableStrategy(name) {
				return UsageError("unknown strategy %q: one of %s", name, strings.Join(selectableStrategies, ", "))
			}
			return setStrategy(cmd, name)
		},
	}
}

func isSelectableStrategy(name string) bool {
	for _, candidate := range selectableStrategies {
		if name == candidate {
			return true
		}
	}
	return false
}

func setStrategy(cmd *cobra.Command, name string) error {
	changed := false
	err := config.WithDocument(func(d *config.Document) error {
		cfg, err := d.Config()
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: %s still cannot be used and the engine will run on its built-in defaults until it can: %v\n",
				config.FileName, err)
			cfg = config.Defaults()
		}

		if strategyIsExclusive(cfg, name) {
			return nil
		}
		changed = true
		if err := d.Set(config.KeyHover, strconv.FormatBool(name == "hover")); err != nil {
			return err
		}
		if err := d.Set(config.KeyManual, strconv.FormatBool(name == "manual")); err != nil {
			return err
		}
		automatic := name
		if name == "hover" || name == "manual" {
			automatic = engine.StrategyHeadroom.String()
		}
		parsed, _ := engine.ParseStrategy(automatic)
		if err := d.Set(config.KeyStrategy, parsed.String()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(cmd.ErrOrStderr(), "strategy is already %s.\n", name)
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "strategy = %s\n", name)
	return nil
}

func strategyIsExclusive(cfg config.Config, name string) bool {
	switch name {
	case "hover":
		return cfg.Hover && !cfg.Manual && cfg.Strategy == engine.StrategyHeadroom
	case "manual":
		return cfg.Manual && !cfg.Hover && cfg.Strategy == engine.StrategyHeadroom
	default:
		return !cfg.Hover && !cfg.Manual && cfg.Strategy.String() == name
	}
}
