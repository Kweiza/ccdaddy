package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"errors"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// maxImportSize caps how much of an import file is READ. It is a document
// someone hands this process, so its length is not ours to trust; the cap is
// generous enough for a store with hundreds of accounts.
//
// It counts the bytes on disk rather than the JSON they carry, so a base64
// document's effective ceiling is three quarters of this. That is deliberate:
// the cap exists to bound what an untrusted file can make this process
// allocate, and the encoded form is what it allocates.
const maxImportSize = 16 << 20

func newImportCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "import <PATH>",
		Short: "Load accounts from a document written by 'ccdad export'",
		Long: "Load accounts from a document written by 'ccdad export'. PATH may be '-' to\n" +
			"read stdin, and may hold either the JSON document or one line of the base64\n" +
			"'ccdad export --base64' writes — the form is detected, not declared.\n\n" +
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
			prov  provider.ID
		}
		var batch []staged

		for _, row := range payload.Accounts {
			// idx is deliberately not carried: an account already here keeps
			// the position it has, and a new one lands at the end. Imposing the
			// exported order on a store that already has one would renumber
			// accounts the import never mentioned.
			_, known := existing[row.UUID]

			// Derived before anything is staged, so a row this build cannot
			// classify costs nothing. The document's own blob is read rather
			// than the filtered snapshot, so the derivation cannot depend on
			// what the filter happens to keep.
			prov, perr := importProviderOf(row, row.Credentials)
			if perr != nil {
				skipped = append(skipped, fmt.Sprintf("%s (%v)", row.label(), perr))
				continue
			}

			creds := importSnapshot(row.Credentials)
			switch {
			case len(creds) > 0:
				if known && !force && localCredentialIsNewer(s, row.UUID, creds) {
					skipped = append(skipped, fmt.Sprintf(
						"%s (the credentials here are newer; --force to overwrite them)", row.label()))
					continue
				}
				if known {
					stored, cerr := s.Credentials(row.UUID)
					switch {
					case errors.Is(cerr, store.ErrNoCredentials):
						// Nothing here to lose, and this import is the repair
						// for an account row whose credential file went missing.
					case cerr != nil:
						// "could not read" is not "there is nothing there". The
						// ccdad store is named because it is the one that
						// answered -- the document is fine -- and the row is
						// left alone rather than overwritten blind.
						skipped = append(skipped, fmt.Sprintf(
							"%s (the ccdad store could not read its stored credentials: %v; "+
								"'ccdad remove' it, then import again to replace them)", row.label(), cerr))
						continue
					default:
						// INVARIANT 5. store.Add replaces the credential file
						// WHOLESALE -- `ccdad add-token` carries the same
						// warning for the same reason -- so handing it the
						// document's blob alone deletes this account's api-key
						// record, its designOauth and its trustedDeviceToken as
						// the price of restoring one OAuth login. The
						// newer-credentials check above cannot stand in for it:
						// that compares claudeAiOauth and nothing else, so it
						// never sees the keys being dropped.
						//
						// The document still wins on every key it CARRIES, so
						// --force keeps meaning "the document's generation of
						// claudeAiOauth beats the newer local one" rather than
						// "erase what the document never mentioned".
						//
						// Filtered through importSnapshot for the same reason
						// the document is: a machine key that once reached this
						// account's snapshot must not be re-blessed here, by a
						// path with no rule on it.
						for k, v := range importSnapshot(stored) {
							if _, carried := creds[k]; !carried {
								creds[k] = v
							}
						}
					}
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
			batch = append(batch, staged{row: row, creds: creds, prov: prov})
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
				Provider:         item.prov,
				Tier:             item.row.Tier,
				RateLimitTier:    item.row.RateLimitTier,
				SeatTier:         item.row.SeatTier,
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

	document, wasBase64, err := decodeExportDocument(raw)
	if err != nil {
		return exportPayload{}, err
	}

	var payload exportPayload
	if err := json.Unmarshal(document, &payload); err != nil {
		if wasBase64 {
			// Which half failed is the whole message here. The file on disk
			// contains none of the bytes this parser is complaining about, so
			// "not JSON" on its own points the reader at a file they can open
			// and see nothing wrong with.
			return exportPayload{}, UsageError(
				"that file is not a ccdad export: it decoded from base64, but what came out is not JSON: %v", err)
		}
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

// decodeExportDocument returns the JSON inside raw — which is either the
// document itself or one line of base64 holding it — and says which it was.
//
// The two forms cannot be confused, so this sniffs rather than trying one and
// falling back to the other: a ccdad export is a JSON OBJECT, so the plain form
// always begins with '{', and '{' is in neither base64 alphabet. One byte
// decides it. A fallback would instead report whichever attempt happened to
// fail last, which for a corrupt JSON document is a complaint about base64.
func decodeExportDocument(raw []byte) (document []byte, wasBase64 bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0:
		// Answered here rather than left to the parser, whose "unexpected end
		// of JSON input" is a description of a document that is not there.
		return nil, false, UsageError("that file is empty, and no ccdad export is")
	case trimmed[0] == '{':
		return raw, false, nil
	}
	decoded, err := decodeBase64Document(trimmed)
	if err != nil {
		// Both forms are named, because "not JSON" alone sends the reader
		// looking for a JSON mistake in a file that was never meant to hold
		// any. base64's own error names an input OFFSET and never a byte out
		// of the document, which is what makes it safe to repeat.
		return nil, true, UsageError(
			"that file is not a ccdad export: it does not begin with '{', and it is not base64 either (%v)", err)
	}
	return decoded, true, nil
}

// isExportDocument reports whether s is an export DOCUMENT rather than a path
// to one.
//
// It is readExport's own sniff plus one more requirement: what comes out has to
// be a JSON OBJECT, which every ccdad export is and which a path that happens
// to be spelled entirely in base64 characters is not. Without that requirement
// a path decoding to the bytes "123" would answer yes, because a bare number is
// valid JSON.
//
// `ccdad bootstrap` is the caller, and it asks because its variable holds a
// path while its output is a container log — see the comment there.
func isExportDocument(s string) bool {
	document, _, err := decodeExportDocument([]byte(s))
	if err != nil {
		return false
	}
	document = bytes.TrimSpace(document)
	return len(document) > 0 && document[0] == '{' && json.Valid(document)
}

// decodeBase64Document decodes one base64 document in whatever shape the tool
// that wrote it produced.
//
// It is deliberately permissive, and each concession is here because the
// refusal it replaces would land somewhere expensive:
//
//   - EVERY whitespace byte is dropped, not only the trailing one. `base64`
//     wraps at 76 columns unless it is given -w0, and the machine that finds
//     out is the one running the deployment, not the one that wrote the file.
//     Whitespace is not data in base64, so dropping it loses nothing.
//   - BOTH alphabets are read. '-' and '_' are the url-safe alphabet's two
//     characters and appear in no standard-alphabet encoding, so their
//     presence is the answer — again a decision rather than a second attempt.
//   - PADDING is stripped and the Raw encodings do the work, so a padded
//     document and an unpadded one take one path instead of two.
//
// Being permissive costs nothing downstream: what comes out still has to parse
// as JSON and still has to carry a schemaVersion before it is an export.
func decodeBase64Document(b []byte) ([]byte, error) {
	compact := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n', '\v', '\f':
		default:
			compact = append(compact, c)
		}
	}
	enc := base64.RawStdEncoding
	if bytes.ContainsAny(compact, "-_") {
		enc = base64.RawURLEncoding
	}
	return enc.DecodeString(string(bytes.TrimRight(compact, "=")))
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

// importProviderOf decides which provider a document's row describes.
//
// Three answers, in this order.
//
// An EXPLICIT provider wins, and one this build does not know is an error
// rather than a guess: the document was written by something that knows more
// than this binary, the note at the top of the command already says what that
// means, and the caller turns this into a skipped row that names itself. The
// wrong guess is the expensive one -- an unknown provider imported as Claude
// is an account the switch would try to make Claude Code's login.
//
// An ABSENT provider with a Codex record in the blob is a Codex account. That
// key reaches an export from exactly one place, so it is evidence rather than
// a heuristic.
//
// An absent provider with anything else is Claude, which is what every
// document written before the field existed holds.
func importProviderOf(row exportAccount, creds cclink.Blob) (provider.ID, error) {
	if row.Provider != "" {
		return provider.Parse(row.Provider)
	}
	if _, hasCodex := creds[codexauth.Key]; hasCodex {
		return provider.Codex, nil
	}
	return provider.Claude, nil
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
	// The Codex record is the second key added back by name, and for the same
	// reason as ccdadToken: cclink's list mirrors the keys that travel with a
	// CLAUDE login, and a name Claude Code has never heard of does not belong
	// in it. Dropping it here would import every Codex account with no
	// credential at all -- a row in the store and nothing to serve it with.
	if raw, ok := b[codexauth.Key]; ok {
		out[codexauth.Key] = append(json.RawMessage(nil), raw...)
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
	// A Codex record has no claudeAiOauth.expiresAt, so the comparison below
	// finds nothing to compare and every Codex pair answers "not newer" --
	// which silently overwrites a live Codex login with a stale one on every
	// import. The ACCESS TOKEN's own exp is the claim compared instead.
	if localAt, localOK := codexAccessExpiry(stored); localOK {
		remoteAt, remoteOK := codexAccessExpiry(incoming)
		if !remoteOK {
			return false
		}
		return localAt.After(remoteAt)
	}
	local, localOK := oauthExpiresAt(stored)
	remote, remoteOK := oauthExpiresAt(incoming)
	if !localOK || !remoteOK {
		return false
	}
	return local > remote
}

// codexAccessExpiry reads the access token's own exp claim out of the Codex
// record.
//
// The token endpoint mints a new access token on every refresh and its exp
// moves forward with it, so a later exp is a later credential. The id_token's
// exp is never read: it is an hour from login and says nothing about the grant.
// No signature check, on purpose -- this compares two records ccdad wrote
// itself, and a verification here would be a second answer to a question the
// proxy already asks of the endpoint.
//
// An absent or unreadable record answers false, which lets the import proceed:
// the same fail-open localCredentialIsNewer applies to every uncomparable pair.
func codexAccessExpiry(b cclink.Blob) (time.Time, bool) {
	cred, ok, err := codexauth.FromBlob(b)
	if err != nil || !ok {
		return time.Time{}, false
	}
	parts := strings.Split(cred.AccessToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
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
