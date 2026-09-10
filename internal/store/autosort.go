package store

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// applyAutoSort shares the same ordering between listing and resolving an
// account reference. It reads only this store's cache and never polls.
func (s *Store) applyAutoSort(now time.Time) {
	raw, err := os.ReadFile(filepath.Join(s.root, config.FileName))
	if err != nil {
		return
	}
	cfg, err := config.Parse(raw)
	if err != nil || !cfg.AutoSort {
		return
	}
	cache, err := usage.LoadCacheAt(s.root)
	if err != nil || cache.LoadError() != nil {
		return
	}
	s.sortByWeeklyReset(cache, now)
}

func (s *Store) sortByWeeklyReset(cache *usage.Cache, now time.Time) {
	resets := make(map[string]time.Time)
	for _, a := range s.data.Accounts {
		if a.SubscriptionInactive() {
			continue
		}
		entry, ok := cache.Get(a.UUID)
		if !ok || entry.Snapshot == nil || entry.FetchedAt.Before(a.AddedAt) {
			continue
		}
		for _, w := range entry.Snapshot.AllWindows() {
			weekly := w.Name == usage.WindowSevenDay
			if a.Provider == provider.Codex {
				length, known := w.Length()
				weekly = known && length == 7*24*time.Hour && (w.Name == usage.WindowCodexPrimary || w.Name == usage.WindowCodexSecondary)
			}
			if !weekly {
				continue
			}
			at, known := w.Reset()
			if !known || at.Before(now) {
				continue
			}
			if prior, ok := resets[a.UUID]; !ok || at.Before(prior) {
				resets[a.UUID] = at
			}
		}
	}
	sort.SliceStable(s.data.Accounts, func(i, j int) bool {
		a, b := s.data.Accounts[i], s.data.Accounts[j]
		if ra, rb := providerRank(a.Provider), providerRank(b.Provider); ra != rb {
			return ra < rb
		}
		ra, ka := resets[a.UUID]
		rb, kb := resets[b.UUID]
		if ka != kb {
			return ka
		}
		return ka && ra.Before(rb)
	})
	s.reindex()
}
