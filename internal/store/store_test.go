package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
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
	acct := Account{UUID: "u-1", Email: "a@example.com", Kind: identity.KindSubscription, Tier: "claude_max"}
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
	if err := s.Add(Account{UUID: "u-1", Kind: identity.KindCredit}, sampleCreds("AT")); err != nil {
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
		if err := s.Add(Account{UUID: u}, sampleCreds(u)); err != nil {
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

	if err := s.Add(Account{UUID: "u-1", Email: "old@example.com"}, sampleCreds("OLD")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlias("u-1", "work"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-1", Email: "new@example.com"}, sampleCreds("NEW")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-2", Email: "old@example.com"}, sampleCreds("b")); err != nil {
		t.Fatal(err)
	}
	before, ok := s.Get("u-2")
	if !ok {
		t.Fatal("Get(u-2) = false, want the account just added")
	}

	if err := s.Add(Account{UUID: "u-2", Email: "new@example.com"}, sampleCreds("b2")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1", Email: "a@example.com"}, sampleCreds("a")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("AT")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("AT")); err != nil {
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
		if err := s.Add(Account{UUID: u}, sampleCreds(u)); err != nil {
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
	_ = s.Add(Account{UUID: "u-1"}, sampleCreds("a"))
	_ = s.Add(Account{UUID: "u-2"}, sampleCreds("b"))

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
	_ = s.Add(Account{UUID: "u-1"}, sampleCreds("a"))

	if err := s.SetAlias("u-1", "123"); !errors.Is(err, ErrBadAlias) {
		t.Fatalf("SetAlias(numeric) = %v, want ErrBadAlias", err)
	}
}

func TestSetAliasNormalizes(t *testing.T) {
	withStore(t)
	s, _ := Open()
	_ = s.Add(Account{UUID: "u-1"}, sampleCreds("a"))

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
	_ = s.Add(Account{UUID: "u-1"}, sampleCreds("a"))
	_ = s.Add(Account{UUID: "u-2"}, sampleCreds("b"))

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

	if err := s.Add(Account{UUID: "../../pwned"}, sampleCreds("AT")); err == nil {
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

	if err := s.Add(Account{UUID: ""}, sampleCreds("AT")); err == nil {
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

	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("AT")); err == nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("AT")); err != nil {
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

// §6.5 makes `ccdad add` double as re-authentication, and the record a fresh
// login builds knows nothing about the user having held this account out of
// auto-rotation. Idx, Alias and AddedAt were pinned; Disabled was not, so
// dropping its carry silently returned a held-out account to the pool the next
// time its owner logged in again.
func TestReAuthenticationKeepsAnAccountHeldOutOfRotation(t *testing.T) {
	withStore(t)
	s, _ := Open()
	if err := s.Add(Account{UUID: "u-1", Email: "old@example.com", Disabled: true}, sampleCreds("a")); err != nil {
		t.Fatal(err)
	}

	if err := s.Add(Account{UUID: "u-1", Email: "new@example.com"}, sampleCreds("b")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("a")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("a")); err != nil {
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
	if err := s.Add(Account{UUID: "u-1"}, sampleCreds("a")); err != nil {
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
