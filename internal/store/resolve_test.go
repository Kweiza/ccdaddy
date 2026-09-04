package store

import (
	"errors"
	"strings"
	"testing"
)

func fixture() []Account {
	return []Account{
		{UUID: "aaaaaaaa-1111-2222-3333-444444444444", Email: "work@example.com", Alias: "work", Idx: 1},
		{UUID: "bbbbbbbb-1111-2222-3333-444444444444", Email: "personal@example.com", Idx: 2},
		{UUID: "cccccccc-1111-2222-3333-444444444444", Email: "personal@example.com", Idx: 3},
	}
}

func TestResolveByIndex(t *testing.T) {
	got, err := Resolve(fixture(), "2")
	if err != nil {
		t.Fatalf("Resolve(\"2\") = %v, want nil", err)
	}
	if got.Idx != 2 {
		t.Fatalf("Idx = %d, want 2", got.Idx)
	}
}

func TestResolveByAliasIsCaseInsensitive(t *testing.T) {
	got, err := Resolve(fixture(), "WORK")
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "work" {
		t.Fatalf("Alias = %q, want work", got.Alias)
	}
}

func TestResolveByEmail(t *testing.T) {
	got, err := Resolve(fixture(), "work@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Idx != 1 {
		t.Fatalf("Idx = %d, want 1", got.Idx)
	}
}

// A repeated email is a hard error naming every candidate, never a prompt:
// `ccdad run` ends in an exec and a caller needs determinism.
func TestResolveAmbiguousEmailIsAnError(t *testing.T) {
	_, err := Resolve(fixture(), "personal@example.com")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve(ambiguous email) = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"2", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should name candidate index %s", err, want)
		}
	}
}

func TestResolveByUUIDPrefix(t *testing.T) {
	got, err := Resolve(fixture(), "bbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if got.Idx != 2 {
		t.Fatalf("Idx = %d, want 2", got.Idx)
	}
}

// Short prefixes are refused: a switch overwrites the live credentials file, so
// a near-miss must not silently resolve.
func TestResolveRejectsShortUUIDPrefix(t *testing.T) {
	if _, err := Resolve(fixture(), "bbb"); err == nil {
		t.Fatal("Resolve(short prefix) = nil, want an error")
	}
}

// An ambiguous prefix names its candidates, so the user can see which
// characters would disambiguate without running `ccdad status` first.
func TestResolveAmbiguousUUIDPrefixNamesTheCandidates(t *testing.T) {
	accts := []Account{
		{UUID: "aaaaaaaa-1111-2222-3333-444444444444", Email: "one@example.com", Idx: 1},
		{UUID: "aaaaaaaa-9999-2222-3333-444444444444", Email: "two@example.com", Idx: 2},
	}

	_, err := Resolve(accts, "aaaaaaaa")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve(ambiguous prefix) = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"one@example.com", "two@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should name candidate %s", err, want)
		}
	}
}

func TestResolveUnknownNamesTheAlternatives(t *testing.T) {
	_, err := Resolve(fixture(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(unknown) = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "work") {
		t.Fatalf("error %q should list the available references", err)
	}
}

func TestResolveOnAnEmptyStoreSaysSo(t *testing.T) {
	_, err := Resolve(nil, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(empty) = %v, want ErrNotFound", err)
	}
	// Both logins, because resolution is provider-blind and an empty store
	// carries no hint of which one the reader wants. Bare 'ccdad add' is a
	// usage error, so naming it would answer a failed lookup with a second
	// failure.
	for _, want := range []string{"ccdad add claude", "ccdad add codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should point at %q when nothing is managed yet", err, want)
		}
	}
}

func TestResolveEmptyReferenceIsNotFound(t *testing.T) {
	if _, err := Resolve(fixture(), "   "); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(blank) = %v, want ErrNotFound", err)
	}
}

// The resolution order is fixed: index, alias, email, uuid prefix. The three
// tests below each hold one step of it against the step below, because nothing
// in the fixture above has a token claimed by two axes.

// ValidateAlias forbids a purely numeric alias, but a hand-edited accounts.toml
// can still carry one — and the index must win, which is the whole reason the
// rule exists.
func TestResolveIndexBeatsANumericAlias(t *testing.T) {
	accts := []Account{
		{UUID: "aaaaaaaa-1111", Alias: "2", Idx: 1},
		{UUID: "bbbbbbbb-1111", Idx: 2},
	}

	got, err := Resolve(accts, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != "bbbbbbbb-1111" {
		t.Fatalf("Resolve(%q) = %q, want the account at index 2", "2", got.UUID)
	}
}

// An alias may legitimately look like a uuid prefix: both draw from [a-z0-9].
func TestResolveAliasBeatsEmailAndUUIDPrefix(t *testing.T) {
	accts := []Account{
		{UUID: "aaaaaaaa-1111", Email: "shared@example.com", Idx: 1},
		{UUID: "bbbbbbbb-1111", Alias: "aaaaaaaa", Idx: 2},
	}

	got, err := Resolve(accts, "aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != "bbbbbbbb-1111" {
		t.Fatalf("Resolve = %q, want the alias holder", got.UUID)
	}
}

// A real uuid cannot contain '@', so this collision is not reachable from
// stored data; the case exists to pin the documented step order against a
// refactor that reshuffles it.
func TestResolveEmailBeatsUUIDPrefix(t *testing.T) {
	accts := []Account{
		{UUID: "user@exam", Idx: 1},
		{UUID: "bbbbbbbb-1111", Email: "user@exam", Idx: 2},
	}

	got, err := Resolve(accts, "user@exam")
	if err != nil {
		t.Fatal(err)
	}
	if got.Idx != 2 {
		t.Fatalf("Resolve = idx %d, want the email holder", got.Idx)
	}
}

// uuids are hex, so roughly 2.3% of them begin with eight digits. An index miss
// must fall through to step 4 rather than ending the search, or those accounts
// are unreachable by a numeric-looking UUID prefix.
func TestResolveFindsAUUIDPrefixThatIsAllDigits(t *testing.T) {
	accts := []Account{{UUID: "12345678-1111-2222-3333-444444444444", Idx: 1}}

	got, err := Resolve(accts, "12345678")
	if err != nil {
		t.Fatalf("Resolve(all-digit uuid prefix) = %v, want the account", err)
	}
	if got.Idx != 1 {
		t.Fatalf("Idx = %d, want 1", got.Idx)
	}
}

// Step 1 of the resolution order is "all digits", which "+2" is not.
func TestResolveDoesNotReadASignedNumberAsAnIndex(t *testing.T) {
	if _, err := Resolve(fixture(), "+2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(\"+2\") = %v, want ErrNotFound", err)
	}
}

func TestValidateAlias(t *testing.T) {
	// ValidateAlias judges the normalized form, so the CLI can validate a flag
	// value before touching the store and get the same verdict SetAlias would.
	valid := []string{"work", "dev-2", "a.b", "x_y", "Work", "WORK", "  work  "}
	for _, a := range valid {
		if err := ValidateAlias(a); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", a, err)
		}
	}
	invalid := []string{"", "  ", "123", "-lead", "has space", "UPPER!", "emoji😀"}
	for _, a := range invalid {
		if err := ValidateAlias(a); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want an error", a)
		}
	}
}

func TestValidateAliasErrorsAreBadAlias(t *testing.T) {
	for _, a := range []string{"", "123", "-lead", "has space"} {
		if err := ValidateAlias(a); !errors.Is(err, ErrBadAlias) {
			t.Errorf("ValidateAlias(%q) = %v, want ErrBadAlias", a, err)
		}
	}
}
