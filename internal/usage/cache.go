package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/zone"
)

// The on-disk usage cache: one document the daemon writes and every CLI
// invocation reads.
//
// It is what keeps `ccdad list` and `ccdad status --json` from ever
// disagreeing, and it is the only thing standing between a scripted
// `ccdad list` and the endpoint's 28-30 requests per identity per rolling hour.
// That budget is a SLIDING WINDOW, so a burst saturates the identity for up to a
// full hour and pausing does not give the capacity back early — which is why
// serveTTL is enforced here, on the read path, and not only where a fetch is
// issued.

const (
	// ServeTTL is the poll policy's serveTTL: a reading younger than this is
	// served from the cache with no fetch, `--refresh` included.
	//
	// It is an alias rather than a second spelling. The policy lives in
	// internal/pollpolicy; a cache that carried its own copy of the number
	// would be one edit away from serving readings the scheduler thinks are
	// already stale.
	ServeTTL = pollpolicy.ServeTTL

	CacheFileName = "usage.json"
	// cacheLockDir is a DIRECTORY, because that is what cclock's mutex is.
	cacheLockDir = "usage.json.lock"

	// cacheLockStale is how long a lock's mtime may go unadvanced before
	// another process may take it over. A cache write is a sub-second
	// operation, so this only ever matters after a crash — and it must stay at
	// least twice cclock's touch interval or a live holder's lock goes stale by
	// its own definition between two touches.
	cacheLockStale = 30 * time.Second
)

// CachePath is where the cache lives.
func CachePath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, CacheFileName), nil
}

func cacheLockPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, cacheLockDir), nil
}

// PollState is the poll policy's per-account state, persisted so that restarting
// ccdad does not reset a backoff that a 429 earned. The policy owns what these
// mean; the cache only carries them across a process boundary.
type PollState struct {
	// Interval is the cadence currently in force, after any AIMD increase.
	Interval time.Duration `json:"interval,omitempty"`
	// LastRateLimited is when a 429 was last seen. The zero time means never,
	// which is not the same as "an hour ago".
	LastRateLimited time.Time `json:"last_rate_limited,omitempty"`
	// LastBindingPct is the previous sample's binding utilization, and
	// HasLastBinding whether there was one. The poll policy detects movement by
	// comparing against it, so it is persisted for the same reason the backoff
	// is: a restarted daemon with no baseline sees no movement, and one that
	// treated "no baseline" as movement would drop the whole fleet to the urgent
	// cadence on every start.
	LastBindingPct float64 `json:"last_binding_pct,omitempty"`
	HasLastBinding bool    `json:"has_last_binding,omitempty"`
}

// Entry is one account's cached reading.
type Entry struct {
	// Snapshot is the normalized reading. It is shared, not copied, so treat it
	// as read-only.
	Snapshot *Snapshot `json:"snapshot"`
	// FetchedAt is when the reading was taken.
	FetchedAt time.Time `json:"fetched_at"`
	// NextPollAt is when the scheduler intends to poll again.
	NextPollAt time.Time `json:"next_poll_at,omitempty"`
	Poll       PollState `json:"poll,omitempty"`
	// Probe is what the last probe of this account did. A probe spends the
	// account's own quota, so its schedule is persisted for the same reason
	// Poll's backoff is: a stamp that did not survive the process is a budget
	// that resets every time ccdad restarts.
	Probe ProbeState `json:"probe,omitempty"`
	// StandDownUntil is how long this account has yielded its share of the
	// identity's budget to the one a session is actually running against.
	//
	// It is a SEPARATE field from NextPollAt because it has a different writer:
	// another account's poll sets it, while NextPollAt is set by this account's
	// own. One field would mean whichever of the two goroutines finished last
	// erased the other's decision — including a 429's floor.
	StandDownUntil time.Time `json:"stand_down_until,omitempty"`
	// ServeTTL is how long this reading may be served before another poll is
	// worth a request. Zero means the package default: an entry written before
	// this field existed has no opinion, and no opinion must not read as "stale
	// immediately".
	ServeTTL time.Duration `json:"serve_ttl,omitempty"`
}

// Age is how old the reading is, and whether that could be worked out at all. An
// entry dated in the future is a clock that moved backwards, not a fresh
// reading, so it reports no age rather than a negative one.
func (e Entry) Age(now time.Time) (time.Duration, bool) {
	d := now.Sub(e.FetchedAt)
	if d < 0 {
		return 0, false
	}
	return d, true
}

// FreshWithin reports whether this reading is younger than ttl. An entry dated
// in the future is a clock that moved backwards rather than a fresh reading, so
// it is fresh under no ttl at all.
func (e Entry) FreshWithin(now time.Time, ttl time.Duration) bool {
	age, ok := e.Age(now)
	return ok && age < ttl
}

// Fresh reports whether this reading may be served without a fetch.
//
// It is the flat ServeTTL and deliberately NOT ScheduledTTL. This is the gate on
// the HAND-HELD path — `ccdad list --refresh`, through MayFetch — where it is
// the only rate bound there is, since that path ignores nextPollAt on purpose.
// Serving the scheduler's shortened TTL here would let a scripted refresh reach
// the endpoint six times as often as before, against an allowance of 28-30 an
// hour, on exactly the account where a 429 is most expensive.
func (e Entry) Fresh(now time.Time) bool { return e.FreshWithin(now, ServeTTL) }

// ScheduledTTL is the TTL that gates the next scheduled poll of this reading.
//
// It is the flat ServeTTL, and a SHORTER persisted value is ignored. The field
// still exists and still round-trips, because the file is written by one version
// of ccdad and read by another and an unknown-shaped row must not become an
// error — but the danger band used to write 30 s here, and every cache on every
// machine that has been in the band is carrying those rows right now. A build
// that merely stopped WRITING the short TTL would keep honouring the one already
// on disk until that account's next successful poll, which is precisely the poll
// the short TTL is letting through too early.
//
// A LONGER persisted value is honoured, because a future ccdad that slows a
// reading down is telling this one something it does not know, and the safe
// direction for an unknown is the slower one.
func (e Entry) ScheduledTTL() time.Duration {
	if e.ServeTTL > ServeTTL {
		return e.ServeTTL
	}
	return ServeTTL
}

// PollAt is when the scheduler intends to poll this account next: the schedule
// its own last poll earned, or a stand-down another account's poll wrote,
// whichever is LATER. Both are real, and neither cancels the other.
//
// live exempts the account Claude Code is logged in as, and that exemption is
// what makes a stand-down safe to persist at all. A stand-down is written for
// the accounts that do not matter right now; a switch changes which account that
// is, and holding the newly live one to a stand-down written for its predecessor
// would blind the engine on the only account a session can be cut off on, for as
// long as half an hour.
func (e Entry) PollAt(live bool) time.Time {
	if !live && e.StandDownUntil.After(e.NextPollAt) {
		return e.StandDownUntil
	}
	return e.NextPollAt
}

// cacheFile is the on-disk shape.
type cacheFile struct {
	// Version lets a later release migrate rather than guess.
	Version int `json:"version"`
	// Accounts is keyed by account UUID and never by idx. store.sortAndReindex
	// recompacts idx on every removal, so a cache keyed on it would silently
	// attribute one account's usage to another after any `ccdad remove`.
	Accounts map[string]Entry `json:"accounts"`
}

// Cache is the parsed usage cache.
type Cache struct {
	data    cacheFile
	loadErr error
}

// LoadError is why the cache came back empty when a file did exist. It is not
// fatal — a cache that cannot be read leaves every account UNKNOWN, which the
// engine already knows how to handle — but `ccdad doctor` should still be able
// to say so out loud rather than having the corruption stay invisible.
func (c *Cache) LoadError() error { return c.loadErr }

// Get returns an account's cached reading.
func (c *Cache) Get(uuid string) (Entry, bool) {
	e, ok := c.data.Accounts[uuid]
	return e, ok
}

// Put stores an account's reading.
func (c *Cache) Put(uuid string, e Entry) {
	if c.data.Accounts == nil {
		c.data.Accounts = map[string]Entry{}
	}
	c.data.Accounts[uuid] = e
}

// Delete drops an account's reading.
func (c *Cache) Delete(uuid string) { delete(c.data.Accounts, uuid) }

// MayFetch reports whether the endpoint may be called for this account, and it
// is the gate `--refresh` has to pass too. A reading younger than ServeTTL means
// no; anything else — no reading at all, an aged one, or one dated in the future
// by a clock that moved — means yes.
func (c *Cache) MayFetch(uuid string, now time.Time) bool {
	e, ok := c.Get(uuid)
	if !ok {
		return true
	}
	return !e.Fresh(now)
}

// Prune drops readings that no longer belong to anyone.
//
// accounts maps each managed account's uuid to when it was added. An entry whose
// uuid is absent belonged to an account that has been removed. An entry OLDER
// than its account's AddedAt belonged to a previous account at the same uuid —
// removed and added again — and letting that through would hand a fresh login
// the headroom its predecessor had already spent.
func (c *Cache) Prune(accounts map[string]time.Time) {
	for uuid, e := range c.data.Accounts {
		addedAt, managed := accounts[uuid]
		if !managed || e.FetchedAt.Before(addedAt) {
			delete(c.data.Accounts, uuid)
		}
	}
}

// storeRoot returns ccdad's state directory, refusing a relative one for the
// same reason store.Open does: a relative root puts the cache in whatever
// directory ccdad happened to be run from, a different one each time.
func storeRoot() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}
	return root, nil
}

// LoadCache reads the cache without taking a lock.
//
// No lock is needed to read: every write is a rename, so a reader sees one whole
// version of the document or another, never a torn one. A file that is
// unreadable ANYWAY — hand-edited, truncated by a full disk, written by a future
// version — degrades to an empty cache, which reads as UNKNOWN for every
// account. It must never degrade to zero: an unread account is not an empty
// one, and cswap's version of this bug parked its engine permanently.
func LoadCache() (*Cache, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	c := &Cache{data: cacheFile{Version: 1, Accounts: map[string]Entry{}}}

	raw, err := os.ReadFile(filepath.Join(root, CacheFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		// A cache we cannot read at all is the same condition as one we cannot
		// parse: unknown, not fatal.
		c.loadErr = fmt.Errorf("reading %s: %w", CacheFileName, err)
		return c, nil
	}

	var parsed cacheFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		c.loadErr = fmt.Errorf("parsing %s: %w", CacheFileName, err)
		return c, nil
	}
	if parsed.Accounts != nil {
		c.data.Accounts = parsed.Accounts
	}
	c.data.Version = parsed.Version
	return c, nil
}

// save writes the cache atomically. The caller must hold the lock.
//
// ccdad's OWN moments are rendered in the machine's zone -- fetched_at,
// next_poll_at, stand_down_until and the two stamps inside poll and probe --
// which is the rule status.json, every --json document and the history series
// obey, applied at this file's one serialiser for the same reason: a writer's
// job is to choose an instant, and an instant has no zone.
//
// This document is the one that CANNOT carry a single zone, and that is
// deliberate rather than a gap. The snapshot half is a verbatim mirror of the
// endpoint's body -- Snapshot.MarshalJSON writes the endpoint's own shape and
// Snapshot.UnmarshalJSON reads it back through the parser a live response goes
// through, which is what lets a cache be compared against bodies recorded from
// the endpoint. Rendering a resets_at in a local offset would still parse and
// would end that, so the wire's moments stay exactly as they arrived.
//
// The boundary is the `snapshot` object, and it holds structurally rather than
// by a list of names: every moment inside a Snapshot lives in an unexported
// field behind that codec, so the walk below cannot reach one even if somebody
// wanted it to.
//
// What CAN break the mirror is fromTime, which is the one place a snapshot's
// moments are rendered. Changing its .UTC() is what
// TestTheCacheRendersItsOwnMomentsLocallyAndMirrorsTheWireVerbatim catches --
// measured, by making that substitution and watching it fail. Adding an
// exported time.Time to Snapshot does NOT reach the wire and does NOT fail
// anything here, because Snapshot.MarshalJSON builds an explicit wire struct
// rather than encoding its own fields; a field added there has to be added to
// toWire before it means anything at all.
func (c *Cache) save(root string) error {
	c.data.Version = 1
	encoded, err := json.Marshal(zone.In(c.data, time.Local))
	if err != nil {
		return fmt.Errorf("encoding %s: %w", CacheFileName, err)
	}
	return atomicfile.WriteFile(filepath.Join(root, CacheFileName), encoded, 0o600)
}

// WithCache runs fn against the cache under a cross-process lock and writes back
// what it changed. This is the only safe way to modify the cache.
//
// An atomic rename alone is not enough here. The daemon writes one account's
// entry while `ccdad list --refresh` writes another, and both do a
// read-modify-write of the same document: without the lock the second rename
// silently drops the first one's entry. The lock is cclock's — the same
// mkdir-based advisory mutex ccdad already uses against Claude Code, with the
// same staleness recovery — rather than a second lock mechanism.
//
// fn returning an error leaves the file exactly as it was: a poll that failed
// halfway must not persist half a reading.
func WithCache(timeout time.Duration, fn func(*Cache) error) (err error) {
	root, rerr := storeRoot()
	if rerr != nil {
		return rerr
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}

	lock, aerr := cclock.Acquire(filepath.Join(root, cacheLockDir), cclock.Options{
		Stale:   cacheLockStale,
		Timeout: timeout,
	})
	if aerr != nil {
		return fmt.Errorf("locking the usage cache: %w", aerr)
	}
	// Release's return value is part of the answer, not noise. cclock detects a
	// takeover two ways, and the one Release performs — a synchronous re-stat of
	// the lock directory — is the ONLY one that can see a takeover in the window
	// between the touch goroutine's last tick and now. Discarding it would
	// report success for exactly the write that raced. It also reports a lock
	// directory that could not be removed, which would otherwise block every
	// other writer silently until the stale threshold elapsed.
	defer func() { err = errors.Join(err, lock.Release()) }()

	c, err := LoadCache()
	if err != nil {
		return err
	}
	// A cache that could not be parsed is deliberately overwritten rather than
	// preserved: nothing in it was recoverable, and refusing to write would
	// leave the file broken for every future run. LoadError has already
	// recorded what happened for `ccdad doctor`.
	if err := fn(c); err != nil {
		return err
	}
	return c.save(root)
}
