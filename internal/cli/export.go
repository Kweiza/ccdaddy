package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// exportSchemaVersion is the payload's contract version. The contract is
// additive per §9.4: a reader must ignore fields and accounts it does not
// recognize rather than refusing the document.
const exportSchemaVersion = 1

// exportPayload is what `ccdad export` writes and `ccdad import` reads.
//
// §5.1: uuid, email and alias go in and idx does NOT. idx recompacts on every
// removal, so an export carrying it would reproduce a stale ordinal on the
// machine it was imported into — and the two machines would then disagree about
// what `ccdad switch 2` means. Order is carried implicitly by the array, which
// is the part of idx that is actually portable.
type exportPayload struct {
	SchemaVersion int             `json:"schemaVersion"`
	ExportedAt    time.Time       `json:"exportedAt"`
	Accounts      []exportAccount `json:"accounts"`

	// Full says whether the credential snapshots are present. It is recorded
	// rather than inferred so `import` can tell "this backup carries no
	// credentials" from "this account happened to have none".
	Full bool `json:"full"`

	// Machine carries the MCP logins, and only when --include-mcp asked for
	// them. It is not per-account: these belong to the machine.
	Machine *exportMachine `json:"machine,omitempty"`

	// UnknownKeys is §4.3's probe, surfaced in the export itself and not only
	// on stderr. Six machine keys drifted into the credentials file after
	// clauth's carry list was written, so an export taken by a build that did
	// not recognize a key should say so in the artifact that outlives it.
	UnknownKeys []string `json:"unknownKeys,omitempty"`
}

type exportAccount struct {
	UUID             string    `json:"uuid"`
	Email            string    `json:"email,omitempty"`
	Alias            string    `json:"alias,omitempty"`
	Kind             string    `json:"kind"`
	Tier             string    `json:"tier,omitempty"`
	RateLimitTier    string    `json:"rateLimitTier,omitempty"`
	OrganizationUUID string    `json:"organizationUuid,omitempty"`
	Disabled         bool      `json:"disabled,omitempty"`
	AddedAt          time.Time `json:"addedAt"`

	// Credentials is the account's stored snapshot, present only with --full.
	// Without it the export carries no token of any kind, which is what makes
	// the DEFAULT safe to mail to yourself.
	Credentials cclink.Blob `json:"credentials,omitempty"`
}

// exportMachine is the machine-scoped half, and the only thing here that is not
// this ccdad store's own data.
//
// Both halves travel together or neither does: §4.1 records that
// mcpOAuthClientConfig is the client-secret half of mcpOAuth and that Claude
// Code reads them under the same composite key, so carrying only the token half
// produces MCP logins that cannot refresh — a backup that looks complete and is
// not.
type exportMachine struct {
	MCPOAuth             json.RawMessage `json:"mcpOAuth,omitempty"`
	MCPOAuthClientConfig json.RawMessage `json:"mcpOAuthClientConfig,omitempty"`
}

func newExportCmd() *cobra.Command {
	var (
		full       bool
		includeMCP bool
		out        string
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write the account store to a portable JSON document",
		Long: "Write the account store to a portable JSON document.\n\n" +
			"By default the payload carries no credentials at all: it describes which\n" +
			"accounts exist, not how to log in as them. --full adds each account's stored\n" +
			"credential snapshot, refresh tokens included, which is what makes it a real\n" +
			"backup and a real secret — it must go to a file, not to a terminal.\n\n" +
			"--include-mcp additionally carries this machine's MCP server logins. It\n" +
			"requires --full and is the only path by which they leave the machine.\n" +
			"'ccdad import' never installs them; restoring MCP logins is deliberately a\n" +
			"manual act.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// All three of §9.1's conditions on --include-mcp are enforced
			// together, and the flag alone is a usage error rather than a
			// silent upgrade to --full: the difference between the two payloads
			// is every MCP client secret on the machine, which is not something
			// to infer from a flag the user did not pass.
			if includeMCP && !full {
				return UsageError("--include-mcp carries this machine's MCP logins, so it needs --full as well")
			}
			if full && out == "" && stdoutIsTTY() {
				return UsageError("a --full export holds live refresh tokens; write it to a file with --out PATH, " +
					"or redirect stdout somewhere that is not a terminal")
			}

			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()

			payload := exportPayload{
				SchemaVersion: exportSchemaVersion,
				ExportedAt:    time.Now().UTC(),
				Full:          full,
				Accounts:      make([]exportAccount, 0, len(accounts)),
			}
			for _, a := range accounts {
				row := exportAccount{
					UUID:             a.UUID,
					Email:            a.Email,
					Alias:            a.Alias,
					Kind:             a.Kind.String(),
					Tier:             a.Tier,
					RateLimitTier:    a.RateLimitTier,
					OrganizationUUID: a.OrganizationUUID,
					Disabled:         a.Disabled,
					AddedAt:          a.AddedAt,
				}
				if full {
					creds, err := s.Credentials(a.UUID)
					if err != nil {
						// One account with no readable snapshot must not cost
						// the other nine their backup. Say which one, and carry
						// its metadata anyway.
						fmt.Fprintf(cmd.ErrOrStderr(),
							"warning: %s is exported without credentials (%v)\n", a.Label(), err)
					} else {
						row.Credentials = creds
					}
				}
				payload.Accounts = append(payload.Accounts, row)
			}

			// The live file is read for the drift probe, and for the MCP keys
			// when they were asked for. A file that cannot be read is not a
			// reason to refuse the export — the accounts are the payload.
			live, liveErr := cclink.Load()
			if liveErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: the live credentials file could not be read (%v); "+
						"the unknown-key probe is not included\n", liveErr)
			} else {
				payload.UnknownKeys = cclink.UnknownKeys(live)
			}
			if includeMCP {
				payload.Machine = machineKeysOf(live)
				if payload.Machine == nil {
					// "on this machine" is a claim about the machine, and
					// inside a `ccdad run` session it is a claim this command
					// cannot make: the default mode scopes mcpOAuth away with
					// the credentials (§3.3's named cost), so an empty answer
					// in here says nothing about what the live login carries.
					// Someone backing their MCP logins up from inside a
					// session would otherwise be told there were none.
					if session, inSession := currentScopedSession(); inSession {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"note: this shell is inside a `ccdad run` session (%s), which does not carry the machine's "+
								"MCP logins — so there are none to include HERE. Export from a shell outside the session "+
								"to capture them.\n", session.describe())
					} else {
						fmt.Fprintln(cmd.ErrOrStderr(), "note: there are no MCP logins on this machine to include.")
					}
				} else {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"WARNING: this export carries this machine's MCP server logins, client secrets included. "+
							"They are not scoped to any account and are not encrypted here. "+
							"Treat the file as a credential: keep it at 0600, do not commit it, and delete it once restored.")
				}
			}

			encoded, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return fmt.Errorf("encoding the export: %w", err)
			}
			encoded = append(encoded, '\n')

			if out != "" {
				// 0600 through the atomic writer, for the reason --out exists
				// at all: a shell redirect creates the file at the umask —
				// typically 0644 — in a directory that is not 0700.
				if err := cclink.WriteFileAtomic(out, encoded, 0o600); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d account(s) to %s (mode 0600).\n", len(payload.Accounts), out)
				return nil
			}
			_, err = cmd.OutOrStdout().Write(encoded)
			return err
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "include each account's stored credentials")
	cmd.Flags().BoolVar(&includeMCP, "include-mcp", false, "also include this machine's MCP logins (requires --full)")
	cmd.Flags().StringVar(&out, "out", "", "write to PATH at mode 0600 instead of stdout")
	return cmd
}

// machineKeysOf lifts the two MCP halves out of the live credentials file,
// returning nil when neither is there so the payload omits the block entirely
// rather than carrying an empty one.
func machineKeysOf(live cclink.Blob) *exportMachine {
	m := &exportMachine{}
	if v, ok := live["mcpOAuth"]; ok {
		m.MCPOAuth = append(json.RawMessage(nil), v...)
	}
	if v, ok := live["mcpOAuthClientConfig"]; ok {
		m.MCPOAuthClientConfig = append(json.RawMessage(nil), v...)
	}
	if m.MCPOAuth == nil && m.MCPOAuthClientConfig == nil {
		return nil
	}
	return m
}
