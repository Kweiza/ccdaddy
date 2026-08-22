package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
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

// attribution is what `which` learned: the account, and how Claude Code gets
// to it. The how is reported even when the account is not one ccdad manages,
// because "not managed" plus "because apiKeyHelper is set" is actionable where
// "not managed" alone is not.
type attribution struct {
	account store.Account
	ok      bool
	// via names the mechanism, in the caller's words.
	via string
}

// attributeLogin answers "which managed account is Claude Code actually using".
//
// It models Claude Code's two competing axes in the order Claude Code resolves
// them, which is documented rule by rule in identity/apikey.go:
//
//   - An API key from the ENVIRONMENT, a file descriptor or an apiKeyHelper
//     turns the OAuth path off entirely (`BE()`), so it is asked first. The
//     credentials file must not even be consulted then: reporting its account
//     would name one Claude Code is not using.
//   - Otherwise the OAuth axis answers, in `ua()`'s own order —
//     CLAUDE_CODE_OAUTH_TOKEN first, the credentials file second.
//   - Only when the OAuth axis has nothing does a STORED primaryApiKey become
//     the credential. It is Claude Code's lowest-priority source, and treating
//     it as the answer while a login exists is the single mistake this whole
//     model exists to avoid.
func attributeLogin(live cclink.Blob, accounts []store.Account,
	lookup func(uuid string) (cclink.Blob, error), env identity.APIKeyEnvironment) attribution {

	key, source := env.Resolve()

	if source.DisplacesOAuth() {
		if key == "" {
			// A file descriptor or a helper: something WILL resolve and it will
			// displace the login, but reading it means reading another
			// process's descriptor or running a user's command. Neither belongs
			// in a read-only question, so the honest answer is the mechanism
			// without the account.
			return attribution{via: source.String()}
		}
		if acct, ok := apiKeyOwner(accounts, lookup, key); ok {
			return attribution{account: acct, ok: true, via: source.String()}
		}
		return attribution{via: source.String()}
	}

	if envToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); envToken != "" {
		for _, a := range accounts {
			stored, err := lookup(a.UUID)
			if err != nil {
				continue
			}
			if rec, ok := tokenRecordOf(stored); ok && rec.Token == envToken {
				return attribution{account: a, ok: true, via: "CLAUDE_CODE_OAUTH_TOKEN"}
			}
		}
		return attribution{via: "CLAUDE_CODE_OAUTH_TOKEN"}
	}

	if _, hasOAuth := live["claudeAiOauth"]; hasOAuth {
		acct, ok := attributeWith(live, accounts, lookup)
		return attribution{account: acct, ok: ok, via: "the Claude Code credentials file"}
	}

	// No login anywhere, so Claude Code falls through to its stored key.
	if source == identity.APIKeyManaged {
		if acct, ok := apiKeyOwner(accounts, lookup, key); ok {
			return attribution{account: acct, ok: true, via: source.String()}
		}
		return attribution{via: source.String()}
	}
	return attribution{via: "none"}
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
			// A config that cannot be read is not a reason to refuse the whole
			// question: it costs the two api-key inputs, and the environment
			// axes — which are the ones that OVERRIDE a login — still answer.
			cfg, cfgErr := cclink.LoadGlobalConfig()
			if cfgErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: could not read Claude Code's config (%v); a stored API key cannot be seen from here\n", cfgErr)
				cfg = nil
			}
			env := claudeAPIKeyEnvironment(cfg)
			res := attributeLogin(live, s.Accounts(), s.Credentials, env)

			if asJSON {
				payload := map[string]any{"schemaVersion": 1, "attributed": res.ok, "via": res.via}
				if res.ok {
					payload["account"] = accountJSON(res.account)
				}
				if env.EnvKeyNeedsApproval() {
					payload["envKeyNeedsApproval"] = true
				}
				if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
					payload["unknownKeys"] = unknown
				}
				if err := writeJSON(cmd, payload); err != nil {
					return err
				}
				if !res.ok {
					// The exit code is the same with or without --json: the flag
					// changes the representation, never the answer.
					return WithCode(errSilent, ExitProbeNegative)
				}
				return nil
			}

			noteEnvKeyApproval(cmd, env)
			if !res.ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "The current credential (%s) is not one ccdad manages.\n", res.via)
				// A negative probe answer, not a failure: exit 5 so a script can
				// tell it from a real error.
				return WithCode(errSilent, ExitProbeNegative)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.account.Label())
			fmt.Fprintf(cmd.ErrOrStderr(), "via %s\n", res.via)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}
