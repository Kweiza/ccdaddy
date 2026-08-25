package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// isolate points the store at a temporary directory, so a test never reads or
// writes the developer's own ~/.ccdad.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCDAD_HOME", dir)
	return dir
}

// ---- the three degradation paths: missing, unparseable, unreadable ----------

// A read failure is not an empty document. usage.LoadCache returns an empty
// cache and a nil error when os.ReadFile fails for any reason but ErrNotExist,
// and WithCache then saves over the file it could not read. A cache survives
// that: the next poll rebuilds it. Six hours of series does not.
//
// The bytes are the assertion, and a mode-000 file is what makes them one. An
// atomic write replaces a file by renaming a sibling over it, and a rename cares
// about the DIRECTORY's permissions and not the target's -- measured here, a
// rename onto a mode-000 file succeeds and the old contents are gone. So this is
// a document that a caller which mistook "cannot read" for "empty" would
// genuinely destroy, which is the whole reason the arm exists.
func TestAReadFailureRefusesToWrite(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := Record(time.Second, "uuid-a", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}
	p := mustPath(Path())
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(p, 0o000); err != nil {
		t.Skipf("this host will not make a file unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if _, err := os.ReadFile(p); err == nil {
		t.Skip("this host reads through a mode-000 file (running as root?)")
	}

	if err := WithHistory(time.Second, func(h *History) error {
		h.Put("uuid-a", Sample{At: now.Add(time.Minute)})
		return nil
	}); err == nil {
		t.Fatal("WithHistory returned nil on an unreadable document; it must refuse")
	}

	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("the unreadable document was overwritten:\n want %s\n  got %s", want, got)
	}
}

// The refusal happens before the caller's mutation runs, not after it. That is
// what makes the arm portable: on a host where a mode-000 file is still readable
// -- Windows, or root -- the sibling test above skips, and only this one is left
// to say that a document nobody could read is never the document a transaction
// is applied to.
func TestAnUnreadableDocumentIsNeverHandedToTheCaller(t *testing.T) {
	isolate(t)
	p := mustPath(Path())
	if err := os.MkdirAll(p, 0o700); err != nil { // a directory where a file goes
		t.Fatal(err)
	}
	ran := false
	err := WithHistory(time.Second, func(h *History) error {
		ran = true
		h.Put("uuid-a", Sample{At: time.Unix(1, 0)})
		return nil
	})
	if err == nil {
		t.Fatal("WithHistory returned nil on an unreadable document; it must refuse")
	}
	if ran {
		t.Error("the transaction ran against a document that could not be read")
	}
	fi, statErr := os.Stat(p)
	if statErr != nil || !fi.IsDir() {
		t.Fatalf("the unreadable path was replaced: %v %v", fi, statErr)
	}
}

// A document that parses as nonsense IS overwritten, exactly as the cache's is
// -- this is the other half of the rule and the two must not be conflated.
func TestAnUnparseableDocumentIsOverwritten(t *testing.T) {
	isolate(t)
	p := mustPath(Path())
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{{{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WithHistory(time.Second, func(h *History) error {
		h.Put("uuid-a", Sample{At: time.Unix(1, 0)})
		return nil
	}); err != nil {
		t.Fatalf("WithHistory refused an unparseable document: %v", err)
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h.Series("uuid-a", time.Time{})); got != 1 {
		t.Fatalf("samples = %d, want 1", got)
	}
}

// The unparseable arm keeps the reason for `ccdad doctor` rather than throwing
// it away, and the missing-file arm is not an error at all: a fleet that has
// never been polled has no series, which is a fact and not a fault.
func TestLoadHistoryReportsTheReasonItCameBackEmpty(t *testing.T) {
	isolate(t)
	h, err := LoadHistory()
	if err != nil {
		t.Fatalf("LoadHistory() on a store with no file: %v", err)
	}
	if h.LoadError() != nil {
		t.Errorf("LoadError() = %v on a missing file, want nil", h.LoadError())
	}
	if got := h.Series("uuid-a", time.Time{}); got != nil {
		t.Errorf("Series() = %v on a missing file, want nil", got)
	}

	if err := os.WriteFile(mustPath(Path()), []byte("{{{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err = LoadHistory()
	if err != nil {
		t.Fatalf("LoadHistory() on an unparseable file: %v", err)
	}
	if h.LoadError() == nil {
		t.Error("LoadError() = nil after an unparseable document; doctor would report nothing")
	}
}

// ---- retention --------------------------------------------------------------

// Both bounds bite, and each must bite alone: a test that trips both at once
// cannot tell which one is doing the work.
func TestRetentionDropsByAgeWithTheCountUnderItsCap(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	write := []Sample{
		{At: now.Add(-9 * time.Hour)}, // older than the 8h bound
		{At: now.Add(-7 * time.Hour)},
		{At: now.Add(-1 * time.Hour)},
	}
	for _, s := range write {
		if err := Record(time.Second, "uuid-a", s, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := h.Series("uuid-a", time.Time{})
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2 (the 9h-old one is past the age bound)", len(got))
	}
	if !got[0].At.Equal(now.Add(-7 * time.Hour)) {
		t.Errorf("oldest surviving sample = %v", got[0].At)
	}
}

// retain must still bracket the measurement window when the poller is leaving
// the longest gap its own policy permits, and that gap is not any one number in
// pollpolicy: it is what Next returns for an account AIMD has parked at the
// congestion ceiling, jittered, and then multiplied by Share for the accounts
// sharing one identity. Reconstructing it by calling those two functions rather
// than by restating their constants is what makes this a check -- a rule change
// inside either of them lands here, where a copied-out figure would not.
//
// This is the arithmetic the first version of retain got wrong: at six hours it
// named Share and then did not apply it, which left the bound twelve minutes
// short for an identity of four and an hour and eighteen minutes short for one
// of six.
func TestRetainCoversTheLongestGapThePolicyPermits(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	in := pollpolicy.Input{
		Now:       now,
		Reading:   pollpolicy.Reading{BindingPct: 50, Known: true},
		Threshold: 85,
	}
	// Repeated 429s are what park the AIMD estimate at the ceiling; one is not
	// enough, because the increase is multiplicative from wherever it stands.
	var st pollpolicy.State
	for i := 0; i < 32; i++ {
		st = pollpolicy.RateLimited(st, now, 0, false)
	}
	// The top of the jitter band, from inside the [0,1) the argument is
	// documented to be: the widest gap is the one retention has to survive.
	at, _ := pollpolicy.Next(st, in, math.Nextafter(1, 0))
	if pollpolicy.InDangerBand(in) {
		t.Fatal("this fixture is inside the danger band, where Share does not divide; it cannot measure the worst gap")
	}
	gap := pollpolicy.Share(at.Sub(now), maxIdentityAccounts, in)

	if want := MeasuredSpan + gap; retain < want {
		t.Fatalf("retain = %v, want at least %v (%v of window plus a %v gap for %d accounts on one identity)",
			retain, want, MeasuredSpan, gap, maxIdentityAccounts)
	}
}

func TestRetentionDropsByCountWithTheAgeUnderItsBound(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	// 600 samples inside one hour: every one is inside the age bound, so only
	// the count bound can be what trims them.
	for i := 0; i < 600; i++ {
		s := Sample{At: now.Add(-time.Hour).Add(time.Duration(i) * time.Second)}
		if err := Record(time.Second, "uuid-a", s, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := h.Series("uuid-a", time.Time{})
	if len(got) != 512 {
		t.Fatalf("samples = %d, want 512", len(got))
	}
	// The newest are the ones kept. A bound that trimmed the other end would
	// still report 512 and would throw away the only readings a rate is
	// measured from.
	if !got[len(got)-1].At.Equal(now.Add(-time.Hour).Add(599 * time.Second)) {
		t.Errorf("newest surviving sample = %v, want the last one written", got[len(got)-1].At)
	}
}

// The two account-scoped exclusions are on the READ side. usage.Cache.Prune is
// not the precedent it looks like: it has one caller, switcher.Evaluate, which
// prunes an in-memory copy and never writes it back.
func TestSamplesBeforeAddedAtAreInvisible(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	added := now.Add(-2 * time.Hour)
	for _, at := range []time.Time{now.Add(-3 * time.Hour), now.Add(-time.Hour)} {
		if err := Record(time.Second, "uuid-a", Sample{At: at}, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h.Series("uuid-a", added)); got != 1 {
		t.Fatalf("visible samples = %d, want 1 -- a re-added uuid must not inherit the old one's slope", got)
	}
	if got := len(h.Series("uuid-a", time.Time{})); got != 2 {
		t.Fatalf("stored samples = %d, want 2 -- the exclusion is a read filter, not a deletion", got)
	}
}

// Retention sweeps EVERY account, not just the one being written, and an
// account left with nothing is removed from the document rather than left as a
// bare key.
//
// Both halves matter and neither is visible from a one-account fixture. The
// sweep is the only thing that ever expires an account that stopped being
// polled -- removed by `ccdad remove`, logged out, or simply never scheduled
// again -- because nothing else will ever come back to write it, and it is what
// lets the read side skip filtering the managed set. The deletion is what stops
// every uuid the machine has ever seen from accumulating in the file forever.
func TestPruningSweepsAnAccountNobodyWritesAnyMore(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// uuid-a's only reading was taken long enough ago to be past the age bound
	// by the time uuid-b is written. It has to be recorded at its own `now`, or
	// the transaction that writes it prunes it in the same breath and there is
	// nothing left to sweep.
	stale := now.Add(-retain - time.Hour)
	if err := Record(time.Second, "uuid-a", Sample{At: stale}, stale); err != nil {
		t.Fatal(err)
	}
	if got := len(mustLoad(t).Series("uuid-a", time.Time{})); got != 1 {
		t.Fatalf("uuid-a samples before the sweep = %d, want 1", got)
	}

	if err := Record(time.Second, "uuid-b", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}

	h := mustLoad(t)
	if got := h.Series("uuid-a", time.Time{}); got != nil {
		t.Errorf("uuid-a still has %d samples after a write to uuid-b; the sweep only touched the account it was handed", len(got))
	}
	if got := len(h.Series("uuid-b", time.Time{})); got != 1 {
		t.Errorf("uuid-b samples = %d, want 1 -- the sweep took the account it was written for", got)
	}

	// The encoded document, not just the reader's view: an account emptied but
	// left in place is invisible to Series and still on disk forever.
	raw, err := os.ReadFile(mustPath(Path()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Accounts["uuid-a"]; ok {
		t.Errorf("uuid-a survives on disk as %s; an emptied series must leave the document", v)
	}
	if _, ok := doc.Accounts["uuid-b"]; !ok {
		t.Error("uuid-b is missing from the document")
	}
}

// An account the store no longer manages is excluded structurally: nothing
// enumerates the document, so the only way to reach a series is to name a uuid,
// and a caller that has dropped the account never names it.
func TestAnUnknownAccountHasNoSeries(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := Record(time.Second, "uuid-a", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Series("uuid-gone", time.Time{}); got != nil {
		t.Errorf("Series() for an unmanaged uuid = %v, want nil", got)
	}
}

// ---- the document ------------------------------------------------------------

// A sample carries readings, not a rendering: percentages, resets, and the
// paid-usage half survive the disk unchanged, and an account with no monthly
// limit round-trips as "no limit" rather than as a limit of zero.
func TestASampleRoundTripsThroughDisk(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	limit := 100.0
	want := Sample{
		At: now,
		Windows: map[usage.WindowName]Reading{
			usage.WindowFiveHour: {Pct: 24, Reset: now.Add(90 * time.Minute)},
			usage.WindowSevenDay: {Pct: 85, Reset: now.Add(80 * time.Hour)},
			// A window whose reset the endpoint did not report keeps a zero
			// time, which is not a reset at the epoch.
			usage.ScopedWindowName(usage.ScopeModel, "Fable"): {Pct: 10},
		},
		Credit: &Credit{Used: 12.5, Limit: &limit, Currency: "USD"},
	}
	if err := Record(time.Second, "uuid-a", want, now); err != nil {
		t.Fatal(err)
	}
	unlimited := Sample{
		At:     now.Add(time.Minute),
		Credit: &Credit{Used: 3, Currency: "USD"},
	}
	if err := Record(time.Second, "uuid-a", unlimited, now); err != nil {
		t.Fatal(err)
	}

	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := h.Series("uuid-a", time.Time{})
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2", len(got))
	}
	if !got[0].At.Equal(want.At) {
		t.Errorf("at = %v, want %v", got[0].At, want.At)
	}
	for name, w := range want.Windows {
		g, ok := got[0].Windows[name]
		if !ok {
			t.Fatalf("window %q was dropped", name)
		}
		if g.Pct != w.Pct || !g.Reset.Equal(w.Reset) {
			t.Errorf("window %q = %+v, want %+v", name, g, w)
		}
	}
	if got[0].Credit == nil || got[0].Credit.Limit == nil || *got[0].Credit.Limit != limit {
		t.Errorf("credit = %+v, want a limit of %v", got[0].Credit, limit)
	}
	if got[0].Credit.Currency != "USD" || got[0].Credit.Used != 12.5 {
		t.Errorf("credit = %+v, want 12.5 USD", got[0].Credit)
	}
	if got[1].Credit == nil || got[1].Credit.Limit != nil {
		t.Errorf("credit = %+v, want a nil limit -- no monthly limit means unlimited, not zero", got[1].Credit)
	}
}

// An unreadable window is absent, and absent is what the encoded document says.
// A zero in its place would read as "nothing used" and would flatten a rate.
func TestAnAbsentWindowStaysAbsentOnDisk(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	s := Sample{At: now, Windows: map[usage.WindowName]Reading{usage.WindowFiveHour: {Pct: 24}}}
	if err := Record(time.Second, "uuid-a", s, now); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(mustPath(Path()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version  int `json:"version"`
		Accounts map[string]struct {
			Samples []struct {
				Windows map[string]json.RawMessage `json:"windows"`
				Credit  json.RawMessage            `json:"credit"`
			} `json:"samples"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the document on disk does not parse: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	sample := doc.Accounts["uuid-a"].Samples[0]
	if _, ok := sample.Windows[string(usage.WindowSevenDay)]; ok {
		t.Error("an unreported window was written out; it must be absent, never zero")
	}
	if sample.Credit != nil {
		t.Errorf("credit = %s, want absent when the account reported none", sample.Credit)
	}
}

// Put is keyed on (uuid, At) so a write that WithHistory abandoned can be
// re-applied. Two points at the same instant would be a rate divided by zero.
func TestPutReplacesTheSampleAtTheSameInstant(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := Sample{At: now, Windows: map[usage.WindowName]Reading{usage.WindowFiveHour: {Pct: 24}}}
	second := Sample{At: now, Windows: map[usage.WindowName]Reading{usage.WindowFiveHour: {Pct: 31}}}
	for _, s := range []Sample{first, second} {
		if err := Record(time.Second, "uuid-a", s, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := h.Series("uuid-a", time.Time{})
	if len(got) != 1 {
		t.Fatalf("samples = %d, want 1 -- the same instant must not appear twice", len(got))
	}
	if got[0].Windows[usage.WindowFiveHour].Pct != 31 {
		t.Errorf("pct = %v, want the replacement's 31", got[0].Windows[usage.WindowFiveHour].Pct)
	}
}

// Two accounts are two series. A document keyed on anything the store recompacts
// would attribute one account's burn to another after a `ccdad remove`.
func TestAccountsDoNotShareASeries(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := Record(time.Second, "uuid-a", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Record(time.Second, "uuid-b", Sample{At: now.Add(time.Duration(i) * time.Minute)}, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h.Series("uuid-a", time.Time{})); got != 1 {
		t.Errorf("uuid-a samples = %d, want 1", got)
	}
	if got := len(h.Series("uuid-b", time.Time{})); got != 2 {
		t.Errorf("uuid-b samples = %d, want 2", got)
	}
}

// Series hands out a copy. A caller that sorts, trims or overwrites what it got
// must not be editing what the next reader sees.
func TestSeriesReturnsACopyOldestFirst(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := WithHistory(time.Second, func(h *History) error {
		// Out of order on purpose: nothing promises the writer appends in
		// time order, and every reader downstream measures a slope.
		h.Put("uuid-a", Sample{At: now})
		h.Put("uuid-a", Sample{At: now.Add(-time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	got := h.Series("uuid-a", time.Time{})
	if len(got) != 2 || !got[0].At.Before(got[1].At) {
		t.Fatalf("Series() = %v, want oldest first", got)
	}
	got[0] = Sample{At: now.Add(-99 * time.Hour)}
	again := h.Series("uuid-a", time.Time{})
	if !again[0].At.Equal(now.Add(-time.Hour)) {
		t.Errorf("the second read saw the first caller's edit: %v", again[0].At)
	}
}

// The lock exists at all, at the path every other process would look for it,
// and it does not survive the hold. Nothing else in this file would notice a
// WithHistory that took no lock: every other test runs one transaction at a
// time, so the mutual exclusion the daemon's append and a hand-held refresh
// depend on would be gone with the suite green.
func TestWithHistoryTakesARealCrossProcessLock(t *testing.T) {
	root := isolate(t)

	var lockedDuringHold bool
	if err := WithHistory(time.Second, func(h *History) error {
		_, statErr := os.Stat(filepath.Join(root, lockDir))
		lockedDuringHold = statErr == nil
		return nil
	}); err != nil {
		t.Fatalf("WithHistory() error = %v", err)
	}
	if !lockedDuringHold {
		t.Errorf("no lock existed at %s while the series was being modified", filepath.Join(root, lockDir))
	}
	if _, err := os.Stat(filepath.Join(root, lockDir)); err == nil {
		t.Error("the lock survived the hold")
	}
}

// The lock is checked for compromise BEFORE the save, not after it. WithCache
// saves and only then releases, and a release is the earliest cclock will tell
// an unasking caller anything -- so a writer stalled past the stale threshold
// saves over the writer that took the lock away from it. A cache loses one poll
// that way; a series loses every sample the new holder appended.
//
// The takeover is staged and the transaction then returns nil, which is the
// point of the fixture rather than an incidental. A fixture that reported its
// own failures by returning an error would make WithHistory abandon the save
// for the ORDINARY reason, leaving exactly the one sample this test asserts --
// so a broken fixture would read as a pass. Here the only thing that can
// abandon the write is the ownership check, and a fixture that did not manage
// to stage a takeover fails on `staged` instead.
//
// Nothing sleeps. cclock's touch goroutine has not ticked yet and cannot have
// noticed, so a check that only consulted Compromised() would see an open
// channel and go ahead -- which is the real failure this guards, because a
// process resuming from a suspend reaches its write before that goroutine is
// scheduled no matter how short the interval is.
//
// The error is not the assertion, because Release reports the takeover either
// way. The file is: the second sample must not be on disk.
func TestATakenLockAbandonsTheWrite(t *testing.T) {
	root := isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := Record(time.Second, "uuid-a", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}

	var stageErr error
	staged := false
	err := WithHistory(time.Second, func(h *History) error {
		h.Put("uuid-a", Sample{At: now.Add(time.Minute)})

		// What a stealer does: the holder looked stale, so its lock directory
		// was removed and re-created by someone else. The path comes from the
		// constant the code under test uses, so a renamed lock fails here
		// rather than quietly testing nothing.
		dir := filepath.Join(root, lockDir)
		if _, err := os.Stat(dir); err != nil {
			stageErr = fmt.Errorf("no lock at %s during the hold: %w", dir, err)
			return nil
		}
		if err := os.Remove(dir); err != nil {
			stageErr = err
			return nil
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			stageErr = err
			return nil
		}
		// The new directory's mtime is set explicitly, and the fixture does not
		// work without it. cclock recognizes a takeover by comparing mtimes, and
		// filesystem timestamps are granular: measured on this tree, a remove
		// and a re-create inside the same millisecond hand back a byte-identical
		// mtime and the steal is invisible. A real stealer takes a lock it
		// deemed stale, so its directory carries a plainly different time.
		stolen := time.Now().Add(-time.Second)
		if err := os.Chtimes(dir, stolen, stolen); err != nil {
			stageErr = err
			return nil
		}
		staged = true
		return nil
	})
	if stageErr != nil {
		t.Fatalf("the takeover could not be staged: %v", stageErr)
	}
	if !staged {
		t.Fatal("the takeover was never staged, so this test proved nothing")
	}
	if err == nil {
		t.Fatal("WithHistory returned nil after its lock was taken")
	}

	h, lerr := LoadHistory()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got := len(h.Series("uuid-a", time.Time{})); got != 1 {
		t.Fatalf("samples = %d, want 1 -- the write went ahead over the lock's new owner", got)
	}
}

// fn returning an error leaves the file exactly as it was: a poll that failed
// halfway must not persist half a reading.
func TestAFailedTransactionWritesNothing(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := Record(time.Second, "uuid-a", Sample{At: now}, now); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the poll failed halfway")
	if err := WithHistory(time.Second, func(h *History) error {
		h.Put("uuid-a", Sample{At: now.Add(time.Minute)})
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("WithHistory() error = %v, want %v", err, boom)
	}
	h, err := LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(h.Series("uuid-a", time.Time{})); got != 1 {
		t.Errorf("samples = %d, want 1 -- an aborted transaction wrote anyway", got)
	}
}

// The sizes maxSamples is justified by are measured, so they are measured here
// too. A figure quoted in a comment and never executed is how the first version
// of that comment came to claim 150 KB for a document that cannot be smaller
// than about 800 KB at the same cap -- and it was the whole argument that the
// cap costs nothing.
//
// It writes through save, not through a hand-rolled json.Marshal, so the
// encoder, the struct tags and the omitempty decisions are all the real ones.
// It does not write through Record: filling the cap that way is 3072 lock
// acquisitions to measure a document shape that no lock affects.
//
// The tolerance is wide enough for a Go release that reorders a map or shortens
// a float, and narrow enough that adding a field to Sample lands here.
func TestTheDocumentSizeAtTheCapIsWhatMaxSamplesClaims(t *testing.T) {
	all := []usage.WindowName{
		usage.WindowFiveHour, usage.WindowSevenDay, usage.WindowSevenDayOpus,
		usage.WindowSevenDaySonnet, usage.WindowSevenDayOAuthApps, usage.WindowCinderCove,
	}
	for _, c := range []struct {
		name      string
		windows   []usage.WindowName
		perSample int
		atCapMB   float64
	}{
		{"three windows and a credit", all[:3], 267, 0.78},
		{"six windows and a credit", all, 455, 1.33},
	} {
		t.Run(c.name, func(t *testing.T) {
			one := encodedSize(t, 1, 1, c.windows)
			two := encodedSize(t, 1, 2, c.windows)
			if got := two - one; !within(float64(got), float64(c.perSample), 0.05) {
				t.Errorf("a sample costs %d bytes, and maxSamples' rationale says %d", got, c.perSample)
			}
			atCap := encodedSize(t, maxIdentityAccounts, maxSamples, c.windows)
			if got := float64(atCap) / (1024 * 1024); !within(got, c.atCapMB, 0.05) {
				t.Errorf("%d accounts at the cap encode to %.2f MB, and maxSamples' rationale says %.2f MB",
					maxIdentityAccounts, got, c.atCapMB)
			}
		})
	}
}

func within(got, want, frac float64) bool { return math.Abs(got-want) <= want*frac }

// encodedSize writes a full document through save and reports what landed.
func encodedSize(t *testing.T, accounts, samples int, windows []usage.WindowName) int {
	t.Helper()
	root := t.TempDir()
	limit := 100.0
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h := &History{data: historyFile{Version: 1, Accounts: map[string]Account{}}}
	for a := range accounts {
		// A real key is a UUID, and its 36 characters are a measurable share of
		// a small account's series.
		uuid := fmt.Sprintf("%08x-1234-4567-89ab-%012x", a, a)
		var acct Account
		for i := range samples {
			s := Sample{
				At:      at.Add(time.Duration(i) * pollpolicy.MinInterval),
				Windows: map[usage.WindowName]Reading{},
				Credit:  &Credit{Used: 12.5, Limit: &limit, Currency: "USD"},
			}
			for w, n := range windows {
				s.Windows[n] = Reading{Pct: 24.5, Reset: at.Add(time.Duration(w+1) * time.Hour)}
			}
			acct.Samples = append(acct.Samples, s)
		}
		h.data.Accounts[uuid] = acct
	}
	if err := h.save(root); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Size())
}
