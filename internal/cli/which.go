package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// credentialIdentity is the value attribution compares, for accounts whose
// credential really does live in the credentials file.
//
// The refresh token leads because it survives an access-token rotation: a
// Claude Code that has refreshed since the switch still carries it. An access
// token is the fallback, for a record that has been through Claude Code's
// dead-token clear or was written without one.
//
// There is deliberately no API-key case: the claudeAiOauth object has exactly
// eight keys and none of them is an API key, so a branch for one would be dead
// code pretending to be defensive. Token accounts are matched by
// attributeLogin instead, where Claude Code actually reads them from.
//
// The kind prefix is load-bearing: without it an access token stored by one
// account could match a refresh token stored by another. "" means "cannot
// identify" and never matches anything, including another "" — which is the
// case for every token account, whose stored blob has no OAuth record at all.
func credentialIdentity(b cclink.Blob) string {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return ""
	}
	var payload struct {
		RefreshToken string `json:"refreshToken"`
		AccessToken  string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	switch {
	case payload.RefreshToken != "":
		return "refresh:" + payload.RefreshToken
	case payload.AccessToken != "":
		return "access:" + payload.AccessToken
	}
	return ""
}

// attributeLogin answers "which managed account is Claude Code actually using".
//
// It is not always the credentials file. CLAUDE_CODE_OAUTH_TOKEN takes
// precedence over the stored claudeAiOauth in Claude Code itself, so when that
// variable is set the file is not the answer and must not be consulted as a
// fallback — reporting the file's account would name one Claude Code is not
// using. A headless machine, which is the whole reason `ccdad add-token`
// exists, is exactly where this matters.
//
// ANTHROPIC_API_KEY is deliberately NOT handled here. Claude Code accepts it
// only when its 20-character suffix is already in customApiKeyResponses.approved
// in ~/.claude.json, and it competes with apiKeyHelper and the stored
// primaryApiKey in an order ccdad does not yet model. Guessing at that order
// would produce a confident wrong answer, which is worse than "not managed".
func attributeLogin(live cclink.Blob, accounts []store.Account, lookup func(uuid string) (cclink.Blob, error)) (store.Account, bool) {
	if envToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); envToken != "" {
		for _, a := range accounts {
			stored, err := lookup(a.UUID)
			if err != nil {
				continue
			}
			if rec, ok := tokenRecordOf(stored); ok && rec.Token == envToken {
				return a, true
			}
		}
		return store.Account{}, false
	}
	return attributeWith(live, accounts, lookup)
}

// attributeWith matches the live credentials file against the managed accounts,
// taking the credential lookup as a parameter so it can be tested without a
// real store.
func attributeWith(live cclink.Blob, accounts []store.Account, lookup func(uuid string) (cclink.Blob, error)) (store.Account, bool) {
	liveID := credentialIdentity(live)
	if liveID == "" {
		return store.Account{}, false
	}
	for _, a := range accounts {
		stored, err := lookup(a.UUID)
		if err != nil {
			continue
		}
		if credentialIdentity(stored) == liveID {
			return a, true
		}
	}
	return store.Account{}, false
}

func newWhichCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:           "which",
		Short:         "Show which managed account Claude Code is logged in as",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			live, err := cclink.Load()
			if err != nil {
				return err
			}
			acct, ok := attributeLogin(live, s.Accounts(), s.Credentials)

			if asJSON {
				payload := map[string]any{"schemaVersion": 1, "attributed": ok}
				if ok {
					payload["account"] = accountJSON(acct)
				}
				if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
					payload["unknownKeys"] = unknown
				}
				if err := writeJSON(cmd, payload); err != nil {
					return err
				}
				if !ok {
					// The exit code is the same with or without --json: the flag
					// changes the representation, never the answer.
					return WithCode(errSilent, ExitProbeNegative)
				}
				return nil
			}

			if !ok {
				fmt.Fprintln(cmd.ErrOrStderr(), "The current login is not one ccdad manages.")
				// A negative probe answer, not a failure: exit 5 so a script can
				// tell it from a real error.
				return WithCode(errSilent, ExitProbeNegative)
			}
			fmt.Fprintln(cmd.OutOrStdout(), acct.Label())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}
