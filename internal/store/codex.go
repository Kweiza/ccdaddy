package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// codexBlobKey is the top-level key a Codex account's credential file holds,
// and codexRefreshTokenHashOf is how the needs-relogin mark is computed from
// it. Both are spelled here rather than imported, and that is a cycle rather
// than a duplication: the Codex refresher re-reads and saves through this
// package's lock, so the credential package imports the store and the store
// can never import it back. A test in that package's EXTERNAL test package --
// which is outside the cycle -- asserts the two spellings agree by writing a
// mark with one and reading it with the other.
const codexBlobKey = "codexOAuth"

// codexRefreshTokenHashOf names the refresh token in a stored blob without
// being one. The mark lives in accounts.toml, which is plaintext at 0600 and
// is the file people paste into an issue.
//
// A blob with no Codex record, or one whose record has no refresh token,
// answers the empty string. That is not an error: it is an account there is no
// grant to mark, and equality against an empty hash is what makes
// Account.NeedsRelogin answer false for it.
func codexRefreshTokenHashOf(b cclink.Blob) (string, error) {
	raw, ok := b[codexBlobKey]
	if !ok {
		return "", nil
	}
	var rec struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", fmt.Errorf("the stored %s record cannot be read: %w", codexBlobKey, err)
	}
	if rec.RefreshToken == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(rec.RefreshToken))
	return hex.EncodeToString(sum[:])[:16], nil
}

// SetCredentials replaces one account's credential file, leaving its row alone.
//
// Add cannot stand in for it. Add is the re-authentication path: it re-derives
// the index, the partition side and the per-account flags, and it refuses a
// provider change. A token rotation changes the credential file and nothing
// else, and going through Add would make every rotation run that whole path.
//
// It refuses an account the document does not name, because the alternative is
// a credential file with no row pointing at it -- exactly the orphan
// OrphanCredentialsAt exists to report.
func (s *Store) SetCredentials(uuid string, creds cclink.Blob) error {
	return s.mutate(func() error {
		if _, ok := s.Get(uuid); !ok {
			return fmt.Errorf("%q is not an account this store names", uuid)
		}
		return s.writeCredentials(uuid, creds)
	})
}

// SetCodexReloginFor records that this account's grant was refused for good,
// but only while the account still holds the refused token.
//
// expectedHash is the hash of the token whose refresh was rejected, as the
// caller saw it. The comparison is against what the store holds NOW, inside
// the lock, and that is the whole point of the method: without it, a terminal
// refusal for token A racing a `ccdad add codex` that stored token B would
// mark an account holding a perfectly good grant as needing a relogin.
//
// A miss is not an error. It means the store moved past the rejected token
// while the request was in flight, which is the ordinary outcome of a race the
// user won -- so it writes nothing and reports success.
func (s *Store) SetCodexReloginFor(uuid, expectedHash, mark string) error {
	return s.mutate(func() error {
		if _, ok := s.Get(uuid); !ok {
			return fmt.Errorf("%q is not an account this store names", uuid)
		}
		blob, err := s.Credentials(uuid)
		if err != nil {
			return err
		}
		current, err := codexRefreshTokenHashOf(blob)
		if err != nil {
			return err
		}
		if current == "" || current != expectedHash {
			return nil
		}
		for i := range s.data.Accounts {
			if s.data.Accounts[i].UUID == uuid {
				s.data.Accounts[i].CodexReloginFor = mark
				return nil
			}
		}
		return nil
	})
}

// CodexAccounts and ClaudeAccounts are where every provider-specific path
// starts.
//
// They are methods rather than a predicate each caller writes, because a
// caller that wrote its own is a caller that could forget -- and the cost of
// forgetting is asymmetric: a Codex row that reaches the Claude ranking is a
// switch that rewrites Claude Code's credentials file for an account that has
// no Claude login.
func (s *Store) CodexAccounts() []Account { return s.accountsOf(provider.Codex) }

// ClaudeAccounts is CodexAccounts' twin. See it.
func (s *Store) ClaudeAccounts() []Account { return s.accountsOf(provider.Claude) }

func (s *Store) accountsOf(p provider.ID) []Account {
	out := make([]Account, 0, len(s.data.Accounts))
	for _, a := range s.data.Accounts {
		if a.Provider == p {
			out = append(out, a)
		}
	}
	return out
}
