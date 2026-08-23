package cli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/config"
)

// `ccdad config` is the user-facing half of ~/.ccdad/config.toml.
//
// The exit taxonomy is the contract here, and two of its codes do real work:
//
//	get <KEY>     0 the file sets it · 5 it does not · 2 there is no such key
//	set/unset     0 done · 3 unset had nothing to remove · 2 bad key or value
//	list, path    0
//
// 5 rather than 1 for an unset key is what makes `ccdad config get x || default`
// safe: a probe answering "no" is not a failure. 2 stays usage-only, which is
// why an unknown KEY is refused rather than accepted — an accepted typo is a
// config that silently does nothing, and cron cannot see the difference.
//
// A file that cannot be DECODED is exit 1 from every verb, including set: the
// command was well formed and the file is not, and rewriting a document ccdad
// could not read would delete whatever the user was in the middle of typing.

// errNothingToUnset skips the write when `config unset` has nothing to remove.
// It never reaches the user: the command reports exit 3 in its own words.
var errNothingToUnset = errors.New("nothing to unset")

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and write ~/.ccdad/config.toml",
		Long: "The auto-switch engine's knobs: thresholds, anti-flap margins, the\n" +
			"strategy, and the credit ceiling.\n\n" +
			"Keys this ccdad does not know are left alone rather than deleted, so a file\n" +
			"written by a newer release survives an older one. Setting one is still a\n" +
			"usage error: a typo that is quietly accepted is a setting that does nothing.\n\n" +
			"No credential belongs here. This file is plain text and is what people paste\n" +
			"into a bug report; tokens live in the store's credentials directory.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			return UsageError("config needs a subcommand: one of get, set, unset, list, path")
		},
	}
	cmd.AddCommand(
		newConfigGetCmd(),
		newConfigSetCmd(),
		newConfigUnsetCmd(),
		newConfigListCmd(),
		newConfigPathCmd(),
	)
	return cmd
}

func newConfigGetCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Print one key's value, or exit 5 if the file does not set it",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			d, err := config.LoadDocument()
			if err != nil {
				return err
			}
			value, set, err := d.Value(key)
			if err != nil {
				return UsageError("%s", err.Error())
			}

			// The EFFECTIVE value is what the engine would run on, and it is
			// the useful half of a negative answer: "not set" alone leaves the
			// user guessing what is in force instead.
			cfg := effective(cmd, d)
			effectiveValue, verr := cfg.Value(key)
			if verr != nil {
				return UsageError("%s", verr.Error())
			}
			if !set {
				value = effectiveValue
			}

			if asJSON {
				source := "default"
				if set {
					source = "file"
				}
				if err := writeJSON(cmd, map[string]any{
					"schemaVersion": 1,
					"key":           key,
					"value":         value,
					"set":           set,
					"source":        source,
				}); err != nil {
					return err
				}
			} else if set {
				// Bare, with no key name and no quotes, so `$(ccdad config get
				// threshold)` is the value and nothing else.
				fmt.Fprintln(cmd.OutOrStdout(), value)
			}

			if !set {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s is not set in %s; the engine uses %s.\n",
					key, config.FileName, effectiveValue)
				return WithCode(errSilent, ExitProbeNegative)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object on stdout")
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Set one key, preserving everything else in the file",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			// What the file now holds, which is not always what was typed: a
			// duration is stored canonically, so `set cooldown 300s` has to
			// report 5m0s or the very next `get` contradicts it.
			stored := value
			err := config.WithDocument(func(d *config.Document) error {
				if err := d.Set(key, value); err != nil {
					// Both an unknown key and an unusable value are usage
					// errors, and returning it from INSIDE the callback is what
					// leaves the file untouched.
					return UsageError("%s", err.Error())
				}
				if written, set, verr := d.Value(key); verr == nil && set {
					stored = written
				}
				// The value is legal on its own; the document as a whole may
				// still not be, and the engine ignores an unusable file
				// wholesale. Setting a key into one would otherwise look like
				// it took effect.
				if _, cerr := d.Config(); cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: %s still cannot be used and the engine will run on its built-in defaults until it can: %v\n",
						config.FileName, cerr)
				}
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s = %s\n", key, stored)
			return nil
		},
	}
	return cmd
}

func newConfigUnsetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset KEY",
		Short: "Remove one key, returning it to its default",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			err := config.WithDocument(func(d *config.Document) error {
				removed, err := d.Unset(key)
				if err != nil {
					return UsageError("%s", err.Error())
				}
				if !removed {
					// Nothing to write. The file is left byte-for-byte as it
					// was rather than rewritten identically, so an unset that
					// changed nothing cannot look like an edit to the daemon.
					return errNothingToUnset
				}
				return nil
			})
			switch {
			case errors.Is(err, errNothingToUnset):
				fmt.Fprintf(cmd.ErrOrStderr(), "%s is not set in %s.\n", key, config.FileName)
				return WithCode(errSilent, ExitNothingToDo)
			case err != nil:
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s unset.\n", key)
			return nil
		},
	}
	return cmd
}

func newConfigListCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show every key, its value, and where that value came from",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := config.LoadDocument()
			if err != nil {
				return err
			}
			cfg := effective(cmd, d)
			unknown := d.UnknownKeys()
			if len(unknown) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: %s carries keys this ccdad does not know, and they are being ignored (not deleted): %v\n",
					config.FileName, unknown)
			}

			type row struct {
				Key    string `json:"key"`
				Value  string `json:"value"`
				Source string `json:"source"`
			}
			rows := make([]row, 0, len(config.Keys()))
			for _, key := range config.Keys() {
				value, verr := cfg.Value(key)
				if verr != nil {
					return verr
				}
				_, set, verr := d.Value(key)
				if verr != nil {
					return verr
				}
				source := "default"
				if set {
					source = "file"
				}
				rows = append(rows, row{Key: key, Value: value, Source: source})
			}

			if asJSON {
				path, err := config.Path()
				if err != nil {
					return err
				}
				payload := map[string]any{
					"schemaVersion": 1,
					"path":          path,
					"keys":          rows,
				}
				if len(unknown) > 0 {
					payload["unknownKeys"] = unknown
				}
				return writeJSON(cmd, payload)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tVALUE\tSOURCE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\n", r.Key, r.Value, r.Source)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object on stdout")
	return cmd
}

func newConfigPathCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print where the config file resolved to",
		Long: "CCDAD_HOME moves the whole store, and CLAUDE_CONFIG_DIR and\n" +
			"CLAUDE_SECURESTORAGE_CONFIG_DIR move what ccdad reads Claude Code out of, so\n" +
			"the answer is worth asking for rather than assuming.\n\n" +
			"It reports the path whether or not the file exists: that is where writing one\n" +
			"would put it.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, statErr := os.Stat(path)
			exists := statErr == nil

			if asJSON {
				home, err := ccpath.StoreHome()
				if err != nil {
					return err
				}
				return writeJSON(cmd, map[string]any{
					"schemaVersion": 1,
					"path":          path,
					"home":          home,
					"exists":        exists,
				})
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			if !exists {
				fmt.Fprintf(cmd.ErrOrStderr(), "There is no file there yet; 'ccdad config set' creates it.\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit one JSON object on stdout")
	return cmd
}

// effective is the config the engine would run on for this document.
//
// A document that decodes but does not validate is NOT an error here. The
// engine falls back to its defaults for exactly this file, so reporting
// anything else would tell the user their invalid value is in force; the note
// says which one is wrong, and the listing then shows what is really running.
func effective(cmd *cobra.Command, d *config.Document) config.Config {
	cfg, err := d.Config()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: %v; the values below are the built-in defaults, which is what the engine uses until the file is fixed.\n", err)
		return config.Defaults()
	}
	return cfg
}
