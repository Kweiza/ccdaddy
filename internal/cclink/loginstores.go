package cclink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// LoginStores is what each of Claude Code's two login stores holds, read
// separately.
//
// Load answers "who is logged in" and stops at the first store that has an
// answer, because that is how Claude Code's combinator reads. That is the right
// question for most callers and the wrong one for a machine whose stores
// DISAGREE: the item names one account, the file names another, and Load
// reports the item and nothing else. On 2026-11-10 that was the state a
// timed-out keychain write left behind, and every ccdad command that asked
// through Load saw a machine that looked switched while Claude Code -- which
// falls back to the file whenever its own keychain read fails, as it does on a
// loaded Mac -- went on spending the other account. Telling the two apart needs
// both answers.
type LoginStores struct {
	// Item is the keychain item's login, valid when HasItem. Off macOS, and on
	// a Mac with no item, HasItem is false: the file is then the only store.
	Item    Blob
	HasItem bool
	// File is the credentials file's login. FileErr is why it could not be
	// read; with an item present that is not a failure of the whole read,
	// because Claude Code reads the item first.
	File    Blob
	FileErr error
}

// Live is the login Claude Code reads first, and which store that is: the item
// when there is one, the file otherwise. It is Load's answer, from the same
// read.
func (l LoginStores) Live() (Blob, CredentialSource, error) {
	if l.HasItem {
		return l.Item, SourceKeychainItem, nil
	}
	if l.FileErr != nil {
		return nil, SourceNone, l.FileErr
	}
	if len(l.File) == 0 {
		return l.File, SourceNone, nil
	}
	return l.File, SourceCredentialsFile, nil
}

// base is the blob a switch merges onto and decides from: the item with the
// file's machine keys folded back in, or the file alone when there is no item.
// loadLiveUnderLock carries the argument for both halves.
func (l LoginStores) base() Blob {
	if !l.HasItem {
		return l.File
	}
	return overlayMachineKeys(l.Item, l.File)
}

// ReadLoginStores reads both stores: the item with the LOOKUP bound, the file
// at the path Claude Code would read.
//
// The error is for an item that could not be read, and it is the only error:
// a locked keychain is an unknown about the store Claude Code reads first, and
// nothing about the file can stand in for it. A file that cannot be read is
// carried in FileErr, so a caller that only wants Live gets exactly what Load
// gave it.
func ReadLoginStores() (LoginStores, error) {
	item, ok, err := loadFromKeychain(keychainReadTimeout)
	if err != nil {
		return LoginStores{}, err
	}
	out := LoginStores{Item: item, HasItem: ok}
	path, err := ccpath.CredentialsPath()
	if err != nil {
		out.FileErr = err
		return out, nil
	}
	out.File, out.FileErr = loadFrom(path)
	return out, nil
}

// LoginIdentity is the value that says whether two blobs hold the same login:
// the refresh token, or the access token for a record without one, prefixed by
// which it is. "" means "cannot identify" and never equals anything.
//
// The refresh token leads because it survives an access-token rotation; the
// kind prefix keeps an access token stored in one record from matching a
// refresh token stored in another. switcher.CredentialIdentity is this, under
// the name its attribution rules are written against.
func LoginIdentity(b Blob) string {
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

// LoginExpiry is when a blob's login expires, and whether it says. It is the
// evidence of WHICH of two logins was issued later: Claude Code's grants all
// last as long, so the later expiry is the later issue.
func LoginExpiry(b Blob) (time.Time, bool) {
	return oauthExpiresAt(b)
}

// sameCredential reports whether two blobs carry the same OAuth credential:
// equal access token, refresh token and expiry, with at least one token
// present. Equal identity alone is not enough -- see writeMerged.
func sameCredential(a, b Blob) bool {
	ca, okA := credentialOf(a)
	cb, okB := credentialOf(b)
	return okA && okB && ca == cb && (ca.access != "" || ca.refresh != "")
}

type credentialFields struct {
	access, refresh string
	expiresAt       int64
	hasExpiry       bool
}

func credentialOf(b Blob) (credentialFields, bool) {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return credentialFields{}, false
	}
	var wire struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    *int64 `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return credentialFields{}, false
	}
	c := credentialFields{access: wire.AccessToken, refresh: wire.RefreshToken}
	if wire.ExpiresAt != nil {
		c.expiresAt, c.hasExpiry = *wire.ExpiresAt, true
	}
	return c, true
}

// ErrLoginOutcomeUnknown is a switch whose keychain write ran out of time and
// whose item could not be read back to say whether it landed. Nothing further
// was written: the credentials file still holds what it held, and the item
// holds either login. It is not "nothing changed" and it is not "switched", and
// a caller must not report either.
var ErrLoginOutcomeUnknown = errors.New("whether the keychain item now holds the new login is not known")

// writeOutcome is what reading the item back after an unconfirmed write found.
type writeOutcome uint8

const (
	// writeUnresolved: the read failed, or the item holds a login that is
	// neither the one written nor the one it replaced.
	writeUnresolved writeOutcome = iota
	// writeLanded: the item holds the login that was being written.
	writeLanded
	// writeDidNotLand: the item still holds the login it held before.
	writeDidNotLand
)

// confirmKeychainWrite reads the item back after a write whose outcome is not
// known, and says which login it holds.
//
// It reads with keychainWriteTimeout and a fresh deadline of its own: the
// keychain that was too slow for the write is the one being asked, and the
// time the write already spent is no reason to give the answer less.
//
// The comparison is by CREDENTIAL -- access token, refresh token and expiry
// -- the same sameCredential the write-skip uses, not by login identity. A
// write that rotates the access token and keeps the refresh token has the
// same identity before and after; comparing identities would call it landed
// when the item still holds the old access token. When written and previous
// are the same credential the item holds what was asked for either way, and
// that reads as landed.
//
// A LOGOUT is the write with no login in it (ClearLogin, for an api-key
// account), so "the login it was given" is NO login: an item that holds none,
// or is gone, has landed. Comparing identities there would compare "" with "",
// which LoginIdentity's contract says never matches, and every logout that ran
// out of time would read as unknown however it went.
func confirmKeychainWrite(written, previous Blob) writeOutcome {
	got, ok, err := loadKeychainCredentialsWithin(context.Background(), keychainWriteTimeout)
	if err != nil {
		return writeUnresolved
	}
	if _, wantsLogin := written["claudeAiOauth"]; !wantsLogin {
		if _, hasLogin := got["claudeAiOauth"]; !ok || !hasLogin {
			return writeLanded
		}
	}
	switch {
	case !ok:
		return writeUnresolved
	case sameCredential(got, written):
		return writeLanded
	case sameCredential(got, previous):
		return writeDidNotLand
	}
	return writeUnresolved
}

// settleKeychainWrite turns a failed keychain write into what it means for the
// switch, and returns nil only when the write landed after all.
//
// A failure `security` ANSWERED is the failure it says it is: nothing was
// written. A failure that was ccdad giving up on it (writeOutcomeUnknown) is
// read back first, because the request may have reached securityd before the
// client was killed -- and reporting "nothing was written" about a write that
// landed is what left the item on one account and the file on another.
func settleKeychainWrite(writeErr error, written, previous Blob) error {
	if writeOutcomeUnknown(writeErr) {
		switch confirmKeychainWrite(written, previous) {
		case writeLanded:
			return nil
		case writeUnresolved:
			return unconfirmedWriteError(writeErr)
		}
	}
	return fmt.Errorf("the keychain item Claude Code reads first could not be updated, so nothing "+
		"was written and the login did not change: %w", writeErr)
}

// unconfirmedWriteError is what writeMerged returns when confirmKeychainWrite
// could not settle a write. It says, in a user's terms, what is and is not
// known, and that the next check repairs it.
func unconfirmedWriteError(writeErr error) error {
	return fmt.Errorf("%w: the keychain item Claude Code reads first did not answer while it was being "+
		"updated (the write is allowed %s), and reading it back did not say what it holds now. The "+
		"credentials file was left as it was. The daemon compares both stores on its next pass and puts "+
		"one login in both; running the same command again settles it now: %v",
		ErrLoginOutcomeUnknown, keychainWriteTimeout, writeErr)
}
