package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/zone"
)

// errSilent carries an exit code without a message. Commands use it when they
// have already told the user what happened in their own words, so ExecuteWith
// must not print "ccdad: " on top of that.
var errSilent = errors.New("")

// readerZone is the zone this machine's own documents and absolute stamps are
// rendered in, and it is the machine's.
//
// It covers two surfaces that are one rule: every `--json` document, and the
// handful of places a command prints an absolute moment to a person in a
// machine layout rather than through view.Timestamp -- `ccdad doctor`'s three.
// Both are read on the machine that produced them, piped into jq at the same
// prompt or read off the same terminal, so the zone that makes them readable is
// the reader's own. Rendering the moments the endpoint reported in UTC beside
// the moments ccdad computed in local time makes a document that cannot be read
// down the page: a live store carried five poll times at +09:00 beside one at
// Z, and the Z row looked nine hours overdue when it was four minutes in the
// future.
//
// It is a var so a test can pin a zone that is nobody's default. Nothing sets
// TZ in CI, so time.Local is UTC there, and a test that asserted against it
// would accept exactly the rows the bug leaves behind.
//
// It is deliberately NOT the zone of `ccdad export`, whose file is written to
// be carried to another machine: the writer's local offset is not the reader's
// there, and export.go keeps its own UTC for that reason.
//
// Nor is it view.Timestamp's, which takes its location as a parameter and falls
// back to UTC. That one is handed moments by arithmetic that must not touch the
// environment; this one is only ever reached from a command already running in
// front of the reader.
var readerZone = func() *time.Location { return time.Local }

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
	// One rule for every document rather than a habit each payload builder
	// keeps. The alternative is a .UTC() or .In() at each of the two dozen
	// places a moment enters a payload, which is the shape this bug arrived in:
	// the sites that remembered agreed with each other and the ones that forgot
	// published the zone their input happened to carry.
	payload = zone.In(payload, readerZone())
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
		// Unconditional, unlike the four optional keys below it. A consumer
		// cannot branch on a key that is sometimes absent without deciding
		// what absent means, and the only honest reading of an absent one is
		// "this ccdad predates Codex" -- a statement about the binary rather
		// than about the account.
		//
		// kind and provider are different axes and both are needed: kind says
		// how an account is METERED (subscription, credit, api-key) and
		// provider says whose service it is. A Codex account is a subscription
		// account, always.
		"provider": a.Provider.String(),
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
