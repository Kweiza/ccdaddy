package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

func withStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ccdad")
	t.Setenv("CCDAD_HOME", dir)
	return dir
}

func sampleCreds(token string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"` + token + `"}`)}
}

// skipIfPermissionsAreMeaningless guards the tests that make a directory
// unwritable: root ignores the mode bits, and Windows has no equivalent.
func skipIfPermissionsAreMeaningless(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
}

func TestOpenEmptyStore(t *testing.T) {
	withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatalf("Open() = %v, want nil", err)
	}
	if got := s.Accounts(); len(got) != 0 {
		t.Fatalf("Accounts() = %v, want empty", got)
	}
}

func TestAddPersistsAcrossReopen(t *testing.T) {
	withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	acct := Account{Provider: provider.Claude, UUID: "u-1", Email: "a@example.com", Kind: identity.KindSubscription, Tier: "claude_max"}
	if err := s.Add(acct, sampleCreds("AT")); err != nil {
		t.Fatalf("Add() = %v, want nil", err)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Accounts()
	if len(got) != 1 {
		t.Fatalf("Accounts() = %v, want one account", got)
	}
	if got[0].UUID != "u-1" || got[0].Email != "a@example.com" {
		t.Fatalf("account = %+v", got[0])
	}
	if got[0].Tier != "claude_max" {
		t.Fatalf("Tier = %q, want claude_max", got[0].Tier)
	}
	if got[0].Idx != 1 {
		t.Fatalf("Idx = %d, want 1 for the first account", got[0].Idx)
	}
	if got[0].AddedAt.IsZero() {
		t.Fatal("AddedAt is zero, want the time the account was first stored")
	}
}

// KindCredit, not KindSubscription: KindSubscription is the zero value, so
// asserting on it would also pass when nothing is persisted at all.
func TestKindSurvivesTheTOMLRoundTrip(t *testing.T) {
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Kind: identity.KindCredit}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Accounts()[0].Kind; got != identity.KindCredit {
		t.Fatalf("Kind = %v, want credit", got)
	}
}

// An accounts.toml written before the kind field existed must read as a
// subscription — the side that does not spend money.
func TestAnAccountWithNoStoredKindReadsAsSubscription(t *testing.T) {
	dir := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	legacy := "version = 1\n\n[[accounts]]\nuuid = \"u-1\"\nemail = \"a@example.com\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(dir, accountsFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open()
	if err != nil {
		t.Fatalf("Open() = %v, want it to read an accounts.toml with no kind field", err)
	}
	if got := s.Accounts()[0].Kind; got != identity.KindSubscription {
		t.Fatalf("Kind = %v, want subscription", got)
	}
}

func TestAddAssignsSequentialIndices(t *testing.T) {
	withStore(t)
	s, _ := Open()

	for _, u := range []string{"u-1", "u-2", "u-3"} {
		if err := s.Add(Account{Provider: provider.Claude, UUID: u}, sampleCreds(u)); err != nil {
			t.Fatal(err)
		}
	}
	for i, a := range s.Accounts() {
		if a.Idx != i+1 {
			t.Fatalf("account %d has Idx %d, want %d", i, a.Idx, i+1)
		}
	}
}

// Re-adding the same uuid updates it in place rather than creating a duplicate.
// This is what makes `ccdad add` double as re-authentication.
func TestAddSameUUIDUpdatesInPlace(t *testing.T) {
	withStore(t)
	s, _ := Open()

	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "old@example.com"}, sampleCreds("OLD")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlias("u-1", "work"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "new@example.com"}, sampleCreds("NEW")); err != nil {
		t.Fatal(err)
	}

	got := s.Accounts()
	if len(got) != 1 {
		t.Fatalf("Accounts() = %d accounts, want 1", len(got))
	}
	if got[0].Email != "new@example.com" {
		t.Fatalf("Email = %q, want the refreshed value", got[0].Email)
	}
	// The alias and the index are the user's, not the login's; they survive.
	if got[0].Alias != "work" {
		t.Fatalf("Alias = %q, want it to survive re-authentication", got[0].Alias)
	}
	if got[0].Idx != 1 {
		t.Fatalf("Idx = %d, want it to survive re-authentication", got[0].Idx)
	}

	creds, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(creds["claudeAiOauth"]) != `{"accessToken":"NEW"}` {
		t.Fatalf("credentials = %s, want the refreshed token", creds["claudeAiOauth"])
	}
}

// With a second account present, an Idx that is not preserved comes back as the
// zero value and sorts ahead of account 1 — so this catches a re-add that
// silently renumbers the store. AddedAt is the same kind of hole: it is only
// stamped on first insert, so dropping its preservation leaves it zero.
func TestReAuthenticationKeepsTheIndexAndTheAddedAtTime(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-2", Email: "old@example.com"}, sampleCreds("b")); err != nil {
		t.Fatal(err)
	}
	before, ok := s.Get("u-2")
	if !ok {
		t.Fatal("Get(u-2) = false, want the account just added")
	}

	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-2", Email: "new@example.com"}, sampleCreds("b2")); err != nil {
		t.Fatal(err)
	}

	after, _ := s.Get("u-2")
	if after.Idx != before.Idx {
		t.Fatalf("Idx = %d after re-authentication, want %d", after.Idx, before.Idx)
	}
	if !after.AddedAt.Equal(before.AddedAt) {
		t.Fatalf("AddedAt = %v after re-authentication, want the original %v", after.AddedAt, before.AddedAt)
	}
	if first, _ := s.Get("u-1"); first.Idx != 1 {
		t.Fatalf("account u-1 has Idx %d after an unrelated re-authentication, want 1", first.Idx)
	}
}

// Accounts() hands out a copy: a caller that sorts or edits the result must not
// be reaching into the store's own slice.
func TestAccountsReturnsACopy(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "a@example.com"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	got := s.Accounts()
	got[0].Email = "tampered@example.com"

	if again := s.Accounts(); again[0].Email != "a@example.com" {
		t.Fatalf("Email = %q, want the store to be unaffected by a caller's edit", again[0].Email)
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	got, err := s.Credentials("u-1")
	if err != nil {
		t.Fatalf("Credentials() = %v, want nil", err)
	}
	if string(got["claudeAiOauth"]) != `{"accessToken":"AT"}` {
		t.Fatalf("credentials = %s", got["claudeAiOauth"])
	}
}

func TestCredentialsFileIsSixHundred(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	dir := withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, credentialsDir, "u-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credential file mode = %o, want 600", got)
	}
	// accounts.toml carries no tokens, but it does carry every managed
	// account's email, uuid and organization.
	tomlInfo, err := os.Stat(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := tomlInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("accounts.toml mode = %o, want 600", got)
	}
}

// MkdirAll only sets the mode on directories it creates, so a store restored
// from a backup or made by an older build can be looser than 0700. Both
// directories must be tightened on every open, including credentials/ — the
// only one that holds live tokens.
func TestOpenTightensAPreexistingLooseStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	dir := withStore(t)
	creds := filepath.Join(dir, credentialsDir)
	if err := os.MkdirAll(creds, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, creds} {
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, creds} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %o after Open(), want 700", d, got)
		}
	}
}

func TestRemoveDeletesCredentialsAndCompactsIndices(t *testing.T) {
	dir := withStore(t)
	s, _ := Open()
	for _, u := range []string{"u-1", "u-2", "u-3"} {
		if err := s.Add(Account{Provider: provider.Claude, UUID: u}, sampleCreds(u)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Remove("u-2"); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, credentialsDir, "u-2.json")); !os.IsNotExist(err) {
		t.Fatal("Remove() left the credential file behind")
	}

	got := s.Accounts()
	if len(got) != 2 {
		t.Fatalf("Accounts() = %d, want 2", len(got))
	}
	for i, a := range got {
		if a.Idx != i+1 {
			t.Fatalf("after removal account %q has Idx %d, want %d", a.UUID, a.Idx, i+1)
		}
	}
}

func TestRemoveUnknownIsNotFound(t *testing.T) {
	withStore(t)
	s, _ := Open()

	if err := s.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove(unknown) = %v, want ErrNotFound", err)
	}
}

func TestSetAliasRejectsDuplicates(t *testing.T) {
	withStore(t)
	s, _ := Open()
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a"))
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-2"}, sampleCreds("b"))

	if err := s.SetAlias("u-1", "work"); err != nil {
		t.Fatal(err)
	}
	err := s.SetAlias("u-2", "work")
	if !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("SetAlias(duplicate) = %v, want ErrAliasTaken", err)
	}
	// idx is not a key, so the message names something durable instead.
	if !strings.Contains(err.Error(), "u-1") {
		t.Fatalf("error %q should name the account that holds the alias", err)
	}
}

func TestSetAliasValidates(t *testing.T) {
	withStore(t)
	s, _ := Open()
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a"))

	if err := s.SetAlias("u-1", "123"); !errors.Is(err, ErrBadAlias) {
		t.Fatalf("SetAlias(numeric) = %v, want ErrBadAlias", err)
	}
}

func TestSetAliasNormalizes(t *testing.T) {
	withStore(t)
	s, _ := Open()
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a"))

	if err := s.SetAlias("u-1", "  WORK  "); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("u-1"); got.Alias != "work" {
		t.Fatalf("Alias = %q, want the normalized form", got.Alias)
	}
}

func TestSetAliasOnAnUnknownAccountIsNotFound(t *testing.T) {
	withStore(t)
	s, _ := Open()

	if err := s.SetAlias("nope", "work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAlias(unknown) = %v, want ErrNotFound", err)
	}
}

func TestActiveUUIDPersistsAndIsClearedOnRemoval(t *testing.T) {
	withStore(t)
	s, _ := Open()
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a"))
	_ = s.Add(Account{Provider: provider.Claude, UUID: "u-2"}, sampleCreds("b"))

	if err := s.SetActive("u-1"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ActiveUUID(); got != "u-1" {
		t.Fatalf("ActiveUUID() = %q, want u-1 after a reopen", got)
	}

	if err := reopened.Remove("u-1"); err != nil {
		t.Fatal(err)
	}
	if got := reopened.ActiveUUID(); got != "" {
		t.Fatalf("ActiveUUID() = %q after removing that account, want it cleared", got)
	}
}

// The uuid arrives from an HTTP response body and becomes a path component, so
// a traversal sequence in it would write a credential file outside the store.
func TestAddRejectsAUUIDThatWouldEscapeTheStore(t *testing.T) {
	dir := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Add(Account{Provider: provider.Claude, UUID: "../../pwned"}, sampleCreds("AT")); err == nil {
		t.Fatal("Add(traversal uuid) = nil, want an error")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "pwned.json")); err == nil {
		t.Fatal("Add() wrote a credential file outside the store")
	}
	if got := s.Accounts(); len(got) != 0 {
		t.Fatalf("Accounts() = %v, want the rejected account not to be stored", got)
	}
}

// A hand-edited or corrupt accounts.toml must not be able to make Credentials
// read an arbitrary file either. The escape target is a real, readable blob, so
// this fails if the charset check is removed — asserting only "some error" would
// pass on a path that simply does not exist.
func TestCredentialsRejectsATraversalUUID(t *testing.T) {
	dir := withStore(t)
	s, _ := Open()

	decoy, err := json.Marshal(sampleCreds("SOMEONE-ELSES-TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "escape.json"), decoy, 0o600); err != nil {
		t.Fatal(err)
	}

	// <root>/credentials/../escape.json is <root>/escape.json.
	if _, err := s.Credentials("../escape"); err == nil {
		t.Fatal("Credentials(traversal uuid) = nil, want it refused before the read")
	}
}

func TestAddIsEmptyUUIDRejected(t *testing.T) {
	withStore(t)
	s, _ := Open()

	if err := s.Add(Account{Provider: provider.Claude, UUID: ""}, sampleCreds("AT")); err == nil {
		t.Fatal("Add(no uuid) = nil, want an error")
	}
}

// A failed credential write must leave nothing behind in memory: the next
// A save() from any other call would otherwise persist an account whose tokens
// were never written.
func TestAddDoesNotRecordAnAccountWhoseCredentialsFailedToWrite(t *testing.T) {
	skipIfPermissionsAreMeaningless(t)
	dir := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, credentialsDir)
	if err := os.Chmod(creds, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(creds, 0o700) })

	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT")); err == nil {
		t.Fatal("Add() = nil, want the unwritable credentials directory to fail it")
	}
	if got := s.Accounts(); len(got) != 0 {
		t.Fatalf("Accounts() = %v, want no account recorded after a failed write", got)
	}
}

// The mirror image: a failed credential deletion must leave the account in
// place, or the next save() drops it from accounts.toml while its credential
// file survives as an orphan holding a live token.
func TestRemoveKeepsTheAccountWhenTheCredentialFileCannotBeDeleted(t *testing.T) {
	skipIfPermissionsAreMeaningless(t)
	dir := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, credentialsDir)
	if err := os.Chmod(creds, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(creds, 0o700) })

	if err := s.Remove("u-1"); err == nil {
		t.Fatal("Remove() = nil, want the undeletable credential file to fail it")
	}
	if got := s.Accounts(); len(got) != 1 {
		t.Fatalf("Accounts() = %v, want the account kept after a failed deletion", got)
	}
}

// The charset check stops the reachable attack (a uuid from the profile
// endpoint becoming a path component), but a hand-edited accounts.toml can
// still name a Windows device. "CON.json" is the device CON, not a file, and
// opening it behaves in ways no error path here expects.
func TestValidUUIDRefusesWindowsDeviceNames(t *testing.T) {
	for _, uuid := range []string{"CON", "con", "NUL", "aux", "COM1", "lpt9", "PRN"} {
		if err := ValidateUUID(uuid); err == nil {
			t.Errorf("ValidateUUID(%q) = nil, want it refused as a reserved device name", uuid)
		}
	}
	// The names the store actually uses must keep working.
	for _, uuid := range []string{"u-1", "console", "com", "com10", "aaaaaaaa-1111-2222-3333-444444444444"} {
		if err := ValidateUUID(uuid); err != nil {
			t.Errorf("ValidateUUID(%q) = %v, want nil", uuid, err)
		}
	}
}

// Label picks the account's best human name, and every error message and table
// row in the CLI goes through it. The precedence was never asserted, so a
// reordering would rename accounts everywhere with the suite green.
func TestAccountLabelPrecedence(t *testing.T) {
	long := "aaaaaaaa-1111-2222-3333-444444444444"
	cases := []struct {
		name string
		acct Account
		want string
	}{
		{"alias wins", Account{Alias: "work", Email: "a@example.com", UUID: long}, "work"},
		{"email when there is no alias", Account{Email: "a@example.com", UUID: long}, "a@example.com"},
		{"a short uuid when there is neither", Account{UUID: long}, "aaaaaaaa"},
		{"a uuid too short to truncate is used whole", Account{UUID: "u-1"}, "u-1"},
	}
	for _, tc := range cases {
		if got := tc.acct.Label(); got != tc.want {
			t.Errorf("%s: Label() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A relative store puts live tokens in whatever directory ccdad happened to be
// run from, a different one each time. This is the CCDAD_HOME-is-relative half;
// the home-cannot-be-resolved half is the test below it.
func TestOpenRefusesARelativeStoreRoot(t *testing.T) {
	t.Setenv("CCDAD_HOME", filepath.Join("relative", "ccdad"))

	_, err := Open()
	if err == nil {
		t.Fatal("Open() = nil, want a relative store root refused")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %q, want it to say what to do", err)
	}
}

// The other half of the same hazard, and the one that used to be silent: with
// no home directory and no CCDAD_HOME, ccpath used to hand back the RELATIVE
// path ".ccdad" and Open would happily create a credential tree inside the
// current working directory. The error has to arrive from ccpath rather than
// from Open's own absolute-path guard, so this asserts on the sentence that
// names the variable to set — the absolute-path message would be a wrong
// diagnosis here, since nothing about CCDAD_HOME is at fault.
func TestOpenRefusesWhenTheHomeDirectoryCannotBeResolved(t *testing.T) {
	// If the refusal regresses, whatever gets created lands in a temp directory
	// rather than in internal/store/.
	t.Chdir(t.TempDir())
	t.Setenv("CCDAD_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	} else {
		t.Setenv("HOME", "")
	}

	_, err := Open()
	if err == nil {
		t.Fatal("Open() = nil; want a refusal when the home directory cannot be resolved")
	}
	// Asserted against a sentence only ccpath's error carries. Open's own
	// absolute-path guard ALSO fires here — filepath.IsAbs("") is false — and
	// its message also names CCDAD_HOME, so an assertion on that name alone
	// passes whether or not Open propagates ccpath's error at all. The
	// diagnosis is the difference that matters: nothing about CCDAD_HOME is
	// wrong on this machine, and telling the user to make it absolute sends
	// them to fix a variable they never set.
	if !strings.Contains(err.Error(), "cannot tell where your home directory is") {
		t.Fatalf("error = %q, want ccpath's diagnosis rather than the relative-path one", err)
	}
	if _, serr := os.Stat(".ccdad"); serr == nil {
		t.Fatal("Open() created a relative .ccdad store in the working directory")
	}
}

// `ccdad add` doubles as re-authentication in place, and the record a fresh
// login builds knows nothing about the user having held this account out of
// auto-rotation. Idx, Alias and AddedAt were pinned; Disabled was not, so
// dropping its carry silently returned a held-out account to the pool the next
// time its owner logged in again.
func TestReAuthenticationKeepsAnAccountHeldOutOfRotation(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "old@example.com", Disabled: true}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "new@example.com"}, sampleCreds("b")); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("u-1")
	if !ok {
		t.Fatal("Get(u-1) = false, want the re-authenticated account")
	}
	if got.Email != "new@example.com" {
		t.Fatalf("Email = %q, want the refreshed value", got.Email)
	}
	if !got.Disabled {
		t.Fatal("Disabled = false after re-authentication, want the account still held out of rotation")
	}
}

// primary decides whether the engine ranks an account beside the subscriptions
// and whether the credit ceiling gates it at all. It is a fact about the
// account rather than a tuning value, so it lives in accounts.toml and has to
// come back the same on the next Open.
func TestPrimaryIsWrittenAndReadBack(t *testing.T) {
	dir := withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Kind: identity.KindCredit}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	changed, err := s.SetPrimary("u-1", true)
	if err != nil {
		t.Fatalf("SetPrimary = %v, want nil", err)
	}
	if !changed {
		t.Fatal("SetPrimary reported no change on an account that was not primary")
	}
	if again, err := s.SetPrimary("u-1", true); err != nil || again {
		t.Fatalf("SetPrimary twice = (%v, %v), want (false, nil) — the world is already as asked", again, err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "accounts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "primary = true") {
		t.Fatalf("accounts.toml does not carry the flag:\n%s", raw)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get("u-1"); !got.Primary {
		t.Fatal("the flag did not survive a reopen")
	}
}

// An ordinary account writes no key at all, which is what every other optional
// field's omitempty tag buys: a file full of `primary = false` lines invites a
// reader to think the absence of one somewhere means something different.
func TestAnOrdinaryAccountWritesNoPrimaryKey(t *testing.T) {
	dir := withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "accounts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "primary") {
		t.Fatalf("accounts.toml carries a primary key for an account with no such flag:\n%s", raw)
	}
}

// `ccdad add` doubles as re-authentication in place, and the record a fresh
// login builds knows nothing about the user having armed this seat. Without the
// carry, logging in again puts the account back behind credit.max_auto_spend —
// which defaults to 0, so the account becomes permanently unusable and nothing
// says why.
func TestReAuthenticationKeepsAnAccountPrimary(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "old@example.com", Primary: true}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1", Email: "new@example.com"}, sampleCreds("b")); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("u-1")
	if !ok {
		t.Fatal("Get(u-1) = false, want the re-authenticated account")
	}
	if got.Email != "new@example.com" {
		t.Fatalf("Email = %q, want the refreshed value", got.Email)
	}
	if !got.Primary {
		t.Fatal("Primary = false after re-authentication, want the seat still armed")
	}
}

// An account with no credential file is a broken store, not an account with no
// credentials: returning an empty blob and no error would hand a caller a
// snapshot with no OAuth record in it, which the switcher reads as a credential
// that cannot identify itself.
func TestCredentialsRefusesAnAccountWithNoStoredFile(t *testing.T) {
	withStore(t)
	s, _ := Open()

	got, err := s.Credentials("u-1")
	if err == nil {
		t.Fatalf("Credentials(missing) = %v, nil, want it refused", got)
	}
	if !strings.Contains(err.Error(), "no stored credentials") {
		t.Fatalf("err = %v, want it to name what is missing", err)
	}
}

// The same for a file that exists and is not JSON. Silently treating it as
// empty would let a corrupted credential be overwritten by a merge that thought
// there was nothing to preserve.
func TestCredentialsRefusesACorruptFile(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.credentialPath("u-1"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Credentials("u-1")
	if err == nil {
		t.Fatalf("Credentials(corrupt) = %v, nil, want it refused", got)
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want it to say the stored record is corrupt", err)
	}
}

// SetAlias documents an empty alias as the way to clear one, and that is the
// only way back for a user who mistyped a handle they now cannot re-use
// elsewhere: a duplicate is refused against the account still holding it.
func TestSetAliasClearsWithAnEmptyValue(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlias("u-1", "work"); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAlias("u-1", ""); err != nil {
		t.Fatalf("SetAlias(empty) = %v, want it to clear the alias", err)
	}
	if got, _ := s.Get("u-1"); got.Alias != "" {
		t.Fatalf("Alias = %q, want it cleared", got.Alias)
	}
}

// The version field is what lets a later release migrate rather than guess, and
// it is only worth anything if it is actually written: a document that reads
// back as version 0 is indistinguishable from one an unversioned build wrote.
func TestAccountsFileCarriesItsSchemaVersion(t *testing.T) {
	dir := withStore(t)
	s, _ := Open()
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "accounts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc file
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing accounts.toml: %v\n%s", err, raw)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1: %s", doc.Version, raw)
	}
}

// AccountsAt exists so `ccdad doctor` can have the account list without Open's
// MkdirAll. A diagnostic that manufactures the store it is reporting on is
// worthless, and this is the property that keeps it from doing so.
func TestAccountsAtCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-created")

	accounts, err := AccountsAt(root)
	if err != nil {
		t.Fatalf("AccountsAt on a store that is not there: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("got %d accounts out of a store that does not exist", len(accounts))
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("AccountsAt created the store directory it was asked to read: %v", err)
	}
}

// And it reads the same accounts Open would, so a caller that has to avoid Open
// is not getting a different answer from the rest of the tree.
func TestAccountsAtReadsWhatOpenWrote(t *testing.T) {
	root := withStore(t)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "uuid-a", Email: "a@example.com"}, sampleCreds("AT-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "uuid-b", Email: "b@example.com"}, sampleCreds("AT-b")); err != nil {
		t.Fatal(err)
	}

	got, err := AccountsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	want := s.Accounts()
	if len(got) != len(want) {
		t.Fatalf("AccountsAt returned %d accounts, Open %d", len(got), len(want))
	}
	for i := range want {
		if got[i].UUID != want[i].UUID || got[i].Email != want[i].Email {
			t.Errorf("account %d = %+v, Open says %+v", i, got[i], want[i])
		}
	}
}

// A damaged accounts.toml must be an error rather than an empty list: doctor
// differences the profiles against this, and "no accounts" out of a broken read
// would report every profile on the machine as an orphan.
func TestAccountsAtRefusesADamagedAccountsFile(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("this is not toml{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := AccountsAt(root); err == nil {
		t.Error("AccountsAt read a damaged accounts.toml as an empty store")
	}
}

// The credential rollback. These four sit here rather than in lock_test.go
// because what they pin is the credentials directory, not the lock: mutate's
// failure path restores accounts.toml by never writing it, and until the
// journal in store.go that guarantee stopped at the document.
//
// A batch refused on the fourth of five accounts left the first three
// credential files on disk with no accounts.toml naming them — each holding a
// live refresh token, each invisible to `ccdad status`, `ccdad remove` and
// `ccdad doctor`, which all read the document.
func TestAFailedTransactionRemovesTheCredentialFileItCreated(t *testing.T) {
	dir := withStore(t)

	seed, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err = WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-2"}, sampleCreds("AT-2")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error", err)
	}

	orphan := filepath.Join(dir, credentialsDir, "u-2.json")
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("a refused transaction left a live refresh token at %s with no account naming it", orphan)
	}
	kept := filepath.Join(dir, credentialsDir, "u-1.json")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the rollback took the credentials of an account it never touched: %v", err)
	}
}

// The other half of the same guarantee, and the direction that logs a user
// out: remove deletes the credential file BEFORE it drops the account, so a
// transaction that fails afterwards leaves accounts.toml naming an account
// whose credentials are gone.
func TestAFailedTransactionPutsBackTheCredentialFileItDeleted(t *testing.T) {
	dir := withStore(t)

	seed, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, credentialsDir, "u-1.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err = WithStore(func(s *Store) error {
		if err := s.Remove("u-1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the account is still in accounts.toml but its credentials are gone: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("restored credentials = %s, want %s", after, before)
	}
}

// The deliberate exception, and it is not an oversight. `ccdad run` carries a
// session's refreshed claudeAiOauth back into the store through Add on every
// ordinary run, so the bytes an overwrite replaced are a login the provider
// has already rotated away. Putting them back would trade a leak nobody has
// for an account that can no longer authenticate — and the account is still
// named by accounts.toml either way, so nothing is hidden and nothing is
// missing.
func TestAFailedTransactionKeepsCredentialsItOnlyOverwrote(t *testing.T) {
	dir := withStore(t)

	seed, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-OLD")); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err = WithStore(func(s *Store) error {
		acct, ok := s.Get("u-1")
		if !ok {
			return errors.New("fixture: the seeded account is not there")
		}
		if err := s.Add(acct, sampleCreds("AT-NEW")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, credentialsDir, "u-1.json"))
	if err != nil {
		t.Fatalf("the rollback removed a file it had only overwritten: %v", err)
	}
	if !strings.Contains(string(raw), "AT-NEW") {
		t.Errorf("stored credentials = %s, want the newer login kept", raw)
	}
}

// The callback is not the only way a transaction fails. save() writes
// accounts.toml last, so an unwritable store root refuses the document AFTER
// every credential file is already down — which is the shape ENOSPC has, and
// this machine's disk is the reason the item exists.
func TestAFailedSaveRemovesTheCredentialFileItCreated(t *testing.T) {
	skipIfPermissionsAreMeaningless(t)
	dir := withStore(t)

	if _, err := Open(); err != nil {
		t.Fatal(err)
	}

	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		// Only the root: the credentials directory stays writable, so the
		// rollback can still act while the document cannot be written.
		return os.Chmod(dir, 0o500)
	})
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err == nil {
		t.Fatal("WithStore() = nil, want the unwritable store root to fail the save")
	}

	orphan := filepath.Join(dir, credentialsDir, "u-1.json")
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Errorf("a transaction whose save failed left a live refresh token at %s: %v", orphan, err)
	}
}

// A reversal that did not happen is the leak still being there, so it is part
// of the answer rather than a detail swallowed on the way out.
func TestRollbackSaysWhichCredentialFileItCouldNotRemove(t *testing.T) {
	skipIfPermissionsAreMeaningless(t)
	dir := withStore(t)

	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, credentialsDir)
	t.Cleanup(func() { _ = os.Chmod(creds, 0o700) })

	boom := errors.New("boom")
	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		if err := os.Chmod(creds, 0o500); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error kept", err)
	}
	orphan := filepath.Join(creds, "u-1.json")
	if !strings.Contains(err.Error(), orphan) {
		t.Errorf("WithStore() = %v, want it to name the file it could not remove (%s)", err, orphan)
	}
}

// The in-memory half of the same guarantee. add appends to the slice once the
// credential file is down, so a transaction that fails after that point leaves
// THIS PROCESS naming an account accounts.toml does not have — and the next
// mutate is the only thing that would have cleared it. A Store outlives one
// transaction in `ccdad run` and in the daemon, and a phantom account there is
// a switch onto credentials the rollback has just deleted.
func TestAFailedTransactionLeavesNoPhantomAccountInThisProcess(t *testing.T) {
	withStore(t)

	boom := errors.New("boom")
	var captured *Store
	err := WithStore(func(s *Store) error {
		captured = s
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithStore() = %v, want the callback's error", err)
	}
	if got := captured.Accounts(); len(got) != 0 {
		t.Errorf("Accounts() = %+v, want none: a refused transaction left this process holding an account the document does not have", got)
	}
}

// The two path accessors exist so `ccdad doctor` can name these files without
// respelling the naming rule: checkPermissions used to spell both out and its
// comment said why — "store exports no path accessors and opening it would
// create the tree". They are pinned against what Open actually writes, because
// an accessor that agreed only with itself would be the same gap under a new
// name.
func TestThePathAccessorsNameWhatOpenWrites(t *testing.T) {
	root := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "uuid-a", Email: "work@example.com"}, sampleCreds("AT-a")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(CredentialsDirAt(root), "uuid-a.json")); err != nil {
		t.Errorf("CredentialsDirAt does not name the directory Add wrote into: %v", err)
	}
	if _, err := os.Stat(AccountsFileAt(root)); err != nil {
		t.Errorf("AccountsFileAt does not name the document save wrote: %v", err)
	}
}

// OrphanCredentialsAt is doctor's only route into the credentials directory,
// and it follows AccountsAt's rule rather than Open's: a store that is not
// there yields nothing rather than being brought into existence.
func TestOrphanCredentialsAtCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-created")

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatalf("OrphanCredentialsAt on a store that is not there: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none from a store that does not exist", orphans)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("OrphanCredentialsAt created the store directory it was asked to read: %v", err)
	}
}

// mutate's own ordering — Add writes a credential file before the document
// names it, and re-reads the document inside the lock precisely because a
// write can land in between — is the same window a lockless read tears: the
// listing sees u-1's file, the document has not been re-read to see u-1's
// account yet, and a probe caught in that instant used to call the file an
// orphan. Held under the same lock a write takes, the instant cannot be
// observed: OrphanCredentialsAt waits for the transaction to either commit
// (asserted here) or reverse, and answers from whichever the lock's release
// actually left.
func TestOrphanCredentialsAtWaitsOutAnInFlightAddRatherThanTearingItsRead(t *testing.T) {
	root := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "u-0", Email: "a@example.com"}, sampleCreds("AT-u-0")); err != nil {
		t.Fatal(err)
	}

	// The exact moment mutate can leave an Add in: u-1's credential file is on
	// disk and the document does not name u-1 yet. The lock stands in for the
	// span mutate would hold across writing both.
	if err := os.WriteFile(filepath.Join(CredentialsDirAt(root), "u-1.json"),
		[]byte(`{"claudeAiOauth":{"accessToken":"AT-u-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLock(filepath.Join(root, lockFileName))
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		orphans []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		orphans, err := OrphanCredentialsAt(root)
		done <- result{orphans, err}
	}()

	// A real chance to reach acquireLock and block on it, so a race where the
	// goroutine reads before the lock is even taken cannot pass by luck.
	time.Sleep(20 * time.Millisecond)

	// The commit: the document now names u-1, exactly as Add's own save would
	// leave it, and only then is the lock released.
	doc := file{Version: 1, Accounts: []Account{
		{UUID: "u-0", Email: "a@example.com"},
		{UUID: "u-1", Email: "b@example.com"},
	}}
	encoded, err := toml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AccountsFileAt(root), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("OrphanCredentialsAt: %v", r.err)
		}
		if len(r.orphans) != 0 {
			t.Errorf("orphans = %v, want none — u-1's credential file was read as an orphan mid-transaction, "+
				"before the document that names it was committed", r.orphans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OrphanCredentialsAt never returned after the lock was released — it did not wait for it at all")
	}
}

// The leak: a credential file a refused transaction left behind on a build
// older than the rollback journal, holding a live refresh token at 0600 that
// accounts.toml does not name. `ccdad status`, `ccdad remove` and doctor's
// account rows all read the document, so an orphan is invisible to every one
// of them, indefinitely.
func TestOrphanCredentialsAtNamesACredentialFileNoAccountHas(t *testing.T) {
	root := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{Provider: provider.Claude, UUID: "uuid-a", Email: "work@example.com"}, sampleCreds("AT-a")); err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(root, "credentials", "uuid-gone.json")
	if err := os.WriteFile(leaked, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != leaked {
		t.Errorf("orphans = %v, want exactly [%s] — uuid-a is named by the document", orphans, leaked)
	}
}

// An interrupted atomic write leaves a temp file beside the one it was
// replacing. The fixture is named through cclink.TempPattern rather than
// spelled out, which is what keeps "reachable rather than hypothetical" true:
// a hand-written name stands for the writer's output only until the writer
// changes, and nothing would report that it had stopped standing for anything.
// The stem is not a uuid, and reporting it as one would send the user looking
// for an account that never existed. The rule is a file named exactly
// <uuid>.json.
func TestOrphanCredentialsAtIgnoresAnInterruptedAtomicWrite(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "credentials", strings.Replace(cclink.TempPattern("uuid-a.json"), "*", "1234", 1))
	if err := os.WriteFile(scratch, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none — a half-written temporary file is not an account", orphans)
	}
}

// Answering "nothing is orphaned" out of a read that failed is exactly the
// reassuring lie this is built to remove, and it is the rule checkProfiles and
// checkPrimary already state one layer up.
func TestOrphanCredentialsAtRefusesADamagedAccountsFile(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("this is not toml"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OrphanCredentialsAt(root); err == nil {
		t.Error("OrphanCredentialsAt answered from an accounts.toml it could not parse")
	}
}

// A DirEntry does not follow a symlink, so a name ending in .json that is not a
// file at all still reads as one here. Every shape below is reachable in a
// directory the user can write to, and the sentence doctor builds from this
// list says the path holds a live refresh token at 0600 and can be deleted --
// which about a dangling link is false twice over. checkPermissions walks the
// same directory with os.Stat for exactly this reason.
func TestOrphanCredentialsAtReportsOnlyFilesThatAreReallyThere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege the test runner may not hold on Windows")
	}
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "credentials")
	if err := os.Symlink(filepath.Join(t.TempDir(), "no-such-target"), filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "alinkedcdir.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "aplaindir.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none — not one of those is a credential file holding a token", orphans)
	}
}

// The docstring calls the order a contract, and doctor joins the list into one
// sentence: an unstable order makes two runs of a report meant to diff cleanly
// disagree for no reason at all.
func TestOrphanCredentialsAtSortsWhatItReturns(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "credentials")
	for _, name := range []string{"uuid-c.json", "uuid-a.json", "uuid-b.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		filepath.Join(dir, "uuid-a.json"),
		filepath.Join(dir, "uuid-b.json"),
		filepath.Join(dir, "uuid-c.json"),
	}, "|")
	if got := strings.Join(orphans, "|"); got != want {
		t.Errorf("orphans =\n%s\nwant\n%s", got, want)
	}
}

// A file called exactly `.json` has no uuid in front of it. Reporting one would
// name an account that cannot exist, in a sentence telling the user to go and
// look for it.
func TestOrphanCredentialsAtIgnoresAFileWithNothingInFrontOfJSON(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "credentials", ".json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none — there is no uuid in front of that suffix", orphans)
	}
}

// `uuid-x.JSON` is a file the store itself reads back on macOS and Windows,
// where the filesystem ignores case: credentialPath asks for uuid-x.json and
// gets handed this. Skipping it there would leave a live credential file this
// row can never name. The suffix test is case-insensitive on EVERY platform,
// for the reason ValidateUUID checks Windows device names on every platform —
// a store is one rsync away from being opened somewhere else.
func TestOrphanCredentialsAtNamesAnUppercaseSuffixToo(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(root, "credentials", "uuid-x.JSON")
	if err := os.WriteFile(upper, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	orphans, err := OrphanCredentialsAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != upper {
		t.Errorf("orphans = %v, want exactly [%s]", orphans, upper)
	}
}
