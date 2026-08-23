package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// writeImportFile puts a document on disk for `ccdad import` to read.
func writeImportFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// exportTo runs a --full export into a file and hands back its path, which is
// the round trip every restore actually performs.
func exportTo(t *testing.T, name string) string {
	t.Helper()
	stubStdoutTTY(t, true)
	path := filepath.Join(t.TempDir(), name)
	if code, _, _, top := runRoot(t, "export", "--full", "--out", path); code != ExitOK {
		t.Fatalf("export = %d (%s)", code, top)
	}
	return path
}

func TestImportRoundTripsAFullExport(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	if code, _, _, top := runRoot(t, "alias", "2", "work"); code != ExitOK {
		t.Fatalf("alias = %d (%s)", code, top)
	}
	if code, _, _, top := runRoot(t, "disable", "1"); code != ExitOK {
		t.Fatalf("disable = %d (%s)", code, top)
	}
	path := exportTo(t, "backup.json")

	// A second machine: a fresh store, the same Claude Code home.
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad2"))
	_ = claude

	code, _, stderr, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "Imported 2") {
		t.Errorf("stderr = %q, want both accounts reported", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 2 {
		t.Fatalf("accounts = %+v, want two", s.Accounts())
	}
	two, ok := s.Get("u-2")
	if !ok {
		t.Fatal("u-2 was not imported")
	}
	if two.Alias != "work" {
		t.Errorf("alias = %q, want %q", two.Alias, "work")
	}
	if one, _ := s.Get("u-1"); !one.Disabled {
		t.Error("the disabled flag did not survive the round trip")
	}
	creds, err := s.Credentials("u-2")
	if err != nil {
		t.Fatalf("the imported account has no credentials: %v", err)
	}
	if !strings.Contains(string(creds["claudeAiOauth"]), "RT-u-2") {
		t.Errorf("credentials = %s, want the exported snapshot", creds["claudeAiOauth"])
	}
}

// uuid is the key, so an account already here is updated rather than
// duplicated.
func TestImportOfAKnownAccountIsAnUpdate(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "old"); code != ExitOK {
		t.Fatal("setup failed")
	}
	path := writeImportFile(t, fmt.Sprintf(`{
	  "schemaVersion": 1,
	  "full": true,
	  "accounts": [{"uuid":"u-1","email":"renamed@example.com","alias":"new","kind":"subscription",
	    "credentials":{"claudeAiOauth":%s}}]
	}`, `{"accessToken":"AT2","refreshToken":"RT-imported"}`))

	if code, _, _, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 1 {
		t.Fatalf("accounts = %+v, want the one account updated in place", s.Accounts())
	}
	got, _ := s.Get("u-1")
	if got.Email != "renamed@example.com" {
		t.Errorf("email = %q, want the imported one", got.Email)
	}
	if got.Alias != "new" {
		t.Errorf("alias = %q, want the imported one — store.Add preserves the stored alias, so import has to apply it after", got.Alias)
	}
}

// The rule that import never writes mcpOAuth into the live credentials file,
// enforced one level earlier: a machine key that reached a per-account
// snapshot would be merged into the live file by the next ordinary switch,
// through a path with no rule on it.
func TestImportStripsMachineKeysFromTheSnapshot(t *testing.T) {
	isolate(t)
	path := writeImportFile(t, `{
	  "schemaVersion": 1,
	  "full": true,
	  "machine": {"mcpOAuth": {"server": {"accessToken": "MCP-AT"}}},
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{
	      "claudeAiOauth":{"accessToken":"AT","refreshToken":"RT"},
	      "mcpOAuth":{"server":{"accessToken":"MCP-LEAK"}},
	      "coworkRemoteDevice":{"org":{"key":"K"}}
	    }}]
	}`)

	code, _, stderr, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "MCP logins") {
		t.Errorf("stderr = %q, want it to say the MCP logins are not being installed", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	creds, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mcpOAuth", "coworkRemoteDevice"} {
		if _, leaked := creds[key]; leaked {
			t.Errorf("the machine key %q reached a per-account snapshot", key)
		}
	}
	if _, ok := creds["claudeAiOauth"]; !ok {
		t.Error("the account-scoped key was filtered out too")
	}

	// And the switch that would have carried it never gets the chance.
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch after import = %d (%s)", code, top)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "MCP-LEAK") {
		t.Fatalf("the imported machine key reached the live credentials file:\n%s", raw)
	}
}

// A ccdad token account is stored under ccdad's own key, which is not one of
// Claude Code's five and so is not in cclink's list. Filtering with Extract
// alone would silently discard every `ccdad add-token` account.
func TestImportKeepsTokenAccounts(t *testing.T) {
	isolate(t)
	path := writeImportFile(t, `{
	  "schemaVersion": 1,
	  "full": true,
	  "accounts": [{"uuid":"u-tok","email":"tok@example.com","kind":"api-key",
	    "credentials":{"ccdadToken":{"kind":"api-key","token":"sk-ant-api-XYZ"}}}]
	}`)

	if code, _, _, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	creds, err := s.Credentials("u-tok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(creds[cclink.TokenKey]), "sk-ant-api-XYZ") {
		t.Fatalf("the token record was dropped: %v", creds)
	}
}

// Restoring a stale refresh token over a working one turns a live account into
// a quarantined one, so the local copy wins unless --force says otherwise.
func TestImportSkipsCredentialsOlderThanTheLocalOnes(t *testing.T) {
	isolate(t)
	local := time.Now().Add(6 * time.Hour).UnixMilli()
	older := time.Now().Add(-2 * time.Hour).UnixMilli()

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	fresh := credsWithExpiry("RT-local", local)
	if err := s.Add(store.Account{UUID: "u-1", Email: "one@example.com"}, fresh); err != nil {
		t.Fatal(err)
	}
	path := writeImportFile(t, fmt.Sprintf(`{
	  "schemaVersion": 1, "full": true,
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-stale","expiresAt":%d}}}]
	}`, older))

	code, _, stderr, _ := runRoot(t, "import", path)
	if code != ExitNothingToDo {
		t.Fatalf("import of a stale credential = %d, want %d", code, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("stderr = %q, want it to name the way through", stderr)
	}
	reopened, _ := store.Open()
	got, _ := reopened.Credentials("u-1")
	if !strings.Contains(string(got["claudeAiOauth"]), "RT-local") {
		t.Fatalf("the newer local credential was overwritten: %s", got["claudeAiOauth"])
	}

	if code, _, _, top := runRoot(t, "import", path, "--force"); code != ExitOK {
		t.Fatalf("import --force = %d (%s), want 0", code, top)
	}
	reopened, _ = store.Open()
	got, _ = reopened.Credentials("u-1")
	if !strings.Contains(string(got["claudeAiOauth"]), "RT-stale") {
		t.Fatalf("--force did not overwrite: %s", got["claudeAiOauth"])
	}
}

func credsWithExpiry(refresh string, expiresAt int64) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"claudeAiOauth": json.RawMessage(fmt.Sprintf(
			`{"accessToken":"AT","refreshToken":%q,"expiresAt":%d}`, refresh, expiresAt)),
	}
}

// The atomicity requirement: store.Add and SetAlias each write on their own, so
// a collision on the fourth of five accounts would otherwise leave three
// imported and two not, with no rollback.
func TestImportIsAllOrNothing(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-held", "held@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "taken"); code != ExitOK {
		t.Fatal("setup failed")
	}

	path := writeImportFile(t, `{
	  "schemaVersion": 1, "full": true,
	  "accounts": [
	    {"uuid":"u-a","email":"a@example.com","kind":"subscription",
	      "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-a"}}},
	    {"uuid":"u-b","email":"b@example.com","alias":"taken","kind":"subscription",
	      "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-b"}}}
	  ]
	}`)

	code, _, _, top := runRoot(t, "import", path)
	if code != ExitUsage {
		t.Fatalf("import with a colliding alias = %d (%s), want %d", code, top, ExitUsage)
	}
	if !strings.Contains(top, "u-held") {
		t.Errorf("the collision message %q does not name the account holding the alias", top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 1 {
		t.Fatalf("accounts = %+v; a refused import wrote part of itself", s.Accounts())
	}
	if _, ok := s.Get("u-a"); ok {
		t.Error("the account before the collision was imported anyway")
	}
}

func TestImportRefusesADocumentThatIsNotAnExport(t *testing.T) {
	isolate(t)

	for name, body := range map[string]string{
		"not json":       `{`,
		"no version":     `{"accounts":[]}`,
		"bad uuid":       `{"schemaVersion":1,"full":true,"accounts":[{"uuid":"../escape","kind":"subscription","credentials":{"claudeAiOauth":{"refreshToken":"RT"}}}]}`,
		"duplicate uuid": `{"schemaVersion":1,"full":true,"accounts":[{"uuid":"u-1","kind":"subscription"},{"uuid":"u-1","kind":"subscription"}]}`,
		"numeric alias":  `{"schemaVersion":1,"full":true,"accounts":[{"uuid":"u-1","alias":"7","kind":"subscription","credentials":{"claudeAiOauth":{"refreshToken":"RT"}}}]}`,
	} {
		code, _, _, _ := runRoot(t, "import", writeImportFile(t, body))
		if code != ExitUsage {
			t.Errorf("%s: import = %d, want %d", name, code, ExitUsage)
		}
	}

	if code, _, _, _ := runRoot(t, "import", filepath.Join(t.TempDir(), "nope.json")); code != ExitFailure {
		t.Errorf("import of a missing file = %d, want %d", code, ExitFailure)
	}
}

// A credential-free export can still refresh what is already here, and cannot
// invent an account it has no way to log in as.
func TestImportOfAMetadataOnlyExport(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	path := writeImportFile(t, `{
	  "schemaVersion": 1, "full": false,
	  "accounts": [
	    {"uuid":"u-1","email":"renamed@example.com","alias":"one","kind":"subscription"},
	    {"uuid":"u-new","email":"new@example.com","kind":"subscription"}
	  ]
	}`)

	code, _, stderr, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "no credentials") {
		t.Errorf("stderr = %q, want it to say the account with nothing to attach to was skipped", stderr)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("u-new"); ok {
		t.Error("an account with no credentials anywhere was created")
	}
	got, ok := s.Get("u-1")
	if !ok {
		t.Fatal("the known account is gone")
	}
	if got.Alias != "one" || got.Email != "renamed@example.com" {
		t.Errorf("metadata was not updated: %+v", got)
	}
	if creds, err := s.Credentials("u-1"); err != nil || !strings.Contains(string(creds["claudeAiOauth"]), "RT-u-1") {
		t.Errorf("the local credentials were replaced by a credential-free import: %v %v", creds, err)
	}
}

// The `--json` contract is additive: a newer export's extra fields are
// ignored, not refused, or a backup becomes unreadable by the build that has
// to restore it.
func TestImportAcceptsANewerSchema(t *testing.T) {
	isolate(t)
	path := writeImportFile(t, `{
	  "schemaVersion": 99, "full": true, "somethingNew": {"a": 1},
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription","futureField":true,
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT"}}}]
	}`)

	code, _, stderr, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import of a newer schema = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "newer ccdad") {
		t.Errorf("stderr = %q, want a note that it was written by a newer build", stderr)
	}
}

// Two accounts trading aliases is a legitimate document: uuid is the key and
// the alias belongs to whichever account the export says. Applied
// account-by-account it would fail or succeed depending on which of the two the
// array happens to list first.
func TestImportCanSwapTwoAliases(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatal("setup failed")
	}
	if code, _, _, _ := runRoot(t, "alias", "2", "home"); code != ExitOK {
		t.Fatal("setup failed")
	}

	path := writeImportFile(t, `{
	  "schemaVersion": 1, "full": false,
	  "accounts": [
	    {"uuid":"u-1","email":"one@example.com","alias":"home","kind":"subscription"},
	    {"uuid":"u-2","email":"two@example.com","alias":"work","kind":"subscription"}
	  ]
	}`)

	code, _, _, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import of swapped aliases = %d (%s), want 0", code, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Alias != "home" {
		t.Errorf("u-1 alias = %q, want %q", got.Alias, "home")
	}
	if got, _ := s.Get("u-2"); got.Alias != "work" {
		t.Errorf("u-2 alias = %q, want %q", got.Alias, "work")
	}
}

// An account the document does not give an alias loses the one it had: the
// document is the answer for everything else about the account, and a
// half-applied update is worse than a stated one.
func TestImportClearsAnAliasTheDocumentDoesNotCarry(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, _ := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatal("setup failed")
	}

	path := writeImportFile(t, `{
	  "schemaVersion": 1, "full": false,
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription"}]
	}`)
	if code, _, _, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}

	s, _ := store.Open()
	if got, _ := s.Get("u-1"); got.Alias != "" {
		t.Errorf("alias = %q, want it cleared to match the document", got.Alias)
	}
}
