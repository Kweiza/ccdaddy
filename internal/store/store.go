package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
)

const (
	accountsFile   = "accounts.toml"
	credentialsDir = "credentials"
)

// file is the on-disk shape of accounts.toml.
type file struct {
	// Version lets a later release migrate rather than guess.
	Version  int       `toml:"version"`
	Accounts []Account `toml:"accounts"`
	// ActiveUUID is the account ccdad last activated. It is a hint for display;
	// `ccdad which` derives the truth from the live credentials file.
	ActiveUUID string `toml:"active_uuid,omitempty"`
}

// Store is ccdad's account database.
//
// A Store is not safe for concurrent use across processes: two ccdad
// invocations that each Open, mutate and Save can lose an account, because the
// read-modify-write cycle spans the atomic write rather than being covered by
// it. A store-level lock arrives with the daemon.
type Store struct {
	root string
	data file
}

// reservedDeviceNames are the Windows device names a file cannot be called.
// The check runs on every platform: a store written on Linux is one `rsync`
// away from being opened on Windows, and refusing the name in both places is
// cheaper than having it mean different things.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// validUUID refuses anything that could not be an account uuid. The value
// arrives from the profile endpoint and becomes a path component, so a
// traversal sequence in it would write a credential file outside the store.
func validUUID(uuid string) error {
	if uuid == "" {
		return errors.New("an account needs a uuid")
	}
	if reservedDeviceNames[strings.ToLower(uuid)] {
		return fmt.Errorf("%q is a reserved device name, not a usable account uuid", uuid)
	}
	for _, r := range uuid {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("%q is not a usable account uuid", uuid)
		}
	}
	return nil
}

// Open loads the store, creating an empty one if none exists.
func Open() (*Store, error) {
	root := ccpath.StoreHome()
	// ccpath.homeDir returns "" when os.UserHomeDir fails, which degrades every
	// derived path to a relative one — and a relative store means ccdad creates
	// a credentials tree in whatever directory it happened to be run from, a
	// different one each time, with tokens in it. Refuse instead: the tokens are
	// the whole point of the directory, so the wrong directory is not a
	// degradation worth accepting silently.
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}
	if err := os.MkdirAll(filepath.Join(root, credentialsDir), 0o700); err != nil {
		return nil, fmt.Errorf("creating the ccdad store: %w", err)
	}
	// MkdirAll only sets the mode on directories it CREATES, and a directory
	// restored from a backup or made by an older build can be looser than
	// 0700. These hold live tokens, so tighten both on every open.
	for _, d := range []string{root, filepath.Join(root, credentialsDir)} {
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("securing the ccdad store: %w", err)
		}
	}

	s := &Store{root: root, data: file{Version: 1}}

	raw, err := os.ReadFile(filepath.Join(root, accountsFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", accountsFile, err)
	}
	if err := toml.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", accountsFile, err)
	}
	for i := range s.data.Accounts {
		// An unrecognized name — an older accounts.toml, or a kind a future
		// release adds — resolves to subscription deliberately. Guessing credit
		// would let the account through the credit gate, which spends money;
		// guessing subscription only costs a wasted rotation attempt.
		s.data.Accounts[i].Kind = identity.ParseKind(s.data.Accounts[i].KindName)
	}
	s.sortAndReindex()
	return s, nil
}

// Accounts returns the managed accounts in display order.
func (s *Store) Accounts() []Account {
	out := make([]Account, len(s.data.Accounts))
	copy(out, s.data.Accounts)
	return out
}

// Get looks an account up by uuid.
func (s *Store) Get(uuid string) (Account, bool) {
	for _, a := range s.data.Accounts {
		if a.UUID == uuid {
			return a, true
		}
	}
	return Account{}, false
}

// Add stores an account and its account-scoped credentials.
//
// Re-adding an existing uuid updates it in place: the fresh login replaces the
// stored credentials while the alias and the display index — which belong to
// the user, not to the login — survive. This is what makes `ccdad add` double
// as re-authentication.
func (s *Store) Add(a Account, creds cclink.Blob) error {
	if err := validUUID(a.UUID); err != nil {
		return err
	}
	a.KindName = a.Kind.String()

	existing, isUpdate := s.Get(a.UUID)
	if isUpdate {
		a.Idx = existing.Idx
		a.Alias = existing.Alias
		a.AddedAt = existing.AddedAt
		a.Disabled = existing.Disabled
	} else {
		a.Idx = s.nextIdx()
		if a.AddedAt.IsZero() {
			a.AddedAt = time.Now().UTC()
		}
	}

	// The credential write comes first, and memory is touched only once it has
	// succeeded. The other order leaves an account with no credentials in the
	// in-memory store, which any later Save() would then persist.
	if err := s.writeCredentials(a.UUID, creds); err != nil {
		return err
	}

	if isUpdate {
		for i := range s.data.Accounts {
			if s.data.Accounts[i].UUID == a.UUID {
				s.data.Accounts[i] = a
				break
			}
		}
	} else {
		s.data.Accounts = append(s.data.Accounts, a)
	}
	s.sortAndReindex()
	return s.Save()
}

// Remove deletes an account and its stored credentials.
func (s *Store) Remove(uuid string) error {
	idx := -1
	for i, a := range s.data.Accounts {
		if a.UUID == uuid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, uuid)
	}

	// Delete the credentials before dropping the account, for the mirror image
	// of Add's reason: the other order would drop the account from
	// accounts.toml on the next Save while its credential file survived as an
	// orphan holding a live token.
	if err := os.Remove(s.credentialPath(uuid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stored credentials: %w", err)
	}

	s.data.Accounts = append(s.data.Accounts[:idx], s.data.Accounts[idx+1:]...)
	if s.data.ActiveUUID == uuid {
		s.data.ActiveUUID = ""
	}
	s.sortAndReindex()
	return s.Save()
}

// SetAlias assigns a unique alias. An empty alias clears it.
func (s *Store) SetAlias(uuid, alias string) error {
	if _, ok := s.Get(uuid); !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, uuid)
	}
	normalized := NormalizeAlias(alias)
	if normalized != "" {
		if err := ValidateAlias(normalized); err != nil {
			return err
		}
		for _, a := range s.data.Accounts {
			if a.UUID != uuid && NormalizeAlias(a.Alias) == normalized {
				// Named by label and uuid, never by idx: idx is an ordinal, so
				// a concurrent removal recompacts it and the number the user
				// acted on would point at a different account.
				return fmt.Errorf("%w: %q already belongs to %s (%s)",
					ErrAliasTaken, normalized, a.Label(), a.UUID)
			}
		}
	}
	for i := range s.data.Accounts {
		if s.data.Accounts[i].UUID == uuid {
			s.data.Accounts[i].Alias = normalized
			break
		}
	}
	return s.Save()
}

// Credentials returns an account's stored account-scoped keys.
func (s *Store) Credentials(uuid string) (cclink.Blob, error) {
	// Checked here too, so a hand-edited accounts.toml cannot make this read an
	// arbitrary file.
	if err := validUUID(uuid); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.credentialPath(uuid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no stored credentials for %q; re-run 'ccdad add' for it", uuid)
		}
		return nil, fmt.Errorf("reading stored credentials: %w", err)
	}
	var b cclink.Blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("stored credentials for %q are corrupt", uuid)
	}
	return b, nil
}

// SetActive records which account ccdad last activated.
func (s *Store) SetActive(uuid string) error {
	s.data.ActiveUUID = uuid
	return s.Save()
}

// ActiveUUID is the last account ccdad activated, or "".
func (s *Store) ActiveUUID() string { return s.data.ActiveUUID }

// Save writes accounts.toml atomically.
func (s *Store) Save() error {
	for i := range s.data.Accounts {
		s.data.Accounts[i].KindName = s.data.Accounts[i].Kind.String()
	}
	encoded, err := toml.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", accountsFile, err)
	}
	return cclink.WriteFileAtomic(filepath.Join(s.root, accountsFile), encoded, 0o600)
}

func (s *Store) credentialPath(uuid string) string {
	return filepath.Join(s.root, credentialsDir, uuid+".json")
}

func (s *Store) writeCredentials(uuid string, creds cclink.Blob) error {
	// json.Marshal is compact, so an already-compact RawMessage survives
	// byte-for-byte. MarshalIndent would reformat the nested values.
	encoded, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}
	return cclink.WriteFileAtomic(s.credentialPath(uuid), encoded, 0o600)
}

func (s *Store) nextIdx() int {
	max := 0
	for _, a := range s.data.Accounts {
		if a.Idx > max {
			max = a.Idx
		}
	}
	return max + 1
}

// sortAndReindex keeps the display order stable and the indices contiguous.
// Compacting on removal is why the stability contract says idx is an ordinal
// and not a key.
func (s *Store) sortAndReindex() {
	sort.SliceStable(s.data.Accounts, func(i, j int) bool {
		return s.data.Accounts[i].Idx < s.data.Accounts[j].Idx
	})
	for i := range s.data.Accounts {
		s.data.Accounts[i].Idx = i + 1
	}
}
