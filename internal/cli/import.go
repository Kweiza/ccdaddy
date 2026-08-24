package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// maxImportSize caps how much of an import file is read. It is a document
// someone hands this process, so its length is not ours to trust; the cap is
// generous enough for a store with hundreds of accounts.
const maxImportSize = 16 << 20

func newImportCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "import <PATH>",
		Short: "Load accounts from a document written by 'ccdad export'",
		Long: "Load accounts from a document written by 'ccdad export'. PATH may be '-' to\n" +
			"read stdin.\n\n" +
			"uuid is the key, so an account already here is updated to match the document\n" +
			"rather than duplicated — an alias the document does not carry is cleared — and\n" +
			"the display order is re-derived, since an imported idx would be a stale ordinal.\n\n" +
			"An account whose LOCAL credentials are newer than the exported ones is skipped:\n" +
			"restoring a stale refresh token turns a working account into a quarantined one.\n" +
			"--force imports it anyway.\n\n" +
			"MCP logins in the document are never installed. They are machine-scoped\n" +
			"secrets, and writing another machine's into this one's credentials file is the\n" +
			"thing the export rule exists to prevent.",
		Args:          usageArgs(cobra.ExactArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := readExport(cmd, args[0])
			if err != nil {
				return err
			}
			if payload.SchemaVersion > exportSchemaVersion {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: this export was written by a newer ccdad (schema %d); anything this build does not recognize is ignored\n",
					payload.SchemaVersion)
			}
			if len(payload.Accounts) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "That export holds no accounts.")
				return WithCode(errSilent, ExitNothingToDo)
			}
			if payload.Machine != nil {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: this export carries MCP logins. They are not being installed — "+
						"they belong to the machine they were taken from, not to any account here.")
			}
			if len(payload.UnknownKeys) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: the machine this was exported from had unrecognized credential keys: %s\n",
					strings.Join(payload.UnknownKeys, ", "))
			}

			// Everything that can be judged from the document alone is judged
			// before anything is written, and that is still worth doing now
			// that the store reverses a batch it could not finish: a refusal
			// here names the row and the rule, while a reversal can only say
			// that the machine failed. The store's rollback is the recovery
			// for what CANNOT be judged from the document — an I/O failure on
			// the fourth of five credential writes — and not a reason to stop
			// judging the document.
			if err := validateExport(payload); err != nil {
				return UsageError("%s", err.Error())
			}

			imported, skipped, err := applyImport(payload, force)
			if err != nil {
				return err
			}

			for _, note := range skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "Skipped %s\n", note)
			}
			if len(imported) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "Nothing was imported.")
				return WithCode(errSilent, ExitNothingToDo)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Imported %d account(s): %s\n", len(imported), strings.Join(imported, ", "))
			if !payload.Full {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: that export carried no credentials, so only account details were updated.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite credentials that are newer here than in the export")
	return cmd
}

// applyImport is the transaction: one store mutation that adds or updates every
// account the document carries.
//
// It is a function rather than the body of a RunE because `ccdad bootstrap`
// applies the same document out of CCDAD_IMPORT, and three rules here are
// exactly the ones that go silently wrong when they are written a second time —
// local credentials newer than the document's are kept unless force says
// otherwise, the aliases are cleared across the WHOLE batch before any of them
// is set, and the two per-account flags have to be applied AFTER the add.
//
// The two slices are LABELS: an alias, an email address, or a uuid prefix, taken
// out of the document. `ccdad import` prints them because a person named this
// file on the command line. `ccdad bootstrap` counts them instead, because its
// output is a container log.
func applyImport(payload exportPayload, force bool) (imported, skipped []string, err error) {
	err = store.WithStore(func(s *store.Store) error {
		existing := map[string]store.Account{}
		for _, a := range s.Accounts() {
			existing[a.UUID] = a
		}
		type staged struct {
			row   exportAccount
			creds cclink.Blob
		}
		var batch []staged

		for _, row := range payload.Accounts {
			// idx is deliberately not carried: an account already here keeps
			// the position it has, and a new one lands at the end. Imposing the
			// exported order on a store that already has one would renumber
			// accounts the import never mentioned.
			_, known := existing[row.UUID]

			creds := importSnapshot(row.Credentials)
			switch {
			case len(creds) > 0:
				if known && !force && localCredentialIsNewer(s, row.UUID, creds) {
					skipped = append(skipped, fmt.Sprintf(
						"%s (the credentials here are newer; --force to overwrite them)", row.label()))
					continue
				}
			case known:
				// A metadata-only export can still carry an alias, the two
				// per-account flags and a tier. Keep what is already stored
				// rather than blanking the account's login.
				stored, err := s.Credentials(row.UUID)
				if err != nil {
					skipped = append(skipped, fmt.Sprintf("%s (%v)", row.label(), err))
					continue
				}
				creds = stored
			default:
				skipped = append(skipped, fmt.Sprintf(
					"%s (this export carries no credentials, and there is nothing here to attach them to)",
					row.label()))
				continue
			}
			batch = append(batch, staged{row: row, creds: creds})
		}

		// Collisions are judged AFTER staging, against the rows that will
		// actually be applied, and the order is the whole correctness of the
		// check. It clears from its watch list every local account the batch
		// covers, because the first apply pass below blanks their aliases — and
		// a row that was SKIPPED just above never reaches that pass, so its
		// alias is still held. Judging the whole document instead would exempt
		// the skipped account's alias and let a later row collide with it
		// mid-batch, after that row's credential file is already on disk.
		//
		// Nothing has been written yet: the staging loop only reads.
		rows := make([]exportAccount, 0, len(batch))
		for _, item := range batch {
			rows = append(rows, item.row)
		}
		if err := checkAliasCollisions(rows, existing); err != nil {
			return UsageError("%s", err.Error())
		}

		// The aliases are cleared across the WHOLE batch before any of them is
		// set, and the two passes are not tidiness. An import that hands account
		// B the alias account A currently holds is legitimate — the document is
		// the answer — but applied account-by-account it fails or succeeds
		// depending on which of the two the array happens to list first.
		for _, item := range batch {
			acct := store.Account{
				UUID:             item.row.UUID,
				Email:            item.row.Email,
				Kind:             identity.ParseKind(item.row.Kind),
				Tier:             item.row.Tier,
				RateLimitTier:    item.row.RateLimitTier,
				OrganizationUUID: item.row.OrganizationUUID,
				Disabled:         item.row.Disabled,
				Primary:          item.row.Primary,
				AddedAt:          item.row.AddedAt,
			}
			// AddedAt is carried, and store.add keeps the STORED one when the
			// uuid is already here — so a re-run of the same document cannot
			// move an account's age, and a first import keeps the stamp the
			// machine that wrote the document recorded.
			if err := s.Add(acct, item.creds); err != nil {
				return err
			}
			// Add preserves the STORED alias and the two per-account flags over
			// incoming ones — that is what makes it double as
			// re-authentication — so all three have to be applied after it, or
			// an import could never change any of them. These three calls are
			// the deciding write for an account already here AND for one that
			// is not: Add appends the new record and they then set its flags on
			// it, so the literal above only gives that record its first shape.
			if err := s.SetAlias(item.row.UUID, ""); err != nil {
				return err
			}
			if _, err := s.SetDisabled(item.row.UUID, item.row.Disabled); err != nil {
				return err
			}
			if _, err := s.SetPrimary(item.row.UUID, item.row.Primary); err != nil {
				return err
			}
		}
		for _, item := range batch {
			if item.row.Alias != "" {
				if err := s.SetAlias(item.row.UUID, item.row.Alias); err != nil {
					// A BACKSTOP, and unreachable while the check above is
					// right: validateExport has already refused a malformed
					// alias and a document that gives one alias twice, the
					// pass above blanked every alias this batch owns, and the
					// collision check covered every alias it does not. No
					// test reaches this line, and that is the disclosure.
					//
					// It is here rather than `return err` because the failure
					// it would report quotes the alias out of the document,
					// and `ccdad bootstrap` decides whether a message may be
					// repeated into a container log by asking whether it is a
					// usage error. A regression in the check above would
					// otherwise reopen that hole silently.
					return UsageError("%s", err.Error())
				}
			}
			imported = append(imported, item.row.label())
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return imported, skipped, nil
}

func (a exportAccount) label() string {
	switch {
	case a.Alias != "":
		return a.Alias
	case a.Email != "":
		return a.Email
	case len(a.UUID) > 8:
		return a.UUID[:8]
	}
	return a.UUID
}

// readExport reads and decodes an export document. "-" is stdin, so a backup
// kept encrypted can be piped straight in without ever landing on disk.
func readExport(cmd *cobra.Command, path string) (exportPayload, error) {
	var src io.Reader
	if path == "-" {
		src = cmd.InOrStdin()
	} else {
		f, err := os.Open(path)
		if err != nil {
			return exportPayload{}, fmt.Errorf("reading the export: %w", err)
		}
		defer f.Close()
		src = f
	}

	raw, err := io.ReadAll(io.LimitReader(src, maxImportSize+1))
	if err != nil {
		return exportPayload{}, fmt.Errorf("reading the export: %w", err)
	}
	if len(raw) > maxImportSize {
		return exportPayload{}, fmt.Errorf("that file is larger than %d bytes, which no ccdad export is", maxImportSize)
	}

	var payload exportPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return exportPayload{}, UsageError("that file is not a ccdad export: %v", err)
	}
	// Zero means the field was absent, which is what every JSON document that
	// is not a ccdad export has. A HIGHER version is accepted: the `--json`
	// contract is additive, so a newer export's extra fields are ignored rather
	// than refused.
	//
	// Saying so out loud is the CALLER's, and SchemaVersion is on the payload
	// for it to read. `ccdad import` names the number to a person who typed the
	// path; `ccdad bootstrap` says the same fact without it, because the number
	// comes out of a document it must not describe into a container log. A note
	// printed from here would reach both.
	if payload.SchemaVersion < 1 {
		return exportPayload{}, UsageError("that file is not a ccdad export: it carries no schemaVersion")
	}
	return payload, nil
}

// validateExport judges everything that can be judged from the document alone,
// so the batch is refused before the first credential file is written.
func validateExport(payload exportPayload) error {
	seen := map[string]bool{}
	aliases := map[string]string{}
	for _, row := range payload.Accounts {
		if err := store.ValidateUUID(row.UUID); err != nil {
			return fmt.Errorf("that export is not usable: %w", err)
		}
		if seen[row.UUID] {
			return fmt.Errorf("that export names %s twice; uuid is the key, so it cannot appear more than once", row.UUID)
		}
		seen[row.UUID] = true

		if row.Alias == "" {
			continue
		}
		normalized := store.NormalizeAlias(row.Alias)
		if err := store.ValidateAlias(normalized); err != nil {
			return fmt.Errorf("that export is not usable: %w", err)
		}
		if other, taken := aliases[normalized]; taken {
			return fmt.Errorf("that export gives the alias %q to both %s and %s", normalized, other, row.UUID)
		}
		aliases[normalized] = row.UUID
	}
	return nil
}

// checkAliasCollisions catches the aliases that only collide once the local
// store is in front of us: one held by an account this batch does not cover.
// SetAlias would refuse it mid-batch, and the batch has already begun by then.
//
// rows is the batch that will actually be applied, NOT the whole document. An
// account whose alias this batch is about to blank cannot collide with it, and
// one the batch leaves alone still holds it — so passing rows the caller has
// decided to skip would exempt an alias that is still taken.
func checkAliasCollisions(rows []exportAccount, existing map[string]store.Account) error {
	incoming := map[string]bool{}
	for _, row := range rows {
		incoming[row.UUID] = true
	}
	held := map[string]store.Account{}
	for uuid, a := range existing {
		if a.Alias != "" && !incoming[uuid] {
			held[store.NormalizeAlias(a.Alias)] = a
		}
	}
	for _, row := range rows {
		if row.Alias == "" {
			continue
		}
		normalized := store.NormalizeAlias(row.Alias)
		if other, taken := held[normalized]; taken {
			return fmt.Errorf("%s: %q already belongs to %s (%s), which this import is not applying",
				store.ErrAliasTaken, normalized, other.Label(), other.UUID)
		}
	}
	return nil
}

// importSnapshot filters an imported blob down to what may be stored for ONE
// account.
//
// `ccdad import` never writes mcpOAuth into the live credentials file, and
// enforcing that only there would be too late: a machine key that reached a
// per-account snapshot would be merged into the live file by the next ordinary
// `ccdad switch`, through a path with no rule on it. So the filter runs here,
// one level earlier, at the boundary where the document becomes ccdad's data.
//
// cclink.Extract is the filter rather than a key list written out again — it is
// the same deny-list Merge and Capture use, so a key Anthropic adds cannot be
// account-scoped in one place and machine-scoped in another.
//
// ccdadToken is added back by name, and it is the one exception. It is ccdad's
// OWN record for an API key or setup token — a credential Claude Code never
// reads out of the credentials file — so it is not in cclink's list and must
// not be: that list mirrors Claude Code's own prune, and a name Claude Code has
// never heard of does not belong in it. Dropping it here would silently discard
// every `ccdad add-token` account on import.
func importSnapshot(b cclink.Blob) cclink.Blob {
	if len(b) == 0 {
		return nil
	}
	out := cclink.Extract(b)
	if raw, ok := b[cclink.TokenKey]; ok {
		out[cclink.TokenKey] = append(json.RawMessage(nil), raw...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// localCredentialIsNewer reports whether what is already stored for this
// account outlives what the export carries.
//
// The comparison is claudeAiOauth.expiresAt, in milliseconds, which is what
// both sides were written with. A pair that cannot be compared — a token
// account, a snapshot with no expiry, an unparseable record — answers false and
// lets the import proceed: the user asked for this document, and the worst case
// is an account that needs `ccdad add` again, whereas refusing every
// uncomparable pair would make restoring a backup need --force as a matter of
// routine.
func localCredentialIsNewer(s *store.Store, uuid string, incoming cclink.Blob) bool {
	stored, err := s.Credentials(uuid)
	if err != nil {
		return false
	}
	local, localOK := oauthExpiresAt(stored)
	remote, remoteOK := oauthExpiresAt(incoming)
	if !localOK || !remoteOK {
		return false
	}
	return local > remote
}

// oauthExpiresAt reads claudeAiOauth.expiresAt. It is in MILLISECONDS — the
// credential writer computes it as `now + expires_in*1000` — and is only ever
// compared against another value read the same way here.
func oauthExpiresAt(b cclink.Blob) (int64, bool) {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return 0, false
	}
	var payload struct {
		ExpiresAt *int64 `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.ExpiresAt == nil {
		return 0, false
	}
	return *payload.ExpiresAt, true
}
