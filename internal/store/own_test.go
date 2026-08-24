package store

import (
	"errors"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/identity"
)

// three accounts, so a partition has a middle to get wrong.
func seedThree(t *testing.T) *Store {
	t.Helper()
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u-1", "u-2", "u-3"} {
		a := Account{UUID: u, Email: u + "@example.com", Kind: identity.KindSubscription}
		if err := s.Add(a, sampleCreds("AT-"+u)); err != nil {
			t.Fatalf("Add(%s) = %v", u, err)
		}
	}
	return s
}

func elsewhereSet(s *Store) map[string]bool {
	out := map[string]bool{}
	for _, a := range s.Accounts() {
		out[a.UUID] = a.Elsewhere
	}
	return out
}

// The partition is ONE statement about the whole list, and that is why SetOwned
// takes the set rather than one uuid at a time. A per-account toggle lets a user
// own the same account on two machines by forgetting the other half, which is the
// convergence the flag exists to prevent and which is invisible until a five-hour
// window burns twice as fast.
func TestSetOwnedIsAStatementAboutTheWholeList(t *testing.T) {
	s := seedThree(t)

	changed, err := s.SetOwned([]string{"u-1"})
	if err != nil {
		t.Fatalf("SetOwned() = %v, want nil", err)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2 — the two accounts handed to another machine", changed)
	}
	got := elsewhereSet(s)
	if got["u-1"] || !got["u-2"] || !got["u-3"] {
		t.Errorf("Elsewhere = %v, want only u-1 owned here", got)
	}

	// Naming a different set REPLACES the partition rather than adding to it.
	if _, err := s.SetOwned([]string{"u-3"}); err != nil {
		t.Fatalf("SetOwned() = %v, want nil", err)
	}
	got = elsewhereSet(s)
	if !got["u-1"] || !got["u-2"] || got["u-3"] {
		t.Errorf("Elsewhere = %v, want only u-3 owned here", got)
	}
}

// The empty set is "own everything", the state every store is in before the
// partition is first declared -- and therefore every store that existed before
// the flag did.
func TestAnEmptyOwnedSetClearsThePartition(t *testing.T) {
	s := seedThree(t)
	if _, err := s.SetOwned([]string{"u-2"}); err != nil {
		t.Fatal(err)
	}

	changed, err := s.SetOwned(nil)
	if err != nil {
		t.Fatalf("SetOwned(nil) = %v, want nil", err)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2", changed)
	}
	for uuid, away := range elsewhereSet(s) {
		if away {
			t.Errorf("%s is still another machine's after the partition was cleared", uuid)
		}
	}
}

// Nothing is written unless every uuid resolves. A half-applied partition leaves
// the machine owning a set nobody asked for, which is worse than refusing.
func TestSetOwnedWritesNothingWhenAnyAccountIsUnknown(t *testing.T) {
	s := seedThree(t)
	if _, err := s.SetOwned([]string{"u-1"}); err != nil {
		t.Fatal(err)
	}
	before := elsewhereSet(s)

	_, err := s.SetOwned([]string{"u-2", "u-nope"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetOwned() = %v, want ErrNotFound", err)
	}
	if got := elsewhereSet(s); got["u-1"] != before["u-1"] ||
		got["u-2"] != before["u-2"] || got["u-3"] != before["u-3"] {
		t.Errorf("Elsewhere = %v, want the partition untouched at %v", got, before)
	}

	// And the refusal is durable, not just in memory.
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	// The refused call tried to CLAIM u-2 for this machine, so the evidence that
	// nothing was written is that u-2 is still the other machine's on disk.
	if got := elsewhereSet(reopened); !got["u-2"] {
		t.Error("a refused SetOwned reached the disk and claimed u-2")
	}
}

// An account added after the partition belongs to ANOTHER machine by default.
//
// This is the whole reason the flag is not just `ccdad disable` run N times. A
// machine partitioned by disabling silently starts sharing every account added
// after the last disable, and sharing one account is all it takes: both machines
// rank it the same way and switch to it at the same moment.
func TestAnAccountAddedAfterAPartitionBelongsElsewhere(t *testing.T) {
	s := seedThree(t)
	if _, err := s.SetOwned([]string{"u-1"}); err != nil {
		t.Fatal(err)
	}

	added := Account{UUID: "u-4", Email: "d@example.com", Kind: identity.KindSubscription}
	if err := s.Add(added, sampleCreds("AT-u-4")); err != nil {
		t.Fatal(err)
	}
	if got := elsewhereSet(s); !got["u-4"] {
		t.Error("an account added under a partition was owned here by default, so both " +
			"machines would rank it")
	}
}

// And with NO partition declared the default is the other way, because a store
// that has never run `ccdad own` owns everything.
func TestAnAccountAddedWithNoPartitionIsOwnedHere(t *testing.T) {
	s := seedThree(t)
	added := Account{UUID: "u-4", Email: "d@example.com", Kind: identity.KindSubscription}
	if err := s.Add(added, sampleCreds("AT-u-4")); err != nil {
		t.Fatal(err)
	}
	if got := elsewhereSet(s); got["u-4"] {
		t.Error("an account added to an unpartitioned store was handed to another machine")
	}
}

// Re-adding an account -- `ccdad add` over an existing login, which is how a
// re-authentication lands -- keeps the side of the partition it was on. The
// alternative silently returns an account to this machine on every re-login.
func TestReAddingAnAccountKeepsItsSideOfThePartition(t *testing.T) {
	s := seedThree(t)
	if _, err := s.SetOwned([]string{"u-1"}); err != nil {
		t.Fatal(err)
	}
	again := Account{UUID: "u-2", Email: "b@example.com", Kind: identity.KindSubscription}
	if err := s.Add(again, sampleCreds("AT-u-2-rotated")); err != nil {
		t.Fatal(err)
	}
	if got := elsewhereSet(s); !got["u-2"] {
		t.Error("re-authenticating an account another machine owns claimed it for this one")
	}
}

// The flag survives the TOML round trip, or a restart un-partitions the machine.
func TestThePartitionSurvivesAReopen(t *testing.T) {
	s := seedThree(t)
	if _, err := s.SetOwned([]string{"u-2"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got := elsewhereSet(reopened)
	if !got["u-1"] || got["u-2"] || !got["u-3"] {
		t.Errorf("Elsewhere = %v after a reopen, want only u-2 owned here", got)
	}
}
