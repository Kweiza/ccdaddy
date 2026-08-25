package usage

import (
	"encoding/json"
	"errors"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCDAD_HOME", dir)
	return dir
}

// ---- the Snapshot codec, which the cache is built on -----------------------

func TestSnapshotRoundTripsThroughJSON(t *testing.T) {
	want := mustParse(t, realBody)

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(want, &got) {
		t.Errorf("round trip lost information:\n want %+v\n  got %+v", want, &got)
	}
}

func TestSnapshotRoundTripKeepsSubSecondResets(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 1, "resets_at": "2026-08-22T09:00:00.500Z"}}`)

	encoded, _ := json.Marshal(s)
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	at, ok := got.FiveHour.Reset()
	if !ok {
		t.Fatal("Reset() reported unknown after a round trip")
	}
	want := mustTime(t, "2026-08-22T09:00:00.500Z")
	if !at.Equal(want) {
		t.Errorf("Reset() = %s, want %s — the endpoint sends milliseconds and they must survive the disk", at, want)
	}
}

// The encoded form is the endpoint's own shape, so a cached reading and a live
// one are the same document and one parser reads both.
func TestSnapshotEncodesTheEndpointsOwnKeys(t *testing.T) {
	s := mustParse(t, realBody)
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("the encoded form is not an object: %v", err)
	}
	for _, k := range usageFields {
		if _, ok := keys[k]; !ok {
			t.Errorf("the encoded snapshot dropped %q", k)
		}
	}
	if pct := string(keys["five_hour"]); !strings.Contains(pct, "92.5") {
		t.Errorf("five_hour = %s, want the percent kept verbatim", pct)
	}
}

// Tri-state has to survive the disk, or the cache reintroduces the exact bug the
// parser exists to prevent.
func TestSnapshotRoundTripKeepsUnknownsUnknown(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": null, "resets_at": null},
	                   "extra_usage": {"is_enabled": true, "monthly_limit": null,
	                                   "used_credits": null, "utilization": null}}`)

	encoded, _ := json.Marshal(s)
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !got.FiveHour.Present {
		t.Error("five_hour lost its presence across the disk")
	}
	if _, ok := got.FiveHour.Percent(); ok {
		t.Error("a null utilization came back as a value")
	}
	if _, ok := got.ExtraUsage.UsedCredits(); ok {
		t.Error("a null used_credits came back as a value; unknown spend is not $0")
	}
	if got.SevenDay.Present {
		t.Error("an absent window came back present")
	}
}

// The cache doubles as the --json shape, so what it writes has to be a document
// the endpoint could have produced — not merely one this package can read back.
func TestSnapshotEncodesABlockedAccountTheWayTheEndpointWouldHave(t *testing.T) {
	s := mustParse(t, `{"extra_usage": {"is_enabled": false, "monthly_limit": null,
	                                    "used_credits": null, "utilization": null,
	                                    "disabled_reason": "out_of_credits"}}`)

	encoded, _ := json.Marshal(s)
	var got struct {
		ExtraUsage struct {
			IsEnabled      *bool   `json:"is_enabled"`
			DisabledReason *string `json:"disabled_reason"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.ExtraUsage.IsEnabled == nil || *got.ExtraUsage.IsEnabled {
		t.Errorf("is_enabled = %v, want false — a blocked account has overage off, with a reason", got.ExtraUsage.IsEnabled)
	}
	if got.ExtraUsage.DisabledReason == nil || *got.ExtraUsage.DisabledReason != "out_of_credits" {
		t.Errorf("disabled_reason = %v, want out_of_credits", got.ExtraUsage.DisabledReason)
	}
}

func TestSnapshotRoundTripsAnAbsentExtraUsageAsAbsent(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 1, "resets_at": null}}`)
	if s.ExtraUsage.Present {
		t.Fatal("the fixture already has an extra_usage")
	}

	encoded, _ := json.Marshal(s)
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.ExtraUsage.Present {
		t.Error("an account that reported no extra_usage came back carrying one; " +
			"\"no credit axis at all\" and \"a credit axis we could not read\" are different facts")
	}
}

func TestSnapshotRoundTripsEveryExtraUsageState(t *testing.T) {
	bodies := map[ExtraUsageState]string{
		ExtraUsageEnabled:  `{"extra_usage": {"is_enabled": true, "monthly_limit": null, "used_credits": null, "utilization": null}}`,
		ExtraUsageDisabled: `{"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}}`,
		ExtraUsageBlocked:  `{"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "disabled_reason": "out_of_credits"}}`,
		ExtraUsageUnknown:  `{"extra_usage": {"monthly_limit": null, "used_credits": null, "utilization": null}}`,
	}
	for want, body := range bodies {
		t.Run(want.String(), func(t *testing.T) {
			encoded, _ := json.Marshal(mustParse(t, body))
			var got Snapshot
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if got.ExtraUsage.State != want {
				t.Errorf("State = %v, want %v", got.ExtraUsage.State, want)
			}
		})
	}
}

// ---- the cache -------------------------------------------------------------

func TestCacheRoundTripsAnEntry(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	err := WithCache(time.Second, func(c *Cache) error {
		c.Put("acct-1", Entry{
			Snapshot:       mustParse(t, realBody),
			FetchedAt:      now,
			NextPollAt:     now.Add(3 * time.Minute),
			StandDownUntil: now.Add(30 * time.Minute),
			ServeTTL:       2 * ServeTTL,
			Poll:           PollState{Interval: 180 * time.Second, LastRateLimited: now.Add(-time.Hour)},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WithCache() error = %v", err)
	}

	c, err := LoadCache()
	if err != nil {
		t.Fatalf("LoadCache() error = %v", err)
	}
	e, ok := c.Get("acct-1")
	if !ok {
		t.Fatal("Get() found nothing after a write")
	}
	if pct, ok := e.Snapshot.FiveHour.Percent(); !ok || pct != 92.5 {
		t.Errorf("FiveHour.Percent() = %v, %v", pct, ok)
	}
	if !e.FetchedAt.Equal(now) {
		t.Errorf("FetchedAt = %s, want %s", e.FetchedAt, now)
	}
	if !e.NextPollAt.Equal(now.Add(3 * time.Minute)) {
		t.Errorf("NextPollAt = %s", e.NextPollAt)
	}
	if !e.StandDownUntil.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("StandDownUntil = %s — a restart would poll an account that yielded its share", e.StandDownUntil)
	}
	if e.ServeTTL != 2*ServeTTL {
		t.Errorf("ServeTTL = %s, want %s — the field must round-trip, so a ccdad that "+
			"slows a reading down can tell an older one about it", e.ServeTTL, 2*ServeTTL)
	}
	if e.Poll.Interval != 180*time.Second {
		t.Errorf("Poll.Interval = %v, want 180s — a restart must not reset a backoff", e.Poll.Interval)
	}
	if !e.Poll.LastRateLimited.Equal(now.Add(-time.Hour)) {
		t.Errorf("Poll.LastRateLimited = %s", e.Poll.LastRateLimited)
	}
}

// idx is an ordinal that store.sortAndReindex recompacts on every removal, so a
// cache keyed on it silently attributes one account's usage to another. The key
// is the uuid, and nothing in the file may be an index.
func TestCacheIsKeyedByUUID(t *testing.T) {
	dir := isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("aaaa-1111", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		c.Put("bbbb-2222", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	})

	raw, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	var file struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("the cache file is not the expected shape: %v\n%s", err, raw)
	}
	for _, k := range []string{"aaaa-1111", "bbbb-2222"} {
		if _, ok := file.Accounts[k]; !ok {
			t.Errorf("the cache is not keyed by uuid; keys are %v", keysOf(file.Accounts))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A torn or hand-mangled file degrades every entry to UNKNOWN — no entries at
// all — rather than to zero, which would read as "every account is empty" and
// park the engine.
func TestCacheDegradesACorruptFileToUnknown(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), []byte(`{"accounts": {"a": `), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := LoadCache()
	if err != nil {
		t.Fatalf("LoadCache() error = %v; a corrupt cache must not stop ccdad", err)
	}
	if _, ok := c.Get("a"); ok {
		t.Error("Get() returned an entry out of a corrupt file")
	}
	if c.LoadError() == nil {
		t.Error("LoadError() = nil; the corruption must stay visible to `ccdad doctor` even though it is not fatal")
	}
}

func TestCacheOnAMissingFileIsEmptyAndNotAnError(t *testing.T) {
	isolate(t)

	c, err := LoadCache()
	if err != nil {
		t.Fatalf("LoadCache() error = %v", err)
	}
	if c.LoadError() != nil {
		t.Errorf("LoadError() = %v; a cache that has never been written is not corrupt", c.LoadError())
	}
	if _, ok := c.Get("anything"); ok {
		t.Error("Get() returned an entry from an empty cache")
	}
}

// serveTTL is enforced on the READ path. Without that, `ccdad list --refresh` in
// a shell loop bursts the sliding window and saturates the identity's 28-30
// requests per hour in seconds.
func TestCacheRefusesToFetchInsideServeTTL(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("acct-1", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	})
	c, _ := LoadCache()

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"immediately", now, false},
		{"just inside the TTL", now.Add(ServeTTL - time.Second), false},
		{"exactly at the TTL", now.Add(ServeTTL), true},
		{"past the TTL", now.Add(ServeTTL + time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.MayFetch("acct-1", tc.at); got != tc.want {
				t.Errorf("MayFetch() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCacheAllowsAFetchForAnAccountItHasNeverSeen(t *testing.T) {
	isolate(t)
	c, _ := LoadCache()

	if !c.MayFetch("brand-new", time.Now()) {
		t.Error("MayFetch() = false for an account with no cached reading")
	}
}

// A clock that moved backwards makes an entry look like it is from the future.
// That is not freshness, and serving it would pin the engine to a reading it can
// never age out of.
func TestCacheDoesNotTrustAnEntryFromTheFuture(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("acct-1", Entry{Snapshot: &Snapshot{}, FetchedAt: now.Add(time.Hour)})
		return nil
	})
	c, _ := LoadCache()

	if !c.MayFetch("acct-1", now) {
		t.Error("MayFetch() = false for an entry dated in the future; a backwards clock is not freshness")
	}
	e, _ := c.Get("acct-1")
	if _, ok := e.Age(now); ok {
		t.Error("Age() reported a value for an entry dated in the future")
	}
}

func TestEntryAgeIsTheTimeSinceItWasFetched(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	e := Entry{FetchedAt: now.Add(-90 * time.Second)}

	age, ok := e.Age(now)
	if !ok || age != 90*time.Second {
		t.Errorf("Age() = %v, %v; want 90s", age, ok)
	}
}

// An account that has been removed must not leave its usage behind, and a uuid
// that is added again must not inherit the headroom of its previous life.
func TestCachePrunesRemovedAndReAddedAccounts(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("still-here", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		c.Put("removed", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		c.Put("re-added", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	})

	err := WithCache(time.Second, func(c *Cache) error {
		c.Prune(map[string]time.Time{
			"still-here": now.Add(-24 * time.Hour),
			// Added again AFTER the cached reading was taken, so that reading
			// belongs to an account that no longer exists.
			"re-added": now.Add(time.Minute),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WithCache() error = %v", err)
	}

	c, _ := LoadCache()
	if _, ok := c.Get("still-here"); !ok {
		t.Error("Prune() dropped an account that is still managed")
	}
	if _, ok := c.Get("removed"); ok {
		t.Error("Prune() kept an entry for an account that has been removed")
	}
	if _, ok := c.Get("re-added"); ok {
		t.Error("Prune() let a re-added uuid inherit the previous account's headroom")
	}
}

// The write is a rename, so a reader never sees a partial document.
func TestCacheWritesAtomically(t *testing.T) {
	dir := isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("acct-1", Entry{Snapshot: mustParse(t, realBody), FetchedAt: now})
		return nil
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Matched against the writer's own pattern rather than a hand-spelled
	// ".tmp-": a literal here goes vacuous the moment cclink renames its temp,
	// and a check that stopped being able to fail would report success.
	for _, e := range entries {
		if ok, _ := filepath.Match(cclink.TempPattern(CacheFileName), e.Name()); ok {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}
	info, err := os.Stat(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no mode bits: os.Chmod there toggles the read-only attribute
	// and Stat reports 0666 whatever the file was created with. That is
	// documented rather than fixed in v1, and the store relies on the inherited
	// %USERPROFILE% ACL instead -- which is a property of the directory, not of
	// this file, and not something a Go test can assert here.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("usage.json mode = %o, want 600", perm)
		}
	}
}

// The cache is written by the daemon and read by every CLI invocation, so the
// read-modify-write is a cross-process operation and needs the lock, not just an
// atomic rename. Two sequential holds must both see the other's work.
func TestWithCacheSerializesReadModifyWrite(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if err := WithCache(time.Second, func(c *Cache) error {
		c.Put("a", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	}); err != nil {
		t.Fatalf("first hold: %v", err)
	}
	if err := WithCache(time.Second, func(c *Cache) error {
		if _, ok := c.Get("a"); !ok {
			t.Error("the second hold did not see the first hold's write")
		}
		c.Put("b", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	}); err != nil {
		t.Fatalf("second hold: %v", err)
	}

	c, _ := LoadCache()
	for _, k := range []string{"a", "b"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("entry %q is missing after two holds", k)
		}
	}
}

func TestWithCacheTakesARealCrossProcessLock(t *testing.T) {
	dir := isolate(t)

	var lockedDuringHold bool
	err := WithCache(time.Second, func(c *Cache) error {
		_, statErr := os.Stat(filepath.Join(dir, cacheLockDir))
		lockedDuringHold = statErr == nil
		return nil
	})
	if err != nil {
		t.Fatalf("WithCache() error = %v", err)
	}
	if !lockedDuringHold {
		t.Errorf("no lock existed at %s while the cache was being modified", filepath.Join(dir, cacheLockDir))
	}
	if _, err := os.Stat(filepath.Join(dir, cacheLockDir)); err == nil {
		t.Error("the lock survived the hold")
	}
}

// A callback that fails leaves the file alone: a half-computed poll result must
// not be persisted as though it were a reading.
func TestWithCacheDoesNotWriteWhenTheCallbackFails(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	_ = WithCache(time.Second, func(c *Cache) error {
		c.Put("a", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return nil
	})

	boom := errors.New("boom")
	err := WithCache(time.Second, func(c *Cache) error {
		c.Put("b", Entry{Snapshot: &Snapshot{}, FetchedAt: now})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithCache() error = %v, want boom", err)
	}

	c, _ := LoadCache()
	if _, ok := c.Get("b"); ok {
		t.Error("the failed callback's change was written anyway")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("the failed callback destroyed an existing entry")
	}
}

func TestCachePathIsInsideTheCcdadStore(t *testing.T) {
	dir := isolate(t)
	if got, want := mustPath(CachePath()), filepath.Join(dir, "usage.json"); got != want {
		t.Errorf("mustPath(CachePath()) = %q, want %q", got, want)
	}
	if !strings.HasPrefix(mustPath(CachePath()), mustPath(ccpath.StoreHome())) {
		t.Errorf("mustPath(CachePath()) = %q is outside the store at %q", mustPath(CachePath()), mustPath(ccpath.StoreHome()))
	}
}

// cclock catches a takeover two ways, and only one of them can see a steal that
// happened after the touch goroutine's last tick: the synchronous re-stat inside
// Release. Throwing Release's answer away reports success for the one write that
// actually raced.
func TestWithCacheReportsALockStolenDuringTheHold(t *testing.T) {
	dir := isolate(t)
	lockDir := filepath.Join(dir, cacheLockDir)

	err := WithCache(time.Second, func(c *Cache) error {
		c.Put("a", Entry{Snapshot: &Snapshot{}, FetchedAt: time.Now()})
		// Another process deems the lock stale and takes it over: rmdir then
		// mkdir, which is exactly what cclock.Acquire's steal path does. The
		// touch ticker has not fired yet, so only Release can notice.
		if err := os.Remove(lockDir); err != nil {
			return err
		}
		if err := os.Mkdir(lockDir, 0o700); err != nil {
			return err
		}
		past := time.Now().Add(-time.Second)
		return os.Chtimes(lockDir, past, past)
	})

	if err == nil {
		t.Fatal("WithCache() reported success for a write that raced a lock takeover")
	}
	if !errors.Is(err, cclock.ErrCompromised) {
		t.Errorf("error = %v, want it to carry cclock.ErrCompromised", err)
	}
	// The stealer's directory is not ours to remove.
	if _, statErr := os.Stat(lockDir); statErr != nil {
		t.Error("Release removed a lock directory that belonged to the process that took it over")
	}
	_ = os.Remove(lockDir)
}

// A callback that fails still reports its own error, and joining Release's
// answer must not swallow it.
func TestWithCacheKeepsTheCallbacksError(t *testing.T) {
	isolate(t)
	boom := errors.New("boom")

	err := WithCache(time.Second, func(c *Cache) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
}

// A stand-down and the schedule a poll earned are DIFFERENT facts with different
// writers: one is written by another account's poll, the other by this account's
// own, and folding them into one field would let whichever goroutine finished
// last erase the other. Both apply, and the later one wins.
func TestAStandDownAndAnEarnedScheduleBothApply(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	pushed := Entry{NextPollAt: now.Add(15 * time.Minute), StandDownUntil: now.Add(30 * time.Minute)}
	if got := pushed.PollAt(false); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("PollAt = %s, want the stand-down at %s", got, now.Add(30*time.Minute))
	}
	// A 429's floor is longer than the stand-down here, and a stand-down must
	// never shorten one: that would poll straight through the backoff.
	backedOff := Entry{NextPollAt: now.Add(45 * time.Minute), StandDownUntil: now.Add(30 * time.Minute)}
	if got := backedOff.PollAt(false); !got.Equal(now.Add(45 * time.Minute)) {
		t.Errorf("PollAt = %s, want the earned backoff at %s", got, now.Add(45*time.Minute))
	}
	// The live account is never held by one. A stand-down is written for the
	// accounts that do not matter right now, and a switch changes which one that
	// is — holding the account that just became live would blind the engine on
	// the only account a session can be cut off on, for half an hour.
	if got := pushed.PollAt(true); !got.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("PollAt = %s, want the live account's own schedule %s", got, now.Add(15*time.Minute))
	}
	if got := (Entry{StandDownUntil: now.Add(30 * time.Minute)}).PollAt(true); !got.IsZero() {
		t.Errorf("PollAt = %s, want no schedule at all for a live account that has none", got)
	}
}

// A short serve_ttl left on disk by an older ccdad is IGNORED, and that is the
// half of the danger-band fix that lives out here.
//
// The band used to write 30 s into this field so the scheduler's own freshness
// gate would stop refusing two of every three polls it asked for — which made the
// one structural bound on the hourly allowance a value the policy could rewrite.
// Every cache on every machine that has been in the band is carrying those rows
// right now, so a build that merely stopped WRITING the short TTL would keep
// honouring the one already on disk until the account's next successful poll —
// which is the very poll the short TTL lets through too early.
//
// A LONGER value is still honoured: a future ccdad slowing a reading down is
// telling this one something it does not know, and the safe direction for an
// unknown is the slower one.
func TestAShortServeTTLLeftByAnOlderCcdadIsIgnored(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	minuteOld := now.Add(-time.Minute)

	if got := (Entry{FetchedAt: minuteOld}).ScheduledTTL(); got != ServeTTL {
		t.Errorf("ScheduledTTL = %s, want the package default %s — an entry written "+
			"before this field existed has no opinion, which is not 'stale immediately'", got, ServeTTL)
	}
	// 30 s is the exact value the danger band used to persist.
	stale := Entry{FetchedAt: minuteOld, ServeTTL: 30 * time.Second}
	if got := stale.ScheduledTTL(); got != ServeTTL {
		t.Errorf("ScheduledTTL = %s, want the flat %s — a 30 s row written by an older "+
			"ccdad would otherwise keep unlocking the gate it was written to unlock", got, ServeTTL)
	}
	if !stale.FreshWithin(now, stale.ScheduledTTL()) {
		t.Error("a 60 s reading is stale under the flat TTL, so the old short TTL is still in force")
	}
	longer := Entry{FetchedAt: minuteOld, ServeTTL: 2 * ServeTTL}
	if got := longer.ScheduledTTL(); got != 2*ServeTTL {
		t.Errorf("ScheduledTTL = %s, want %s — a longer TTL is a slower cadence and the "+
			"safe direction for a value this build does not understand", got, 2*ServeTTL)
	}
	if !stale.Fresh(now) {
		t.Error("the hand-held gate moved with the scheduler's TTL")
	}
	// An entry dated in the future is a clock that moved backwards, not a fresh
	// reading, under any TTL.
	if (Entry{FetchedAt: now.Add(time.Hour)}).FreshWithin(now, ServeTTL) {
		t.Error("a reading from the future was served as fresh")
	}
}
