package codexauth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

func sample() Credential {
	return Credential{
		IDToken:      "header.payload.signature",
		AccessToken:  "AT-1",
		RefreshToken: "RT-1",
		AccountID:    "acct-1",
		UserID:       "user-1",
		LastRefresh:  time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
	}
}

// The blob is ccdad's OWN record. Claude Code has never heard of this key, and
// the one place it could reach Claude Code's file is a merge -- which is what
// the separation test in this package pins.
func TestToBlobUsesTheOneKey(t *testing.T) {
	b := sample().ToBlob()
	if len(b) != 1 {
		t.Fatalf("ToBlob() carries %d keys, want exactly one", len(b))
	}
	if _, ok := b[Key]; !ok {
		t.Fatalf("ToBlob() carries %v, want the %s key", keysOf(b), Key)
	}
	if Key != "codexOAuth" {
		t.Fatalf("Key = %q; it is the on-disk name and cannot be renamed", Key)
	}
}

func keysOf(b cclink.Blob) []string {
	out := make([]string, 0, len(b))
	for k := range b {
		out = append(out, k)
	}
	return out
}

func TestBlobRoundTrip(t *testing.T) {
	want := sample()
	got, ok, err := FromBlob(want.ToBlob())
	if err != nil {
		t.Fatalf("FromBlob() = %v, want nil", err)
	}
	if !ok {
		t.Fatal("FromBlob() reported the key absent from a blob ToBlob just built")
	}
	if got != want {
		t.Fatalf("FromBlob(ToBlob(c)) = %+v, want %+v", got, want)
	}
}

// A Claude account's blob has no Codex key, and that is the ordinary state
// rather than a fault: the answer is (zero, false, nil), so a caller can ask
// every account without treating the majority as broken.
func TestFromBlobOnABlobWithoutTheKey(t *testing.T) {
	got, ok, err := FromBlob(cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT"}`)})
	if err != nil {
		t.Fatalf("FromBlob() = %v, want nil for a blob that simply has no Codex record", err)
	}
	if ok {
		t.Fatal("FromBlob() claimed a Claude blob carries a Codex credential")
	}
	if got != (Credential{}) {
		t.Fatalf("FromBlob() = %+v alongside false, want the zero Credential", got)
	}
}

// A nil blob is what store.Credentials hands back for an account whose file was
// never written. It is the same answer as a blob without the key.
func TestFromBlobOnANilBlob(t *testing.T) {
	_, ok, err := FromBlob(nil)
	if err != nil || ok {
		t.Fatalf("FromBlob(nil) = _, %v, %v; want false, nil", ok, err)
	}
}

// Present but unparseable is the third answer and must not be quiet. It means
// a hand edit or a truncated write, and reading it as "no Codex credential"
// would send the account down the Claude path.
func TestFromBlobOnACorruptRecord(t *testing.T) {
	_, ok, err := FromBlob(cclink.Blob{Key: json.RawMessage(`{"access_token":`)})
	if err == nil {
		t.Fatal("FromBlob() accepted an unparseable Codex record")
	}
	if ok {
		t.Error("FromBlob() reported ok alongside its error")
	}
	if !strings.Contains(err.Error(), Key) {
		t.Errorf("FromBlob error = %q, want it to name the key it could not read", err)
	}
}

// The stored JSON uses snake_case. It is ccdad's own document rather than
// anything a vendor defined, but it is on disk, so the names are a
// compatibility commitment and a struct-tag typo is silent: the field would
// round-trip through this package and read back empty from a file written by
// the build before it.
func TestTheStoredRecordUsesTheNamesOnDisk(t *testing.T) {
	raw := sample().ToBlob()[Key]
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_token", "access_token", "refresh_token", "account_id", "user_id", "last_refresh"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("the stored record has no %q field; it carries %v", name, keysOfAny(fields))
		}
	}
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The hash is a PREFIX of a sha256, and it is what the needs-relogin mark is
// compared against. It must be deterministic, must differ for different
// tokens, and must never be the token: the mark is written into accounts.toml,
// which is the file people paste into an issue.
func TestRefreshTokenHash(t *testing.T) {
	a := RefreshTokenHash("RT-1")
	if a != RefreshTokenHash("RT-1") {
		t.Fatal("RefreshTokenHash is not deterministic")
	}
	if a == RefreshTokenHash("RT-2") {
		t.Fatal("RefreshTokenHash collided on two different tokens")
	}
	if len(a) != 16 {
		t.Fatalf("RefreshTokenHash(...) has length %d, want 16", len(a))
	}
	if strings.Contains(a, "RT-1") {
		t.Fatal("RefreshTokenHash returned something containing the token")
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("RefreshTokenHash returned %q, which is not lowercase hex", a)
		}
	}
}

// The empty token hashes to the empty string rather than to the hash of "".
// The mark is compared against this, and an account with no refresh token at
// all must not match a mark that was written for one.
func TestRefreshTokenHashOfNothingIsNothing(t *testing.T) {
	if got := RefreshTokenHash(""); got != "" {
		t.Fatalf("RefreshTokenHash(\"\") = %q, want the empty string", got)
	}
}
