package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"sort"
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
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: "u-1", Email: "one@example.com"}, fresh); err != nil {
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

// primary decides whether a credit-metered seat is ranked beside the
// subscriptions and whether the money ceiling gates it, so a restore that
// dropped it would rebuild a machine that behaves differently from the one it
// was taken from — silently, and only on the day the main pool runs out.
//
// Both halves of the carry are asserted, because they are two different lines
// of code. The store.Account literal is what puts the flag on an account this
// machine has never seen; the SetPrimary call after the add is what puts it on
// one that is already here, since store.Add deliberately preserves the STORED
// flag over an incoming one. Without the second, an import could never turn
// primary back OFF, which is the direction a restore of an older backup needs.
func TestPrimarySurvivesAnExportAndImport(t *testing.T) {
	isolate(t)
	seedPrimaryCreditAccount(t, "u-1", "seat@example.com")
	seedAccount(t, "u-2", "plain@example.com")
	armed := exportTo(t, "armed.json")

	// A machine that has never seen either account.
	isolate(t)
	if code, _, stderr, top := runRoot(t, "import", armed); code != ExitOK {
		t.Fatalf("import = %d (%s%s)", code, stderr, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); !got.Primary {
		t.Error("the imported seat is not primary; the restored machine keeps the credit ceiling the original did not have")
	}
	if got, _ := s.Get("u-2"); got.Primary {
		t.Error("an ordinary account came back primary")
	}

	// And the other direction: a document that does not mark the account
	// primary, imported over a store where it is.
	isolate(t)
	seedAccount(t, "u-1", "seat@example.com")
	plain := exportTo(t, "plain.json")

	isolate(t)
	seedPrimaryCreditAccount(t, "u-1", "seat@example.com")
	if code, _, stderr, top := runRoot(t, "import", plain); code != ExitOK {
		t.Fatalf("import = %d (%s%s)", code, stderr, top)
	}
	reopened, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get("u-1"); got.Primary {
		t.Error("the account is still primary after importing a document that does not mark it so")
	}
}

// The disabled half of the same rule, which nothing pinned before. store.Add
// preserves the STORED flag over an incoming one, so an import that did not
// apply the flag after the add could never turn `disabled` back OFF: restoring
// a backup taken before an account was held out of rotation would leave it held
// out, and the document that says otherwise would be ignored in silence.
func TestImportClearsADisabledFlagTheDocumentDoesNotCarry(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	path := exportTo(t, "before-it-was-held.json")

	isolate(t)
	seedDisabledAccount(t, "u-1", "one@example.com")
	if code, _, stderr, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s%s)", code, stderr, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Disabled {
		t.Error("the account is still disabled after importing a document that does not carry the flag")
	}
}

// The other half of the same rule. `ccdad import` was named on a command line
// by a person who can act on the number, so it keeps saying which version the
// document declares — the note moved out of readExport rather than away.
func TestImportNamesTheSchemaVersionADocumentDeclares(t *testing.T) {
	isolate(t)
	path := writeImportFile(t, `{"schemaVersion":99,"full":true,"accounts":[
	  {"uuid":"u-1","email":"one@example.com","kind":"subscription",
	   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`)

	code, _, stderr, top := runRoot(t, "import", path)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, stderr, top)
	}
	if !strings.Contains(stderr, "schema 99") {
		t.Errorf("stderr = %q, want it to name the version the document declares", stderr)
	}
}

// The all-or-nothing promise stopped at accounts.toml. A row that fails AFTER
// its store.Add — an I/O failure on the fourth of five credential writes, not
// a defect in the document — left every row before it on disk as a credential
// file no accounts.toml named: a live refresh token that `ccdad list`, `ccdad
// remove` and `ccdad doctor` cannot see, because all three read the document.
//
// The failure is injected by putting a DIRECTORY where the second account's
// credential file goes, so the atomic write's rename cannot land on it. It
// stands in for the reachable causes — ENOSPC, EIO, a mode the store did not
// set — and is the only one a test can produce without adding a seam to the
// store for its own sake.
func TestImportLeavesNoCredentialFileWhenTheBatchFailsAfterTheAdd(t *testing.T) {
	const leaked = "RT-DO-NOT-LEAVE-THIS-BEHIND"
	isolate(t)
	home := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(filepath.Join(home, "credentials", "u-b.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	path := writeImportFile(t, fmt.Sprintf(`{
	  "schemaVersion": 1, "full": true,
	  "accounts": [
	    {"uuid":"u-a","email":"a@example.com","kind":"subscription",
	      "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":%q}}},
	    {"uuid":"u-b","email":"b@example.com","kind":"subscription",
	      "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-b"}}}
	  ]
	}`, leaked))

	code, _, _, top := runRoot(t, "import", path)
	if code != ExitFailure {
		t.Fatalf("import = %d (%s), want %d: the machine is what is wrong, not the document", code, top, ExitFailure)
	}

	orphan := filepath.Join(home, "credentials", "u-a.json")
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("a refused import left a live refresh token at %s with no account naming it", orphan)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Accounts(); len(got) != 0 {
		t.Errorf("accounts = %+v; a refused import wrote part of itself", got)
	}
	if strings.Contains(top, leaked) {
		t.Errorf("the refusal repeated a refresh token out of the document:\n%s", top)
	}
}

// The transport --base64 exists for: the document goes into a GitHub secret or
// a `.env` line as one string, and comes back out the far end as accounts.
func TestImportRoundTripsABase64Export(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	stubStdoutTTY(t, true)
	path := filepath.Join(t.TempDir(), "backup.b64")
	if code, _, _, top := runRoot(t, "export", "--full", "--base64", "--out", path); code != ExitOK {
		t.Fatalf("export = %d (%s)", code, top)
	}

	// A second machine: a fresh store.
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad2"))

	code, _, stderr, top := runRoot(t, "import", path)
	if code != ExitOK {
		t.Fatalf("import of a base64 document = %d (%s)\nstderr: %s", code, top, stderr)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 1 {
		t.Fatalf("accounts = %+v, want the one in the document", s.Accounts())
	}
	creds, err := s.Credentials("u-1")
	if err != nil {
		t.Fatalf("the imported account has no credentials: %v", err)
	}
	if !strings.Contains(string(creds["claudeAiOauth"]), "RT-u-1") {
		t.Errorf("credentials = %s, want the exported snapshot", creds["claudeAiOauth"])
	}
}

// Every shape a base64 document arrives in, because the tool that produced it
// is not always ccdad. The wrapped row is why this is permissive at all:
// `base64` wraps at 76 columns unless it is given -w0, and a restore that
// failed over a line break is a deploy that fails where nobody can retry it by
// hand. Whitespace is not data here, so all of it is dropped.
func TestImportAcceptsEveryBase64Shape(t *testing.T) {
	document := `{"schemaVersion":1,"full":true,` +
		// This field is ignored by the reader and is here for the encoder: it
		// pushes bytes into the document that base64 renders as '+' and '/',
		// which are the two characters separating the standard alphabet from
		// the url-safe one. Without them the url rows below would be testing
		// the standard alphabet under another name.
		`"note":"￿￿￿￿",` +
		`"accounts":[{"uuid":"u-1","email":"one@example.com","kind":"subscription",` +
		`"credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`

	std := base64.StdEncoding.EncodeToString([]byte(document))
	if !strings.Contains(std, "+") || !strings.Contains(std, "/") {
		t.Fatalf("this document does not exercise the two characters that separate the alphabets:\n%s", std)
	}

	for _, tc := range []struct{ name, body string }{
		{"padded standard alphabet", std},
		{"unpadded standard alphabet", base64.RawStdEncoding.EncodeToString([]byte(document))},
		{"padded url alphabet", base64.URLEncoding.EncodeToString([]byte(document))},
		{"unpadded url alphabet", base64.RawURLEncoding.EncodeToString([]byte(document))},
		{"wrapped at 76 columns", wrapAt(std, 76)},
		// Spaces and tabs are the rows that defend the whitespace loop in
		// decodeBase64Document. Go's own decoder already skips '\r' and '\n',
		// so the wrapped row above passes with or without that loop and proves
		// nothing about it. Interior spaces are the realistic shape: an
		// unquoted `echo $VAR` collapses a wrapped blob's newlines into them,
		// and so does a YAML folded scalar.
		{"wrapped and space-joined", strings.ReplaceAll(wrapAt(std, 76), "\n", " ")},
		{"wrapped and tab-joined", strings.ReplaceAll(wrapAt(std, 76), "\n", "\t")},
		{"trailing newline", std + "\n"},
		{"leading and trailing whitespace", "  \n\t" + std + " \n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			code, _, stderr, top := runRoot(t, "import", writeImportFile(t, tc.body))
			if code != ExitOK {
				t.Fatalf("import = %d (%s)\nstderr: %s", code, top, stderr)
			}
			if got := accountCount(t); got != 1 {
				t.Fatalf("the store holds %d account(s), want the one in the document", got)
			}
		})
	}
}

// wrapAt breaks a string every n characters, the way `base64` does without -w0.
func wrapAt(s string, n int) string {
	var lines []string
	for len(s) > n {
		lines = append(lines, s[:n])
		s = s[n:]
	}
	return strings.Join(append(lines, s), "\n")
}

// '-' for stdin carries a base64 document too, which is the form that never
// touches the importing machine's disk.
func TestImportReadsBase64FromStdin(t *testing.T) {
	isolate(t)
	body := base64.StdEncoding.EncodeToString([]byte(`{"schemaVersion":1,"full":true,"accounts":[
	  {"uuid":"u-1","email":"one@example.com","kind":"subscription",
	   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`))

	root := NewRootCmd()
	root.SetIn(strings.NewReader(body + "\n"))
	root.SetArgs(explicitArgs([]string{"import", "-"}))
	var out, errOut, top bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	if code := ExecuteWith(root, &top); code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, errOut.String(), top.String())
	}
	if got := accountCount(t); got != 1 {
		t.Fatalf("the store holds %d account(s), want the one piped in", got)
	}
}

// A document that is neither form has to say so as both, because "not JSON" on
// its own sends the reader looking for a JSON mistake in a file that was never
// meant to hold JSON.
func TestImportRefusesADocumentThatIsNeitherJSONNorBase64(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "import", writeImportFile(t, "not a document, and not base64 either!"))
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitUsage, top)
	}
	if !strings.Contains(top, "base64") {
		t.Errorf("the refusal %q never mentions the other form it tried", top)
	}
}

// An empty file used to come back as "unexpected end of JSON input", which is
// the parser describing a document that is not there.
func TestImportRefusesAnEmptyDocument(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "import", writeImportFile(t, "  \n\n"))
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitUsage, top)
	}
	if !strings.Contains(top, "empty") {
		t.Errorf("the refusal %q does not say the file is empty", top)
	}
}

// TestSeatTierSurvivesTheRoundTrip pins the field that decides how an account
// is METERED across a move between machines.
//
// seat_tier is the one profile field that separates a seat billed per unit from
// one metered on a plan window, and it is what identity.PrimaryByDefault reads
// to decide whether an account may be ranked without a spend ceiling. Kind and
// Primary are both carried already, so an import without seat_tier lands an
// account that behaves correctly and can no longer SAY WHY -- and the next
// thing to re-derive either of them from the profile fields, on a machine that
// never saw the profile, gets a different answer.
func TestSeatTierSurvivesTheRoundTrip(t *testing.T) {
	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude,
		UUID: "u-1", Email: "seat@example.com",
		Kind: identity.KindCredit, Primary: true,
		Tier: "claude_enterprise", RateLimitTier: "default_claude_zero",
		SeatTier: "enterprise_usage_based",
	}, credsFor("RT-u-1")); err != nil {
		t.Fatal(err)
	}
	path := exportTo(t, "backup.json")

	// A second machine: a fresh store.
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad2"))
	if code, _, _, top := runRoot(t, "import", path); code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}

	after, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := after.Get("u-1")
	if !ok {
		t.Fatal("u-1 was not imported")
	}
	if got.SeatTier != "enterprise_usage_based" {
		t.Errorf("SeatTier = %q, want %q", got.SeatTier, "enterprise_usage_based")
	}
	// The three that already crossed, asserted here so a change that carries
	// seat_tier by dropping one of them cannot pass.
	if got.Kind != identity.KindCredit || !got.Primary || got.RateLimitTier != "default_claude_zero" {
		t.Errorf("Kind/Primary/RateLimitTier = %v/%v/%q; want credit/true/default_claude_zero",
			got.Kind, got.Primary, got.RateLimitTier)
	}
}

// INVARIANT 5 IN THE ONE PATH THAT STILL BROKE IT. store.Add replaces an
// account's credential file wholesale, so importing a document that carries
// only claudeAiOauth used to delete that account's designOauth and
// trustedDeviceToken as the price of restoring one login. The
// newer-credentials check cannot stand in for it: that compares claudeAiOauth
// and nothing else, so it never sees the keys being dropped.
func TestImportKeepsCredentialKeysTheDocumentNeverMentions(t *testing.T) {
	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	acct := store.Account{Provider: provider.Claude, UUID: "u-1", Email: "one@example.com"}
	if err := s.Add(acct, cclink.Blob{
		"claudeAiOauth":      json.RawMessage(`{"accessToken":"AT","refreshToken":"RT-old"}`),
		"designOauth":        json.RawMessage(`{"kept":true}`),
		"trustedDeviceToken": json.RawMessage(`"device-abc"`),
	}); err != nil {
		t.Fatal(err)
	}

	path := writeImportFile(t, `{
	  "schemaVersion": 1,
	  "full": true,
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT2","refreshToken":"RT-new"}}}]
	}`)
	if code, _, _, top := runRoot(t, "import", path, "--force"); code != ExitOK {
		t.Fatalf("import = %d (%s), want 0", code, top)
	}

	s2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got["claudeAiOauth"]), "RT-new") {
		t.Fatalf("the document's login did not win: %s", got["claudeAiOauth"])
	}
	for _, key := range []string{"designOauth", "trustedDeviceToken"} {
		if _, kept := got[key]; !kept {
			t.Fatalf("%s was deleted by an import that never mentioned it; keys present: %v", key, keysOf(got))
		}
	}
}

func keysOf(b cclink.Blob) []string {
	out := make([]string, 0, len(b))
	for k := range b {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
