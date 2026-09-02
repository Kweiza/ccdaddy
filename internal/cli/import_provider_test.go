package cli

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// jwtWithExp builds an unsigned token whose payload carries only exp, which is
// the one claim the comparison reads. No signature: this compares two records
// ccdad wrote itself, and a fixture that signed them would be pinning a
// verification step that does not exist.
func jwtWithExp(exp time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
}

// The three derivations, in the order they are tried.
//
// An EXPLICIT provider always wins, and an unknown one is a skip rather than a
// guess: the document was written by something that knows more than this
// build, and the note at the top of the command already says what that means.
// An ABSENT provider with a Codex credential in the blob is a Codex account --
// the only way a document can carry that key is from a Codex account. An
// absent provider with anything else is Claude, which is what every document
// written before this field existed holds.
func TestImportDerivesEachRowsProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		row      string
		want     provider.ID
		wantSkip bool
	}{{
		name: "explicit claude",
		row:  `{"uuid":"u-1","kind":"subscription","provider":"claude","credentials":{"claudeAiOauth":{"refreshToken":"RT"}}}`,
		want: provider.Claude,
	}, {
		name: "explicit codex",
		row:  `{"uuid":"u-1","kind":"subscription","provider":"codex","credentials":{"codexOAuth":{"refresh_token":"RT","user_id":"u-1"}}}`,
		want: provider.Codex,
	}, {
		name:     "explicit and unknown",
		row:      `{"uuid":"u-1","kind":"subscription","provider":"gemini","credentials":{"claudeAiOauth":{"refreshToken":"RT"}}}`,
		wantSkip: true,
	}, {
		name: "absent, with a Codex credential",
		row:  `{"uuid":"u-1","kind":"subscription","credentials":{"codexOAuth":{"refresh_token":"RT","user_id":"u-1"}}}`,
		want: provider.Codex,
	}, {
		name: "absent, with a Claude credential",
		row:  `{"uuid":"u-1","kind":"subscription","credentials":{"claudeAiOauth":{"refreshToken":"RT"}}}`,
		want: provider.Claude,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			path := writeImportFile(t, `{"schemaVersion":2,"full":true,"accounts":[`+tc.row+`]}`)
			code, _, errOut, top := runRoot(t, "import", path)

			s, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, known := s.Get("u-1")
			if tc.wantSkip {
				if known {
					t.Fatalf("a row with an unknown provider was imported as %q", got.Provider)
				}
				if !strings.Contains(errOut, "u-1") {
					t.Errorf("the skip does not name the row: %s", errOut)
				}
				return
			}
			if !known {
				t.Fatalf("import (%d, %s) stored nothing: %s", code, top, errOut)
			}
			if got.Provider != tc.want {
				t.Errorf("Provider = %q, want %q", got.Provider, tc.want)
			}
		})
	}
}

// The snapshot filter must keep the Codex record.
//
// It filters through cclink.Extract, which keeps the five keys that travel
// with a CLAUDE login and drops everything else -- so without a name here the
// Codex credential is dropped at the boundary and the account is imported with
// no login at all. ccdadToken is added back by name for the same reason and
// this is the second such key.
func TestImportKeepsTheCodexCredential(t *testing.T) {
	isolate(t)
	path := writeImportFile(t, `{"schemaVersion":2,"full":true,"accounts":[`+
		`{"uuid":"u-x","kind":"subscription","provider":"codex",`+
		`"credentials":{"codexOAuth":{"access_token":"AT","refresh_token":"RT","user_id":"u-x"}}}]}`)
	if code, _, errOut, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s): %s", code, top, errOut)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Credentials("u-x")
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := blob["codexOAuth"]
	if !ok {
		t.Fatalf("the imported Codex account has no credential: %v", blob)
	}
	var rec struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RefreshToken != "RT" {
		t.Fatalf("refresh token = %q, want RT", rec.RefreshToken)
	}
}

// The newer-credentials check compares the right claim for each provider.
//
// It reads claudeAiOauth.expiresAt, which a Codex record does not have, so
// every Codex pair was uncomparable and every import overwrote the local
// credential silently. The access token's own exp is what is compared instead:
// the token endpoint mints a new access token on every refresh and its exp
// moves forward with it, so a later exp is a later credential. The id_token's
// exp is never read -- it is an hour from login and says nothing about the
// grant.
func TestLocalCredentialIsNewerComparesTheCodexAccessExpiry(t *testing.T) {
	newer := cclink.Blob{"codexOAuth": json.RawMessage(
		`{"access_token":"` + jwtWithExp(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)) + `","refresh_token":"RT-new"}`)}
	older := cclink.Blob{"codexOAuth": json.RawMessage(
		`{"access_token":"` + jwtWithExp(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)) + `","refresh_token":"RT-old"}`)}

	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: "u-x", Provider: provider.Codex}, newer); err != nil {
		t.Fatal(err)
	}
	if !localCredentialIsNewer(s, "u-x", older) {
		t.Error("a stored Codex credential whose access token expires a day later did not read as newer")
	}
	if localCredentialIsNewer(s, "u-x", newer) {
		t.Error("a Codex credential with the same expiry read as older than itself")
	}
}
