// Package codexauth is ccdad's Codex credential: the record it stores, the
// hash the needs-relogin mark is made of, and (from a later change) the device
// login, the token refresh and the refresh classifier.
//
// The record is ccdad's OWN. Nothing in Claude Code reads it, nothing in
// codex's own auth.json is read to produce it, and it never reaches Claude
// Code's credentials file: the key is deliberately absent from the list of
// keys that travel with a Claude login.
package codexauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// Key is the one ccdad-owned top-level key in a Codex account's credential
// blob. It is the on-disk name and cannot be renamed: a stored account would
// become one with no login.
const Key = "codexOAuth"

// Credential is the JSON object stored under Key.
//
// The two identity fields are named for what the endpoint calls them, because
// they are easy to swap and the symptom of swapping them is a bearer that is
// valid for the wrong workspace. AccountID is chatgpt_account_id -- the
// WORKSPACE -- and is what the proxy sends as the ChatGPT-Account-Id header.
// UserID is chatgpt_user_id, which is the account's uuid in ccdad's store.
type Credential struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	UserID       string    `json:"user_id"`
	LastRefresh  time.Time `json:"last_refresh"`
}

// FromBlob reads the Codex record out of a stored credential blob.
//
// The three answers are distinct on purpose. Absent is the ORDINARY state --
// every Claude account is one -- so it is (zero, false, nil) and a caller can
// ask about every account without treating the majority as broken. Present and
// unparseable is an error, because reading a truncated write as "no Codex
// record" would send the account down the Claude path with a token sitting in
// its file.
func FromBlob(b cclink.Blob) (Credential, bool, error) {
	raw, ok := b[Key]
	if !ok {
		return Credential{}, false, nil
	}
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credential{}, false, fmt.Errorf("the stored %s record cannot be read: %w", Key, err)
	}
	return c, true, nil
}

// ToBlob is the credential as a stored blob: one key and nothing else.
//
// A blob rather than raw JSON because store.Add and store.SetCredentials
// replace the credential file WHOLESALE, so the value handed to them is the
// whole file. A Codex account has exactly this in it.
func (c Credential) ToBlob() cclink.Blob {
	// The error is impossible: every field is a string or a time.Time, and
	// neither can fail to marshal. It is dropped rather than returned so that
	// callers building a blob inline do not carry an error nothing can
	// produce -- and an empty RawMessage would fail loudly at the next read.
	encoded, err := json.Marshal(c)
	if err != nil {
		return cclink.Blob{}
	}
	return cclink.Blob{Key: json.RawMessage(encoded)}
}

// RefreshTokenHash names a refresh token without being one.
//
// It is what the needs-relogin mark holds, and the mark lives in accounts.toml
// -- the 0600-but-plaintext file people paste into an issue -- so the value
// stored there must not be a credential. A 64-bit prefix is enough: the only
// comparison ever made is against the hash of the token stored for the SAME
// account moments later, so the space being guarded is one account's token
// history rather than every token in the world.
//
// The empty token hashes to the empty string rather than to the hash of "".
// The mark is compared with equality, and an account that has no refresh token
// at all must not match a mark written when it had one.
func RefreshTokenHash(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:])[:16]
}
