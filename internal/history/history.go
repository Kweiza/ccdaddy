// Package history persists the usage readings the poller was already taking, so
// a burn rate can be measured from more than one of them.
//
// It is a separate document from the usage cache rather than a field on
// usage.Entry because the two have different write shapes: usage.json is a small
// file rewritten whole on every poll and read by three commands on every
// invocation, and a series that accumulates belongs to neither of those costs.
// This is the only file in the ccdad store that grows, which is why it carries
// retention bounds and usage.json carries none.
//
// It is authoritative for exactly one thing: readings STRICTLY OLDER than the
// current one. Every present-tense figure -- a percentage left, a credit
// balance, a window's reset -- is read from the usage cache, so two commands
// cannot disagree about one number. This file supplies slopes; usage.json
// supplies levels.
//
// No reading here costs a request. Samples are the polls the daemon already
// makes, appended where a fresh snapshot is already being stored, so the
// endpoint's allowance and the poll cadence are untouched. Nothing samples while
// the daemon is stopped, and the resolution of a series is the poll cadence
// rather than any fixed interval.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"github.com/Kweiza/ccdaddy/internal/zone"
)

const (
	// FileName is the series, beside usage.json in the store root.
	FileName = "history.json"

	// lockDir is a DIRECTORY, because that is what cclock's mutex is: a mkdir,
	// not a file.
	lockDir = FileName + ".lock"

	// lockStale is how long the lock's mtime may go unadvanced before another
	// process may take it over. It matches the usage cache's for the same
	// reason: a write here is a sub-second operation, so this only ever matters
	// after a crash, and it must stay at least twice cclock's touch interval or
	// a live holder's lock goes stale by its own definition between two touches.
	lockStale = 30 * time.Second

	// MeasuredSpan is how far back a burn rate is measured. It is exported
	// because two rules depend on it and they must not be able to disagree:
	// internal/forecast measures over exactly this span, and retain below is
	// sized against it. A second declaration of four hours in the measuring
	// package would drift the first time either figure moved, and the drift
	// would be silent -- a longer measurement over unchanged retention simply
	// loses the oldest end of its own window and reports a narrower span with
	// no error anywhere.
	//
	// Nothing in this package measures anything. The figure lives here because
	// retention cannot be sized without it and it can be: the measuring package
	// already imports this one, and the reverse would be a cycle.
	//
	// Four hours makes the rate a speedometer rather than an odometer, and that
	// is the trade being made knowingly: four idle hours read as "holds" and
	// four hard hours read as "dry in two days", and both are honest reports of
	// the last four hours rather than of the week. Every surface that prints
	// the figure prints the span and the reading count beside it for exactly
	// that reason. It is also where an ordinary cadence clears the measuring
	// package's contribution gates with room -- the poller's sustained floor is
	// 180 s, which offers eighty readings inside the window -- while no span at
	// all guarantees the three readings a rate needs, which is why that package
	// carries a count gate as well.
	//
	// A rate over the trailing four hours needs a sample at or before the
	// four-hour mark, not merely one inside it, so retention has to reach
	// further back than the measurement does.
	MeasuredSpan = 4 * time.Hour

	// maxIdentityAccounts is how many accounts sharing one identity retain and
	// maxSamples are sized for. Six is the largest pool this tree has been run
	// against.
	maxIdentityAccounts = 6

	// retain is how far back samples are kept: MeasuredSpan plus the longest
	// gap the poller can leave, so the oldest end of the window still has a
	// sample bracketing it after the worst possible silence.
	//
	// That gap is NOT pollpolicy's largest interval. The ceiling AIMD parks a
	// repeatedly rate-limited account at is Post429MaxInterval, 1800 s, and
	// pollpolicy.Next jitters it up by JitterFrac before returning it -- 1980 s
	// at the top of the band. But the daemon then passes that already-jittered
	// figure to pollpolicy.Share, which multiplies it by the number of accounts
	// on one identity, because the endpoint's allowance belongs to the identity
	// and not to the account, and nothing clamps the product. Six accounts is
	// therefore 11880 s, 3 h 18 m, between two consecutive readings of any one
	// of them. Four hours plus that is 7 h 18 m; eight hours is the next whole
	// hour above it. TestRetainCoversTheLongestGapThePolicyPermits recomputes
	// the gap through those two functions rather than from their inputs, so a
	// rule change inside either one fails rather than silently invalidating this
	// paragraph.
	//
	// An identity larger than six leaves a longer gap than this and loses the
	// oldest end of the window. The measurement is then made over a shorter span
	// than four hours, which is a narrower answer rather than a wrong one.
	//
	// It is deliberately not derived from the 600 s idle cadence: that is not
	// the longest interval the poller reaches, and sizing against it drops the
	// oldest end of the window on any fleet that has just been rate limited --
	// exactly when a burn rate is worth having.
	retain = 8 * time.Hour

	// maxSamples is a hard bound no cadence can argue past, so a poller that
	// somehow ran far faster than the policy permits could not grow this file
	// without limit. It is not the bound that normally bites: pollpolicy's
	// sustained floor is MinInterval, 180 s, so eight hours of the fastest
	// permitted polling is 160 samples per account, and reaching 512 would take
	// a reading every 56 s.
	//
	// The sizes are measured by encoding this document's own shape rather than
	// estimated, and TestTheDocumentSizeAtTheCapIsWhatMaxSamplesClaims keeps
	// measuring them: a sample costs 287 bytes carrying three windows and a
	// credit, 490 carrying all six the usage schema names. Six accounts at 160
	// samples each is therefore 270-460 KB, and six accounts pinned at this cap
	// would be 0.84-1.44 MB -- more again on a plan that reports scoped weekly
	// caps too, whose names are longer than the fixed ones. That upper figure is
	// why the cap is a cap and not a target: LoadHistory reads and unmarshals
	// the whole document, with no lock and no partial read, on every command
	// that forecasts.
	//
	// Those are the figures for a machine whose zone is not UTC, and they are
	// the ones to size against. save renders the whole document in the
	// machine's zone, and an offset is six characters where Z is one, so a
	// sample carrying three timestamps costs 20 bytes more there than it does
	// on a UTC machine -- 267 and 455 bytes, 0.78 and 1.33 MB, measured. The
	// bound has to be the worse of the two, and every machine that is not a CI
	// runner is the worse of the two.
	maxSamples = 512
)

// Reading is one window's reading inside one sample.
type Reading struct {
	// Pct is the window's utilization in percentage points, as the endpoint
	// reported it. A window whose utilization could not be read has no Reading
	// at all rather than a zero one: nothing read is not nothing used.
	Pct float64 `json:"pct"`

	// Reset is when the window rolls over, recorded truncated to the minute.
	//
	// The endpoint regenerates the sub-second part of resets_at on every
	// request, so two readings of the same unrolled window carry different
	// microseconds. A reader that segments a series on "the reset changed"
	// would then see every consecutive pair as a rollover and measure a rate of
	// zero forever. Truncating loses nothing, because no window rolls over on a
	// sub-minute boundary.
	//
	// The zero time means the reading reported no reset at all, which is not the
	// same as a reset at the epoch.
	Reset time.Time `json:"reset,omitzero"`
}

// Credit is the paid-usage half of a sample, present only when the account
// reports extra usage enabled and a readable balance. An unreadable balance
// omits the whole struct: a credit figure that cannot be read refuses, it never
// defaults to zero.
type Credit struct {
	// Used is the paid usage spent so far, in Currency's major unit.
	Used float64 `json:"used"`

	// Limit is the monthly cap, in Currency's major unit. It is nil when the
	// account reports no monthly limit, which means unlimited -- a zero would
	// mean a cap of nothing, which is the opposite verdict.
	Limit *float64 `json:"limit,omitempty"`

	// Currency is recorded on every credit sample rather than once per account
	// because it is a per-account field, and two accounts' figures do not add up
	// without it.
	Currency string `json:"currency,omitempty"`
}

// Sample is one poll's readings for one account.
type Sample struct {
	// At is when the endpoint answered -- the reading's FetchedAt -- and never
	// the moment of the write. A sample is evidence about when a reading was
	// taken, and the interval between two of them is the denominator of every
	// rate measured from this file.
	At time.Time `json:"at"`

	// Windows carries one entry per window whose utilization was readable. An
	// unreadable window is ABSENT, never zero.
	Windows map[usage.WindowName]Reading `json:"windows,omitempty"`

	// Credit is nil unless this account reported paid usage with a readable
	// balance.
	Credit *Credit `json:"credit,omitempty"`
}

// Account is one account's series, oldest first.
type Account struct {
	Samples []Sample `json:"samples,omitempty"`
}

// historyFile is the on-disk shape.
type historyFile struct {
	// Version is written as 1 and read back without being validated, following
	// usage.json and strategy.json: a document written by a later release must
	// degrade rather than error.
	Version int `json:"version"`

	// Accounts is keyed by account UUID and never by idx. store.sortAndReindex
	// recompacts idx on every removal, so a series keyed on it would silently
	// attribute one account's burn to another after any `ccdad remove`.
	Accounts map[string]Account `json:"accounts"`
}

// History is the parsed series document.
type History struct {
	data    historyFile
	loadErr error
}

// Path is where the series lives.
func Path() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, FileName), nil
}

// storeRoot returns ccdad's state directory, refusing a relative one for the
// same reason store.Open does: a relative root puts the series in whatever
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

// LoadError is why the document came back empty when a file did exist. It is
// only ever a parse failure -- a read failure is returned from LoadHistory
// rather than stashed -- and it is not fatal: a series that cannot be parsed
// leaves every account without a measured rate, which every reader already
// renders as unknown. `ccdad doctor` reports it so corruption does not stay
// invisible.
func (h *History) LoadError() error { return h.loadErr }

// Series returns the samples for one account, oldest first, with the two
// account-scoped exclusions of the read side already applied. addedAt is the
// account's AddedAt as the store records it; a zero value applies no lower
// bound, since no sample can predate it.
//
// Those exclusions are on the read side because the writer cannot make them.
// The one place a sample is appended receives a single account and no store
// handle, so it does not know the managed set, and threading one in would change
// a signature five call sites deep in order to delete rows nobody reads. An
// account that has left the store is excluded structurally instead: nothing
// enumerates this document, so the only way to reach a series is to name a uuid,
// and its samples leave the file when the age bound expires them a few hours
// later.
//
// A sample older than addedAt belonged to a PREVIOUS account at the same uuid --
// removed, then added again. Letting one through would hand a fresh login the
// slope its predecessor's spending earned.
//
// The result is a fresh slice: a caller that sorts, trims or overwrites it must
// not be editing what the next reader sees.
func (h *History) Series(uuid string, addedAt time.Time) []Sample {
	acct, ok := h.data.Accounts[uuid]
	if !ok {
		return nil
	}
	out := make([]Sample, 0, len(acct.Samples))
	for _, s := range acct.Samples {
		if s.At.Before(addedAt) {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortStableFunc(out, func(a, b Sample) int { return a.At.Compare(b.At) })
	return out
}

// Put stores one sample for one account, keyed on (uuid, At): a sample whose At
// is already present is replaced in place rather than appended.
//
// That key is what makes an abandoned write safe to re-apply. WithHistory
// abandons its save when the lock was taken from it, so the sample the caller
// handed over may only land in a later transaction; keyed on nothing, the retry
// would leave two points at the same instant, and a rate measured between them
// divides by zero.
func (h *History) Put(uuid string, s Sample) {
	if h.data.Accounts == nil {
		h.data.Accounts = map[string]Account{}
	}
	acct := h.data.Accounts[uuid]
	for i := range acct.Samples {
		if acct.Samples[i].At.Equal(s.At) {
			acct.Samples[i] = s
			h.data.Accounts[uuid] = acct
			return
		}
	}
	acct.Samples = append(acct.Samples, s)
	h.data.Accounts[uuid] = acct
}

// prune applies both retention bounds to every account, keeping the newest, and
// drops an account with nothing left so an emptied series does not sit in the
// document as a bare key forever.
//
// Both bounds run inside the write transaction, unlike the account-scoped
// exclusions Series applies: these two are the file's only defence against
// unbounded growth, and a bound applied on the read side would leave the growth
// on disk. It is also what eventually clears a removed account, which no reader
// asks about but which the file would otherwise carry indefinitely.
//
// It sweeps every account rather than only the one just written, because an
// account that has stopped being polled -- removed, logged out, or simply not
// scheduled -- is exactly the one whose samples nothing else will ever come back
// to expire.
func (h *History) prune(now time.Time) {
	cutoff := now.Add(-retain)
	for uuid, acct := range h.data.Accounts {
		kept := make([]Sample, 0, len(acct.Samples))
		for _, s := range acct.Samples {
			if s.At.Before(cutoff) {
				continue
			}
			kept = append(kept, s)
		}
		slices.SortStableFunc(kept, func(a, b Sample) int { return a.At.Compare(b.At) })
		if len(kept) > maxSamples {
			kept = kept[len(kept)-maxSamples:]
		}
		if len(kept) == 0 {
			delete(h.data.Accounts, uuid)
			continue
		}
		acct.Samples = kept
		h.data.Accounts[uuid] = acct
	}
}

// LoadHistory reads the series without taking a lock.
//
// No lock is needed to read: every write is a rename, so a reader sees one whole
// version of the document or another, never a torn one.
//
// It has three arms, and the middle one is where this package parts company with
// the usage cache. A missing file is an empty series and no error: a fleet that
// has never been polled has no history, which is a fact and not a fault. A file
// that cannot be PARSED is an empty series too, with the reason kept for
// `ccdad doctor`, because nothing in it was recoverable and refusing to write
// would leave the file broken for every future run.
//
// But a file that cannot be READ -- a permission problem, a directory in its
// place, an I/O error -- returns the error and leaves the caller with nothing.
// The bytes are probably still there, and the reason this differs from the usage
// cache is what the two cost when lost: a cache that degrades to empty is
// rebuilt by the next poll, while a series that degrades to empty is saved over
// by the next write and takes hours of readings with it.
func LoadHistory() (*History, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	h := &History{data: historyFile{Version: 1, Accounts: map[string]Account{}}}

	raw, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return h, nil
		}
		return nil, fmt.Errorf("reading %s: %w", FileName, err)
	}

	var parsed historyFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		h.loadErr = fmt.Errorf("parsing %s: %w", FileName, err)
		return h, nil
	}
	if parsed.Accounts != nil {
		h.data.Accounts = parsed.Accounts
	}
	h.data.Version = parsed.Version
	return h, nil
}

// save writes the series atomically. The caller must hold the lock.
//
// The document is rendered in ONE zone, and it is the machine's. A sample
// carries two kinds of moment from two places -- At is the reading's FetchedAt,
// computed from time.Now() and carrying the machine's offset, while the Reset
// beside it came off the wire, where every resets_at ends in Z and
// internal/usage parses it with an explicit .UTC() -- and encoding/json writes
// whichever offset it is handed. A live series carried 292 timestamps at +09:00
// beside 870 at Z, describing one afternoon.
//
// It is applied HERE, at the one serialiser, and not at the callers that build
// a Sample: a writer's job is to choose an instant, and an instant has no zone.
// status.json and every --json document obey the same rule at the same kind of
// place; see internal/zone.
//
// Only the rendering changes. Nothing reads a moment out of this file as a
// string: the reset a reader segments on is compared with time.Time, which is
// the whole point of Reading.Reset being truncated rather than formatted.
func (h *History) save(root string) error {
	h.data.Version = 1
	encoded, err := json.Marshal(zone.In(h.data, time.Local))
	if err != nil {
		return fmt.Errorf("encoding %s: %w", FileName, err)
	}
	return cclink.WriteFileAtomic(filepath.Join(root, FileName), encoded, 0o600)
}

// WithHistory runs fn against the series under a cross-process lock and writes
// back what it changed. This is the only safe way to modify it.
//
// An atomic rename alone is not enough. The daemon appends one account's sample
// while a hand-held refresh appends another, and both do a read-modify-write of
// the same document: without the lock the second rename silently drops the first
// one's sample. The lock is cclock's -- the same mkdir-based advisory mutex the
// usage cache uses, with the same staleness recovery -- rather than a second
// lock mechanism.
//
// The compromise check happens BEFORE the save, not after, and that is the
// second place this package departs from the usage cache. cclock produces
// ErrCompromised in Release, which the cache reaches only once its write has
// already landed, so a writer stalled past the stale threshold overwrites the
// writer that took the lock away from it. For a cache that costs one poll's
// worth of freshness; for a series it costs every sample the new holder had
// appended. Abandoning the write instead is safe because Put is keyed on
// (uuid, At): re-applying the lost sample duplicates nothing.
//
// That check is cclock's Owned, which stats the lock directory synchronously,
// and NOT a select on Compromised(). Compromised() is closed by cclock's touch
// goroutine on its own ticker, so a takeover can be a whole touch interval old
// before it appears there, and a process that was suspended mid-transaction can
// reach this point before that goroutine is next scheduled at all -- which is
// precisely the stalled writer this ordering exists to stop. A stat answers
// about now.
//
// It narrows the window rather than closing it: nothing makes the check and the
// rename one atomic operation, so a takeover landing between them still
// overwrites. What is left is the microseconds of a stat and a rename instead of
// seconds of an unticked timer. Closing it completely needs a lock the operating
// system enforces on the file itself, which is not what Claude Code's own mkdir
// mutex is, and interoperating with that mutex is why this package uses it.
//
// fn returning an error leaves the file exactly as it was: a poll that failed
// halfway must not persist half a reading.
func WithHistory(timeout time.Duration, fn func(*History) error) (err error) {
	root, rerr := storeRoot()
	if rerr != nil {
		return rerr
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}

	lock, aerr := cclock.Acquire(filepath.Join(root, lockDir), cclock.Options{
		Stale:   lockStale,
		Timeout: timeout,
	})
	if aerr != nil {
		return fmt.Errorf("locking the usage history: %w", aerr)
	}
	// Release's return value is part of the answer, not noise. It reports a
	// takeover that landed after the ownership check below -- the only window
	// that check leaves open -- and it reports a lock directory that could not
	// be removed, which would otherwise block every other writer silently until
	// the stale threshold elapsed.
	defer func() { err = errors.Join(err, lock.Release()) }()

	h, err := LoadHistory()
	if err != nil {
		return err
	}
	if err := fn(h); err != nil {
		return err
	}
	if !lock.Owned() {
		return fmt.Errorf("writing %s: %w", FileName, cclock.ErrCompromised)
	}
	return h.save(root)
}

// Record appends one sample for one account and applies both retention bounds,
// in a single transaction. It is the whole entry point the recorder needs.
//
// now is a parameter rather than a call to time.Now so that the bound applied
// here and the measurement the reader makes cannot disagree about when "now" is,
// and so retention is testable without sleeping through it.
func Record(timeout time.Duration, uuid string, s Sample, now time.Time) error {
	return WithHistory(timeout, func(h *History) error {
		h.Put(uuid, s)
		h.prune(now)
		return nil
	})
}
