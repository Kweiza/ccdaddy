package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestAutoSortUsesWeeklyResetsWithinEachProviderAndResolvesDisplayedIndexes(t *testing.T) {
	s := interleaved(t)
	now := time.Now()
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		for uuid, delay := range map[string]time.Duration{"c-one": 3 * time.Hour, "c-two": time.Hour, "x-one": 4 * time.Hour, "x-two": 2 * time.Hour} {
			at := now.Add(delay)
			snap := &usage.Snapshot{SevenDay: usage.NewWindow(nil, &at)}
			if uuid[0] == 'x' {
				snap = &usage.Snapshot{CodexPrimary: usage.NewWindowWithLength(nil, &at, 7*24*time.Hour)}
			}
			c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: now})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const manual = "c-one=c1 c-two=c2 c-three=c3 x-one=x1 x-two=x2"
	if got := refs(reopen(t)); got != manual {
		t.Fatalf("default order = %s", got)
	}
	if err := os.WriteFile(filepath.Join(s.root, config.FileName), []byte("auto_sort = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sorted := reopen(t)
	const want = "c-two=c1 c-one=c2 c-three=c3 x-two=x1 x-one=x2"
	if got := refs(sorted); got != want {
		t.Fatalf("automatic order = %s, want %s", got, want)
	}
	for ref, uuid := range map[string]string{"c1": "c-two", "x1": "x-two"} {
		got, err := Resolve(sorted.Accounts(), ref)
		if err != nil || got.UUID != uuid {
			t.Fatalf("Resolve(%s) = %s, %v", ref, got.UUID, err)
		}
	}
	// A read does not rewrite manual order. Disabling restores stored positions.
	if err := os.WriteFile(filepath.Join(s.root, config.FileName), []byte("auto_sort = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := refs(reopen(t)); got != manual {
		t.Fatalf("disabled order = %s", got)
	}
}

func TestAutoSortIgnoresExpiredUnknownAndNonWeeklyResetsAndKeepsTiesStable(t *testing.T) {
	s := interleaved(t)
	now := time.Now()
	soon, expired := now.Add(time.Hour), now.Add(-time.Hour)
	c := &usage.Cache{}
	c.Put("c-one", usage.Entry{Snapshot: &usage.Snapshot{SevenDay: usage.NewWindow(nil, &expired)}, FetchedAt: now})
	for _, uuid := range []string{"c-two", "c-three"} {
		c.Put(uuid, usage.Entry{Snapshot: &usage.Snapshot{SevenDay: usage.NewWindow(nil, &soon)}, FetchedAt: now})
	}
	c.Put("x-one", usage.Entry{Snapshot: &usage.Snapshot{CodexSecondary: usage.NewWindowWithLength(nil, &soon, 5*time.Hour)}, FetchedAt: now})
	c.Put("x-two", usage.Entry{Snapshot: &usage.Snapshot{CodexSecondary: usage.NewWindowWithLength(nil, &soon, 7*24*time.Hour)}, FetchedAt: now})
	s.sortByWeeklyReset(c, now)
	const want = "c-two=c1 c-three=c2 c-one=c3 x-two=x1 x-one=x2"
	if got := refs(s); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}
