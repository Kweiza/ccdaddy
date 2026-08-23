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
// Every mutator runs its read-modify-write under the cross-process lock in
// lock.go, which is what makes two concurrent ccdad processes safe: the state
// is re-read inside the lock, so the write is against what the previous holder
// left rather than against whatever Open happened to see. Reads take no lock —
// every write is an atomic rename, so a reader sees one whole version of the
// document or another, never a torn one.
//
// A Store is NOT safe for concurrent use by several goroutines in one process.
// The lock excludes other processes; it does not serialize this one.
type Store struct {
	root string
	data file
	// inTx marks a mutator running inside a WithStore callback, where the lock
	// is already held and the save belongs to the transaction. See mutate.
	inTx bool
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

// ValidateUUID refuses anything that could not be an account uuid. The value
// arrives from the profile endpoint and becomes a path component, so a
// traversal sequence in it would write a credential file outside the store.
//
// It is exported for the same reason ValidateAlias is: a caller staging many
// accounts — `ccdad import` — has to be able to reject the whole batch BEFORE
// the first credential file is written, and the only alternative is
// re-spelling the rule somewhere it can drift from this one.
func ValidateUUID(uuid string) error {
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
	root, err := ccpath.StoreHome()
	if err != nil {
		return nil, err
	}
	// An unresolvable home is now ccpath's error to report, but a CCDAD_HOME
	// that is itself relative still reaches here — and a relative store means
	// ccdad creates a credentials tree in whatever directory it happened to be
	// run from, a different one each time, with tokens in it. Refuse instead:
	// the tokens are the whole point of the directory, so the wrong directory
	// is not a degradation worth accepting silently.
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

	s := &Store{root: root}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads accounts.toml into the store, replacing whatever it held.
//
// It is separate from Open because mutate calls it a second time, INSIDE the
// lock: the copy Open produced was read before the lock was granted, and the
// process that held the lock in between is exactly the one whose write would
// otherwise be lost. Open's directory creation and mode tightening are
// deliberately not repeated here — they are a once-per-process concern, and a
// reload runs on every write.
func (s *Store) load() error {
	s.data = file{Version: 1}

	raw, err := os.ReadFile(filepath.Join(s.root, accountsFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", accountsFile, err)
	}
	if err := toml.Unmarshal(raw, &s.data); err != nil {
		return fmt.Errorf("parsing %s: %w", accountsFile, err)
	}
	for i := range s.data.Accounts {
		// An unrecognized name — an older accounts.toml, or a kind a future
		// release adds — resolves to subscription deliberately. Guessing credit
		// would let the account through the credit gate, which spends money;
		// guessing subscription only costs a wasted rotation attempt.
		s.data.Accounts[i].Kind = identity.ParseKind(s.data.Accounts[i].KindName)
	}
	s.sortAndReindex()
	return nil
}

// AccountsAt reads a store's accounts WITHOUT creating any part of it.
//
// Open is the ordinary entry point and it is the wrong one for a diagnostic:
// it does an MkdirAll and two Chmods, which is right for every caller that is
// about to write and manufactures the very thing `ccdad doctor` was asked to
// report on. This is the same read Open performs, with the creation and the
// mode tightening left out — a store that is not there yields no accounts
// rather than being brought into existence.
//
// It takes the root rather than resolving one so the caller that already
// resolved it does not resolve it twice and risk answering about a different
// directory than the rest of its report.
func AccountsAt(root string) ([]Account, error) {
	s := &Store{root: root}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s.Accounts(), nil
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
	return s.mutate(func() error { return s.add(a, creds) })
}

// add is Add's body, running with the lock already held and the state already
// re-read. It does not save; mutate does, exactly once, so that a WithStore
// callback adding five accounts writes accounts.toml once rather than five
// times with four intermediate states visible on disk.
func (s *Store) add(a Account, creds cclink.Blob) error {
	if err := ValidateUUID(a.UUID); err != nil {
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
	// in-memory store, which any later save() would then persist.
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
	return nil
}

// Remove deletes an account and its stored credentials.
func (s *Store) Remove(uuid string) error {
	return s.mutate(func() error { return s.remove(uuid) })
}

func (s *Store) remove(uuid string) error {
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
	return nil
}

// SetAlias assigns a unique alias. An empty alias clears it.
func (s *Store) SetAlias(uuid, alias string) error {
	return s.mutate(func() error { return s.setAlias(uuid, alias) })
}

func (s *Store) setAlias(uuid, alias string) error {
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
	return nil
}

// Credentials returns an account's stored account-scoped keys.
func (s *Store) Credentials(uuid string) (cclink.Blob, error) {
	// Checked here too, so a hand-edited accounts.toml cannot make this read an
	// arbitrary file.
	if err := ValidateUUID(uuid); err != nil {
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
	return s.mutate(func() error {
		s.data.ActiveUUID = uuid
		return nil
	})
}

// ActiveUUID is the last account ccdad activated, or "".
func (s *Store) ActiveUUID() string { return s.data.ActiveUUID }

// save writes accounts.toml atomically.
//
// It is unexported, and that is the point: it does not take the lock — every
// caller reaches it through mutate, which already holds one, and a save that
// locked on its own would deadlock inside every transaction. Exported, it would
// be the one way left to write the store without the lock, which is the bug
// this whole file exists to remove.
func (s *Store) save() error {
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

// sortAndReindex puts the slice into stored-Idx order and renumbers it.
// Compacting on removal is why the stability contract says idx is an ordinal
// and not a key.
//
// Move must NOT call this. It sorts by the Idx already on disk, so a reordered
// slice is sorted straight back into the order it had; reindex alone is what a
// caller wants once the slice itself is the answer.
func (s *Store) sortAndReindex() {
	sort.SliceStable(s.data.Accounts, func(i, j int) bool {
		return s.data.Accounts[i].Idx < s.data.Accounts[j].Idx
	})
	s.reindex()
}

// reindex renumbers the accounts from their current slice positions, without
// reordering them.
func (s *Store) reindex() {
	for i := range s.data.Accounts {
		s.data.Accounts[i].Idx = i + 1
	}
}

// SetDisabled holds an account out of auto-rotation, or puts it back, and
// reports whether that was a change.
//
// Disabled is a policy for the auto engine and NOT a lock: an explicit `ccdad
// switch <account>` still works on a disabled account, because a user naming an
// account by hand has said what they want more clearly than the flag has. The
// bool exists so the caller can spend §9.3's exit 3 — "the world is already as
// you asked" — rather than reporting a no-op as an action taken.
func (s *Store) SetDisabled(uuid string, disabled bool) (changed bool, err error) {
	err = s.mutate(func() error {
		idx := -1
		for i := range s.data.Accounts {
			if s.data.Accounts[i].UUID == uuid {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, uuid)
		}
		changed = s.data.Accounts[idx].Disabled != disabled
		s.data.Accounts[idx].Disabled = disabled
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// Move puts an account at a 1-based display position and renumbers the rest.
//
// A position past the end clamps to the end rather than erroring: "put it last"
// is what a caller who typed a big number meant, and the alternative is making
// them count. A position below 1 clamps to the front for the same reason —
// callers that want to refuse 0 as a non-position do so before calling, where
// they can say so in the user's own terms.
//
// It renumbers rather than reorders-and-sorts. sortAndReindex sorts on the Idx
// values already stored, so a moved slice handed to it is sorted straight back
// into the order it started in — which is also what a "just set the new Idx and
// Save" implementation runs into on the next Open, where two accounts then share
// an Idx and SliceStable breaks the tie by slice position.
//
// It reports whether the account actually moved, so a caller can spend exit 3
// on a move that asked for the position the account already holds.
//
// Every account between the source and the destination changes number, so a
// script holding an idx from an earlier `ccdad list` now acts on a different
// account. That is the stability contract in `ccdad --help` — idx is an ordinal,
// not a key — and move is the command that makes it routine rather than a side
// effect of removal.
func (s *Store) Move(uuid string, position int) (changed bool, err error) {
	err = s.mutate(func() error {
		from := -1
		for i := range s.data.Accounts {
			if s.data.Accounts[i].UUID == uuid {
				from = i
				break
			}
		}
		if from < 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, uuid)
		}
		to := position - 1
		if to < 0 {
			to = 0
		}
		if last := len(s.data.Accounts) - 1; to > last {
			to = last
		}
		if to == from {
			return nil
		}
		changed = true

		moved := s.data.Accounts[from]
		rest := append(s.data.Accounts[:from:from], s.data.Accounts[from+1:]...)
		reordered := make([]Account, 0, len(s.data.Accounts))
		reordered = append(reordered, rest[:to]...)
		reordered = append(reordered, moved)
		reordered = append(reordered, rest[to:]...)
		s.data.Accounts = reordered
		s.reindex()
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
