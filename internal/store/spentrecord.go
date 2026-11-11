package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The record of spent stored logins, on disk: what the daemon established when
// it named a newer login of an account and could not store it, and nothing
// else (Account.StoredLoginSpent says why each half is there).
//
// IT IS A FILE OF ITS OWN, with a lock of its own, and that is the point of it.
// It is written when storing a login FAILED, possibly on the store lock
// accounts.toml is under, so it cannot live there; and it FAILS CLOSED -- a
// record that cannot be read holds stored logins, and is never written over --
// which the engine's anti-flap state must not: that one degrades towards more
// switching and is rewritten by its next write. Kept in strategy.json, the two
// policies fought over one document. Apart, an unreadable record costs stored
// logins and nothing else, an unreadable state costs a cooldown and nothing
// else, and a binary that predates the record never opens it, so it cannot
// drop it either.

const (
	// SpentRecordFileName is the record's file in the store home.
	SpentRecordFileName = "spent-logins.json"
	// spentRecordLockDir is a DIRECTORY, because that is what cclock's mutex
	// is.
	spentRecordLockDir = SpentRecordFileName + ".lock"
	// spentRecordLockStale mirrors the engine state's: a write is a
	// sub-second operation, so this only matters after a crash, and it stays
	// at least twice cclock's touch interval.
	spentRecordLockStale = 30 * time.Second
)

// spentRecordFile is the on-disk shape.
type spentRecordFile struct {
	Version int `json:"version"`
	// NamedLogins is the identity oracle's answer about every login it named
	// as a managed account's and ccdad could not store: the account's uuid,
	// keyed by the login's LoginHash -- a hash, for the reason
	// SupersededLogins in accounts.toml is one. Only the profile endpoint can
	// say whose such a login is; whether a store still holds it, and whether
	// it was issued after the stored login, every process derives.
	NamedLogins map[string]string `json:"named_logins,omitempty"`
	// SpentStoredLogins is, per account uuid, the stored login known to be
	// SPENT -- the one the account stored when the oracle named a newer login
	// of it that ccdad then could not store -- with that newer login, both as
	// LoginHash. The newer login can be rotated again, out of every store,
	// before the oracle names what replaced it, and then nothing connects the
	// stored login to it; its refresh token is no less spent.
	SpentStoredLogins map[string]spentStoredLogin `json:"spent_stored_logins,omitempty"`
}

// spentStoredLogin is one SpentStoredLogins entry. Spent is "" when the
// stored login could not be read when it was found spent.
type spentStoredLogin struct {
	Spent string `json:"spent,omitempty"`
	Newer string `json:"newer"`
}

// SpentRecord is the parsed record. It is an OracleRecord.
type SpentRecord struct {
	data    spentRecordFile
	loadErr error
}

// SpentRecordPath is where the record lives.
func SpentRecordPath() (string, error) {
	root, err := spentRecordRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, SpentRecordFileName), nil
}

func spentRecordRoot() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}
	return root, nil
}

// LoadSpentRecord reads the record without taking a lock: every write is a
// rename, so a reader sees one whole version or another. A record that was
// never written is empty, and that is not an error. One that cannot be read
// or parsed is empty too, with why in LoadError -- and an unreadable record is
// not an empty one: it holds stored logins (Account.StoredLoginSpent), and
// WithSpentRecord will not write over it.
func LoadSpentRecord() (*SpentRecord, error) {
	path, err := SpentRecordPath()
	if err != nil {
		return nil, err
	}
	r := &SpentRecord{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		r.loadErr = fmt.Errorf("reading %s: %w", SpentRecordFileName, err)
		return r, nil
	}
	if err := json.Unmarshal(raw, &r.data); err != nil {
		r.data = spentRecordFile{}
		r.loadErr = fmt.Errorf("parsing %s: %w", SpentRecordFileName, err)
	}
	return r, nil
}

// WithSpentRecord runs fn against the record under its lock and writes back
// what it changed: temp file, fsync, rename (atomicfile.WriteFile).
//
// NEVER OVER A RECORD THAT COULD NOT BE READ. Writing one would put an empty
// record in its place and release every hold it kept, in every process,
// without a word. The file stays for a person (SpentRecordAdvice says what to
// do with it), and the write is refused.
func WithSpentRecord(timeout time.Duration, fn func(*SpentRecord) error) (err error) {
	root, err := spentRecordRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}
	lock, err := cclock.Acquire(filepath.Join(root, spentRecordLockDir), cclock.Options{
		Stale:   spentRecordLockStale,
		Timeout: timeout,
	})
	if err != nil {
		return fmt.Errorf("locking the record of spent stored logins: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	r, err := LoadSpentRecord()
	if err != nil {
		return err
	}
	if lerr := r.LoadError(); lerr != nil {
		return fmt.Errorf("not writing the record of spent stored logins over one that could not be read: %w", lerr)
	}
	if err := fn(r); err != nil {
		return err
	}
	r.data.Version = 1
	encoded, err := json.Marshal(r.data)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", SpentRecordFileName, err)
	}
	return atomicfile.WriteFile(filepath.Join(root, SpentRecordFileName), encoded, 0o600)
}

// LoadError is why the record could not be read, nil when it could or when
// it was never written.
func (r *SpentRecord) LoadError() error { return r.loadErr }

// NamedLogin is the account the identity oracle named the login with this hash
// as, when ccdad could not store it.
func (r *SpentRecord) NamedLogin(loginHash string) (uuid string, ok bool) {
	uuid, ok = r.data.NamedLogins[loginHash]
	return uuid, ok
}

// NameLogin records the oracle's answer that the login with this hash is
// uuid's, for a login ccdad could not store.
func (r *SpentRecord) NameLogin(loginHash, uuid string) {
	if r.data.NamedLogins == nil {
		r.data.NamedLogins = map[string]string{}
	}
	r.data.NamedLogins[loginHash] = uuid
}

// ForgetNamedLogins forgets every named login keep refuses, and reports
// whether it forgot any.
func (r *SpentRecord) ForgetNamedLogins(keep func(loginHash string) bool) bool {
	forgot := false
	for h := range r.data.NamedLogins {
		if !keep(h) {
			delete(r.data.NamedLogins, h)
			forgot = true
		}
	}
	return forgot
}

// SpentStoredLogin is the stored login of uuid known to be spent, and the
// newer login that spent it, by LoginHash.
func (r *SpentRecord) SpentStoredLogin(uuid string) (spent, newer string, ok bool) {
	e, ok := r.data.SpentStoredLogins[uuid]
	return e.Spent, e.Newer, ok
}

// RecordSpentStoredLogin records that uuid's stored login with hash spent ("" for
// one that could not be read) was spent by the login with hash newer.
//
// An unknown spent login never replaces a known one. It is what a retry
// records when its read of the stored login failed, which says nothing about
// which login is spent; recorded over a known hash it would lift the hold the
// moment the stored login could be read again -- and the login read then may
// be the very one that is spent.
func (r *SpentRecord) RecordSpentStoredLogin(uuid, spent, newer string) {
	if r.data.SpentStoredLogins == nil {
		r.data.SpentStoredLogins = map[string]spentStoredLogin{}
	}
	if prev, ok := r.data.SpentStoredLogins[uuid]; ok && spent == "" {
		spent = prev.Spent
	}
	r.data.SpentStoredLogins[uuid] = spentStoredLogin{Spent: spent, Newer: newer}
}

// ForgetSpentStoredLogins forgets the record of every account keep refuses,
// and reports whether it forgot any.
func (r *SpentRecord) ForgetSpentStoredLogins(keep func(uuid string) bool) bool {
	forgot := false
	for uuid := range r.data.SpentStoredLogins {
		if !keep(uuid) {
			delete(r.data.SpentStoredLogins, uuid)
			forgot = true
		}
	}
	return forgot
}
