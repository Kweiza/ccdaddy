package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// errSilent carries an exit code without a message. Commands use it when they
// have already told the user what happened in their own words, so ExecuteWith
// must not print "ccdad: " on top of that.
var errSilent = errors.New("")

// writeJSON emits one object on stdout. Human notices go to stderr, so a
// --json caller always receives exactly one document.
func writeJSON(cmd *cobra.Command, payload any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("writing JSON output: %w", err)
	}
	return nil
}

// accountJSON renders an account for a --json payload.
//
// uuid leads because it is the key; idx is included for display but `ccdad
// --help` states, as spec §5.1 requires, that it is an ordinal rather than a
// key — and no payload is ever keyed on it.
func accountJSON(a store.Account) map[string]any {
	out := map[string]any{
		"uuid":  a.UUID,
		"idx":   a.Idx,
		"email": a.Email,
		"kind":  a.Kind.String(),
	}
	if a.Alias != "" {
		out["alias"] = a.Alias
	}
	if a.Tier != "" {
		out["tier"] = a.Tier
	}
	if a.Disabled {
		out["disabled"] = true
	}
	return out
}
