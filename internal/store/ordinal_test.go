package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// addTo is one account of one provider, with whichever credential blob that
// provider's writer expects.
func addTo(t *testing.T, s *Store, p provider.ID, uuid string) {
	t.Helper()
	creds := sampleCreds("AT-" + uuid)
	if p == provider.Codex {
		creds = codexCreds("RT-" + uuid)
	}
	if err := s.Add(Account{UUID: uuid, Provider: p, Email: uuid + "@example.com"}, creds); err != nil {
		t.Fatalf("Add(%s, %s) = %v", uuid, p, err)
	}
}

// interleaved is the fleet that made every one of these rules necessary: the
// two providers added alternately, which is what a store looks like after a
// user adds a Codex seat, then another Claude one, then another of each.
func interleaved(t *testing.T) *Store {
	t.Helper()
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	addTo(t, s, provider.Claude, "c-one")
	addTo(t, s, provider.Codex, "x-one")
	addTo(t, s, provider.Claude, "c-two")
	addTo(t, s, provider.Codex, "x-two")
	addTo(t, s, provider.Claude, "c-three")
	return s
}

// refs is every account as "<uuid>=<ref>", in the order the store hands them
// out. One string per assertion is what makes a failure readable: the order and
// the numbering are the same fact, and a test that checked them separately
// would report half of a wrong answer.
func refs(s *Store) string {
	accounts := s.Accounts()
	parts := make([]string, 0, len(accounts))
	for _, a := range accounts {
		parts = append(parts, a.UUID+"="+a.Ref())
	}
	return strings.Join(parts, " ")
}

// The index is numbered WITHIN a provider, and the slice is grouped so that the
// numbers under each heading come out contiguous.
//
// Both halves matter and they are one assertion because they are one mechanism.
// A store-wide ordinal numbered this fleet 1,2,3,4,5 in add order, which the
// account tables then drew as CLAUDE 1,3,5 over CODEX 2,4 -- a table that reads
// as though it has lost rows -- and which made the dashboard cursor jump
// between the two halves on every arrow key, because the cursor steps through
// this slice and the page draws the grouping.
func TestTheIndexIsPerProviderAndTheSliceIsGrouped(t *testing.T) {
	s := interleaved(t)

	const want = "c-one=c1 c-two=c2 c-three=c3 x-one=x1 x-two=x2"
	if got := refs(s); got != want {
		t.Errorf("store order and refs = %q, want %q", got, want)
	}
}

// The grouping and the numbering survive a round trip through accounts.toml.
//
// load is where a hand-edited document arrives and where sortAndReindex runs on
// every open, so this is the assertion that the invariant is a property of the
// store rather than of the sequence of calls that happened to build it.
func TestTheGroupingSurvivesAReopen(t *testing.T) {
	s := interleaved(t)
	want := refs(s)

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := refs(reopened); got != want {
		t.Errorf("after reopen = %q, want %q", got, want)
	}
}

// A removal recompacts the provider it happened in and leaves the other
// provider's numbers alone.
//
// This is what a store-wide ordinal could not do: removing one Claude seat
// renumbered every Codex one below it, so a user reading `ccdad status` for a
// Codex account found it under a different number for a reason that had nothing
// to do with Codex.
func TestARemovalRecompactsOnlyItsOwnProvider(t *testing.T) {
	s := interleaved(t)

	if err := s.Remove("c-two"); err != nil {
		t.Fatal(err)
	}
	const want = "c-one=c1 c-three=c2 x-one=x1 x-two=x2"
	if got := refs(s); got != want {
		t.Errorf("after removing c-two = %q, want %q", got, want)
	}
}

// Ref is the provider's letter and the per-provider number, which is the form
// every ambiguity message tells a user to type and the form Resolve accepts.
func TestRefIsThePrefixAndTheNumber(t *testing.T) {
	for _, tc := range []struct {
		p    provider.ID
		idx  int
		want string
	}{
		{provider.Claude, 1, "c1"},
		{provider.Claude, 12, "c12"},
		{provider.Codex, 1, "x1"},
		{provider.Codex, 3, "x3"},
	} {
		if got := (Account{Provider: tc.p, Idx: tc.idx}).Ref(); got != tc.want {
			t.Errorf("Account{%s, %d}.Ref() = %q, want %q", tc.p, tc.idx, got, tc.want)
		}
	}
}

// A position is counted in the account's OWN provider.
//
// Moving the second Codex seat to position 1 puts it above the first Codex seat
// and moves no Claude account. Under a store-wide position the same command
// would have put it at the very top of the store, above every Claude row, and
// the number it landed on would not have been 1.
func TestMoveCountsPositionsWithinTheProvider(t *testing.T) {
	s := interleaved(t)

	moved, err := s.Move("x-two", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("Move reported no change, want a move")
	}
	const want = "c-one=c1 c-two=c2 c-three=c3 x-two=x1 x-one=x2"
	if got := refs(s); got != want {
		t.Errorf("after moving x-two to 1 = %q, want %q", got, want)
	}
}

// A position past the end clamps to the PROVIDER's last account, not the
// store's. The clamp is what makes "put it last" typeable as a big number, and
// clamping to the store's end would move a Claude account under every Codex one
// -- which is the one thing move may never do.
func TestMoveClampsToTheProvidersOwnEnd(t *testing.T) {
	s := interleaved(t)

	if _, err := s.Move("c-one", 99); err != nil {
		t.Fatal(err)
	}
	const want = "c-two=c1 c-three=c2 c-one=c3 x-one=x1 x-two=x2"
	if got := refs(s); got != want {
		t.Errorf("after moving c-one to 99 = %q, want %q", got, want)
	}
	got, _ := s.Get("c-one")
	if got.Provider != provider.Claude {
		t.Errorf("c-one is stored as %s after the move, want claude", got.Provider)
	}
}

// A bare number names one account under each provider, so it is answered by
// naming both and the two spellings that separate them -- never by picking one.
// The commands that take a reference overwrite a credentials file.
func TestABareIndexThatBothProvidersCarryIsAmbiguous(t *testing.T) {
	s := interleaved(t)

	_, err := Resolve(s.Accounts(), "1")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve(1) = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"c1=c-one@example.com", "x1=x-one@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the ambiguity does not name %q:\n%s", want, err)
		}
	}
}

// A bare number only ONE provider carries still resolves. Refusing it would
// make every reference on a single-provider fleet two characters longer for a
// disambiguation that has nothing to disambiguate.
func TestABareIndexOnlyOneProviderCarriesStillResolves(t *testing.T) {
	s := interleaved(t)

	got, err := Resolve(s.Accounts(), "3")
	if err != nil {
		t.Fatalf("Resolve(3) = %v, want the third Claude account", err)
	}
	if got.UUID != "c-three" {
		t.Errorf("Resolve(3) = %s, want c-three", got.UUID)
	}
}

// The prefixed form always names exactly one account, which is what makes it
// the answer the ambiguity message points at.
func TestTheProviderScopedIndexResolves(t *testing.T) {
	s := interleaved(t)

	for ref, want := range map[string]string{
		"c1": "c-one", "c3": "c-three", "x1": "x-one", "x2": "x-two",
	} {
		got, err := Resolve(s.Accounts(), ref)
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want %s", ref, err, want)
			continue
		}
		if got.UUID != want {
			t.Errorf("Resolve(%q) = %s, want %s", ref, got.UUID, want)
		}
	}
}

// A prefixed reference past the fleet is NOT FOUND rather than a clamp:
// nothing in this package may guess which account a user meant.
func TestAProviderScopedIndexPastTheFleetIsNotFound(t *testing.T) {
	s := interleaved(t)

	if _, err := Resolve(s.Accounts(), "x9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve(x9) = %v, want ErrNotFound", err)
	}
}

// An alias of the prefixed shape, stored by a build that predates the rule,
// goes on naming the account its owner gave it. Taking a working handle away on
// upgrade is a worse answer than letting one legacy alias shadow a reference
// that has a second spelling -- and it is why Resolve tries the alias first.
func TestALegacyAliasShapedLikeAReferenceStillWins(t *testing.T) {
	s := interleaved(t)

	// Set through the field rather than SetAlias, which now refuses the shape.
	// That is the point: an alias like this can only have arrived from an
	// older build, and this is what that store looks like on this one.
	accounts := s.Accounts()
	for i := range accounts {
		if accounts[i].UUID == "c-three" {
			accounts[i].Alias = "x1"
		}
	}

	got, err := Resolve(accounts, "x1")
	if err != nil {
		t.Fatalf("Resolve(x1) = %v, want the aliased account", err)
	}
	if got.UUID != "c-three" {
		t.Errorf("Resolve(x1) = %s, want c-three -- the alias, not the ordinal", got.UUID)
	}
}

// No NEW alias may spell a reference, for the reason no alias may be all
// digits: two axes that accept the same token must not name two accounts.
func TestAnAliasMayNotSpellAReference(t *testing.T) {
	for _, bad := range []string{"c1", "x12", "C1"} {
		if err := ValidateAlias(bad); !errors.Is(err, ErrBadAlias) {
			t.Errorf("ValidateAlias(%q) = %v, want ErrBadAlias", bad, err)
		}
	}
	// The rule is the SHAPE and not the letter. An alias that merely starts
	// with one of the two letters is ordinary and must stay allowed, or every
	// handle beginning with c or x becomes unusable.
	for _, good := range []string{"cx", "c1a", "work", "x", "c", "claude-main"} {
		if err := ValidateAlias(good); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", good, err)
		}
	}
}

// The not-found list is printed by reference. It is not grouped by provider, so
// a bare number in it would appear twice against two different accounts --
// which is the confusion this error exists to end.
func TestTheNotFoundListNamesAccountsByReference(t *testing.T) {
	s := interleaved(t)

	_, err := Resolve(s.Accounts(), "nobody@example.com")
	if err == nil {
		t.Fatal("Resolve of an unknown email = nil, want an error")
	}
	for _, want := range []string{"c1=c-one@example.com", "x2=x-two@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the available list does not carry %q:\n%s", want, err)
		}
	}
}

// A re-add keeps the account's number, and the number it keeps is its own
// provider's. This is what makes `ccdad add` double as re-authentication
// without moving a row on anybody's screen.
func TestReAddingKeepsThePerProviderIndex(t *testing.T) {
	s := interleaved(t)

	before, _ := s.Get("x-two")
	if err := s.Add(Account{UUID: "x-two", Provider: provider.Codex, Email: "x-two@example.com"},
		codexCreds("RT-again")); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get("x-two")
	if after.Idx != before.Idx || after.Ref() != "x2" {
		t.Errorf("after re-add, ref = %q (idx %d), want x2 (idx %d)", after.Ref(), after.Idx, before.Idx)
	}
	if got := refs(s); got != "c-one=c1 c-two=c2 c-three=c3 x-one=x1 x-two=x2" {
		t.Errorf("a re-add reordered the store: %q", got)
	}
}
