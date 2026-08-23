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
//
// The indentation is a decision, not a default: it is what jq-shaped consumers
// read and what a line-oriented one cannot, which is exactly why the one
// exception to the `--json` contract — `auto`'s NDJSON stream — must never
// come through here. A stream encoded by this helper arrives as `{`,
// `  "kind": …`, `}`, and `head -1` returns an opening brace.
//
// The `--json` contract is one rule for every read command rather than a habit
// each of them keeps, so it is asserted across the whole command tree in
// json_contract_test.go — including that every document has the shape this
// function gives it, which is how a command that stopped coming through here
// gets noticed.
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
// --help` states, in its stability contract, that it is an ordinal rather than
// a key — and no payload is ever keyed on it.
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
	if a.Primary {
		out["primary"] = true
	}
	return out
}

// namePath renders an already-resolved path for inclusion in a MESSAGE.
//
// Every caller is reporting some other failure and has already completed an
// operation that needed the same home directory, so the error branch is not
// reachable from a working machine. It exists because Go makes a two-value call
// unusable inside a format string, and because a message that silently contains
// an empty path is worse than one that says why it is empty.
func namePath(path string, err error) string {
	if err != nil {
		return fmt.Sprintf("(unresolved: %v)", err)
	}
	return path
}
