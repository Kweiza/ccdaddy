package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/zone"
)

// StatusSchemaVersion is the version stamped into every document this binary
// writes. The `--json` contract is ADDITIVE: fields are added, never repurposed
// or removed, so a reader of any vintage can read a document of any vintage by
// ignoring what it does not recognise. Bumping this number is therefore not how
// a field is added — it is how a reader is told that something it may care
// about is new, and nothing in ccdad refuses a document on the strength of it.
//
// Getting this right in v1 is not academic. Upgrading ccdad replaces the binary
// while the OLD daemon keeps running and keeps publishing, so a new CLI reads an
// old document until something stops that daemon; and the moment it is
// restarted, any still-running shell pipeline is an old CLI reading a new
// document. Both directions have to work on the day of the upgrade.
const StatusSchemaVersion = 1

// statusFilePerm matches the rest of the store. chmod is a no-op on Windows,
// so this is a Unix-only property and nothing may depend on it.
const statusFilePerm = 0o600

// AccountState is the engine's view of one account. It is a plain string on the
// wire and a reader must NOT switch on it exhaustively: a newer daemon may
// publish a state this binary has never heard of, and the additive contract
// says that is legal. Carry an unrecognised value through and render it.
type AccountState string

const (
	// StateActive is the account Claude Code is currently logged in as.
	StateActive AccountState = "active"
	// StateCandidate is a healthy account the engine could switch to.
	StateCandidate AccountState = "candidate"
	// StateExhausted is over the threshold, and still polled: quota can be
	// granted or reset before the advertised timestamp.
	//
	// It does NOT mean the account is empty. The threshold is a number the user
	// chose -- or, under hover, a pace target derived from how far through its
	// window the account is -- and an account past it routinely has quota left.
	// StateEmpty is the other fact.
	StateExhausted AccountState = "exhausted"
	// StateEmpty is an account with a window that has nothing left in it at all.
	//
	// It is a separate value rather than a harder shade of exhausted because the
	// two drive different decisions: exhausted means the engine would rather not
	// spend this account, empty means it CANNOT -- the next prompt gets a 429.
	// Under hover the gap between them is wide, and publishing one word for both
	// is what had ccdad reporting five accounts "exhausted" while they held
	// between a fifth and a half of their week.
	StateEmpty AccountState = "empty"
	// StateQuarantined is held out of rotation by a dead refresh token.
	StateQuarantined AccountState = "quarantined"
	// StateDisabled was taken out of rotation by the user.
	StateDisabled AccountState = "disabled"
	// StateUnknown is an account whose usage could not be read. It is NOT an
	// empty account, and it must never render as 0%.
	StateUnknown AccountState = "unknown"
)

// AccountStatus is one account's engine state.
//
// Note what is NOT here: utilization, resets, credit spend. See the authority
// note on Status.
type AccountStatus struct {
	UUID  string       `json:"uuid"`
	State AccountState `json:"state,omitempty"`
	// NextPollAt is when the scheduler intends to poll this account next.
	NextPollAt time.Time `json:"nextPollAt,omitzero"`
	// LastPollAt is when it last did.
	LastPollAt time.Time `json:"lastPollAt,omitzero"`
	// LastPollError is why the last attempt failed, if it did. It is the engine's
	// own record and never a substitute for a quota reading: an account whose
	// poll failed is UNKNOWN, not empty.
	LastPollError string `json:"lastPollError,omitempty"`
}

// Status is the document the daemon publishes, and the only thing it publishes.
//
// # Which file is authoritative
//
// `ccdad list` and `ccdad status --json` can never disagree, because the daemon
// and the CLI read the same cache. That is a claim about usage.json, not about
// this file, and it only stays true if it is enforced here: if this document
// also carried utilization percentages, a reader taking quota from status.json
// while `list` takes it from usage.json would disagree with itself mid-tick,
// for no reason other than which file it happened to open.
//
// So the rule is that every field has exactly ONE authoritative file:
//
//   - quota — utilization, window resets, credit spend — is usage.json's, and
//     both `list` and `status` read it from there;
//   - account identity, alias, kind and the disabled flag are accounts.toml's;
//   - daemon and engine state — is a daemon alive, what did it decide, when does
//     it next intend to poll — is this file's, and nothing else records it.
//
// No number is read from two places, so two commands cannot disagree about one.
// A field added here later has to answer the same question first.
//
// # generatedAt is not a heartbeat
//
// The writer skips a write whose content is unchanged, so GeneratedAt advances
// only when something the daemon publishes actually changed. On an idle daemon
// it goes stale on purpose. It is a change stamp, and a reader that treats it as
// liveness will call a working daemon dead. Liveness comes from the singleton
// and from nowhere else — see Observe.
type Status struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	// PID is informational. It is never liveness evidence: the process may have
	// died and the number may have been recycled onto something unrelated.
	PID int `json:"pid"`
	// CredentialHome is the Claude Code credential home this daemon resolved at
	// startup — the directory whose .credentials.json it rewrites.
	//
	// It is here because it is a fact about the daemon PROCESS and nothing else
	// records it, which is this document's rule for what it owns. A daemon
	// started from a shell that resolved a credential home of its own -- which
	// CLAUDE_SECURESTORAGE_CONFIG_DIR decides when it is defined and
	// CLAUDE_CONFIG_DIR decides otherwise -- manages that directory rather than
	// ~/.claude for the rest of its life; the daemon is behaving correctly and
	// every other file on the machine looks normal, so a reader comparing this
	// against its own resolution is the only way anyone finds out. `ccdad
	// doctor` is that reader, in its credential-home row.
	//
	// `ccdad run --full-profile` used to be the example here and is no longer
	// reachable: auto-start refuses inside a `ccdad run` session, and the daemon
	// verbs that would start one by hand are refused there too. An override the
	// user set themselves is deliberately still allowed, which is what keeps
	// this field load-bearing.
	CredentialHome string `json:"credentialHome,omitempty"`
	// StartedAt is when this daemon acquired the singleton.
	StartedAt time.Time `json:"startedAt,omitzero"`
	// Stopped marks the final document a daemon writes on its way out. It is
	// what separates a clean shutdown from a crash: both leave a valid file
	// behind and a free singleton, and only this flag says which happened.
	Stopped bool `json:"stopped,omitempty"`
	// ActiveUUID is the account the engine last observed Claude Code using.
	ActiveUUID   string          `json:"activeUuid,omitempty"`
	LastSwitchAt time.Time       `json:"lastSwitchAt,omitzero"`
	LastSwitchTo string          `json:"lastSwitchTo,omitempty"`
	Accounts     []AccountStatus `json:"accounts,omitempty"`
	// The daily release check, in four fields that pass this file's own
	// authority rule: all four are facts about the daemon PROCESS, and nothing
	// else on the machine records them.
	//
	// UpdateCheckedAt is the DISPATCH stamp, not the answer's. The day's slot is
	// spent the moment the request goes out, so a daemon that dies mid-check
	// does not spend a second one when it comes back.
	UpdateCheckedAt   time.Time `json:"updateCheckedAt,omitzero"`
	NextUpdateCheckAt time.Time `json:"nextUpdateCheckAt,omitzero"`
	// UpdateLatest is the newest release the daemon has seen, spelled the way
	// buildinfo.Version is -- "0.7.0", never a leading v -- so the two sides of
	// the comparison a reader makes are written the same way.
	//
	// It is STICKY across failures: a failed check never erases the last good
	// reading, so a temporary outage does not un-tell the user about a release
	// that is still out. UpdateCheckError is the opposite and a successful
	// check clears it, because without that one failure would leave every
	// reader warning about it forever beside a row saying the check is current.
	//
	// There is deliberately no updateAvailable and no updateCheckEnabled. A
	// boolean computed by a 0.6.1 daemon would tell a 0.7.0 CLI to upgrade to
	// what it is already running -- the upgrade-day skew above, turned into a
	// wrong answer -- so what is published is an OBSERVATION and the comparison
	// happens in the reader, against the reader's own version. Whether the
	// check is switched on has an authoritative file of its own, which is
	// config.toml, and `ccdad doctor` reads it directly.
	UpdateLatest     string `json:"updateLatest,omitempty"`
	UpdateCheckError string `json:"updateCheckError,omitempty"`
	// The tick loop's own health, in three fields, and they exist because none
	// of it was anywhere on disk when it was needed. A daemon whose every tick
	// failed for three hours published this document 11,300 times without one
	// of those documents saying so, and `ccdad doctor` read them all and
	// reported every row ok -- because what it asked was whether a daemon holds
	// the singleton, which is liveness and not health.
	//
	// They pass this file's authority rule: the tick loop is in the daemon
	// process and nothing else on the machine can observe it. daemon.log
	// carries the same failure in prose, but prose is not a thing a check can
	// ask a question of.
	//
	// All three are ABSENT on a healthy daemon rather than zero-valued, so a
	// reader can tell "this daemon is fine" from "this daemon is too old to
	// say" without knowing the vintage of what wrote it.
	//
	// TickFailures is the length of the current unbroken run, and it is
	// published ONE TICK BEHIND: the publish happens inside the tick body, so
	// the document written this tick carries the streak as of the previous one.
	// Loop.Health says the same thing from the other side.
	TickFailures     int       `json:"tickFailures,omitempty"`
	TickFailingSince time.Time `json:"tickFailingSince,omitzero"`
	LastTickError    string    `json:"lastTickError,omitempty"`
	// TickHealthReported is what makes the three fields above readable, and
	// without it they are worse than absent.
	//
	// A healthy daemon publishes no failures, and a daemon too old to have
	// heard of these fields publishes no failures either -- the same bytes, for
	// opposite reasons. A reader that could not tell them apart would print
	// "ok, the tick loop is fine" about the one daemon most likely to be
	// wedged, which is precisely the confident wrong answer this row was added
	// to stop. And the ambiguity is not a corner: the additive contract at the
	// top of this file means an old daemon publishing into a new CLI is the
	// NORMAL state on the day of an upgrade, because the old one keeps running
	// until something stops it.
	//
	// It is written by every daemon that has run a tick and can report on it,
	// so the reader's question is "did the writer say", not "is the value zero".
	TickHealthReported bool `json:"tickHealthReported,omitempty"`
}

// StatusWriter publishes Status documents and skips the ones that would change
// nothing.
//
// The skip is the whole reason this is a type rather than a function. The tick
// loop runs at 1 Hz — about 86,400 writes a day — and cclink.WriteFileAtomic
// creates a fresh temp sibling, fsyncs it and renames it every time. That is
// exactly right for a credential file and far too heavy for a cache that mostly
// republishes the same bytes. Comparing against what was last published turns an
// idle daemon's cost into one stat per second.
//
// It is NOT safe for concurrent use. One daemon publishes, from its tick.
type StatusWriter struct {
	// last is the marshalled document with GeneratedAt zeroed. The stamp has to
	// be excluded or every tick differs by a nanosecond and the skip never
	// fires — which is the shape this optimisation fails in silently.
	last []byte
	// size is what the last published file measured, so a file that was
	// truncated, hand-edited or removed is republished instead of being left
	// broken for the daemon's whole life on the strength of an in-memory belief.
	size int64
	// published records whether last/size mean anything yet.
	published bool
	// zone is the one zone every timestamp in the published document is
	// rendered in.
	//
	// It has to be ONE, and it cannot be left to the writers. nextPollAt is
	// computed two ways -- now.Add(interval) for an account on the ordinary
	// cadence, and a window's resets_at for one whose next look is pulled to a
	// rollover -- and the second of those is parsed from a wire string ending
	// in Z, so it arrives carrying UTC while the first carries the machine's
	// zone. encoding/json writes whichever offset it is handed. A live store
	// held five poll times at +09:00 beside one at Z, and the Z row read as
	// nine hours overdue when it was in fact four minutes in the future.
	//
	// The instants were never wrong and are not changed here. What changes is
	// that the document decides how they are written, which is the only level
	// at which "one document, one zone" is a property rather than a habit.
	zone *time.Location
}

// NewStatusWriter returns a writer that has published nothing, so its first
// Write always writes.
//
// The zone is the machine's, and it is not an argument. Every reader of this
// document is on the machine that wrote it -- `ccdad status`, `ccdad doctor`,
// the dashboard, and the person who opens status.json in an editor -- so there
// is nobody else to ask. UTC would be defensible for a document that travelled,
// and this one does not.
func NewStatusWriter() *StatusWriter { return &StatusWriter{zone: time.Local} }

// loc is the zone to render in, with the zero value of StatusWriter treated as
// the machine's. time.Time.In panics on a nil location, and a writer built as a
// struct literal rather than through the constructor is otherwise a crash in
// the tick loop.
func (w *StatusWriter) loc() *time.Location {
	if w.zone == nil {
		return time.Local
	}
	return w.zone
}

// Write publishes s, stamping the schema version and — only when the content
// changed — the time. It reports whether anything was written.
func (w *StatusWriter) Write(s Status, now time.Time) (bool, error) {
	root, err := storeRoot()
	if err != nil {
		return false, err
	}

	// Before either marshal, so the bytes the skip compares are the bytes that
	// would be written. It also detaches s.Accounts from the caller's slice --
	// Status is passed by value and the slice header inside it is not -- which
	// is what makes the two assignments below safe to do on a copy.
	s = zone.In(s, w.loc())

	s.SchemaVersion = StatusSchemaVersion
	s.GeneratedAt = time.Time{}
	probe, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", StatusFileName, err)
	}
	if w.published && bytes.Equal(probe, w.last) && w.onDisk() {
		return false, nil
	}

	s.GeneratedAt = now.In(w.loc())
	encoded, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", StatusFileName, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return false, fmt.Errorf("creating the ccdad store: %w", err)
	}
	if err := cclink.WriteFileAtomic(filepath.Join(root, StatusFileName), encoded, statusFilePerm); err != nil {
		return false, err
	}
	w.last, w.size, w.published = probe, int64(len(encoded)), true
	return true, nil
}

// onDisk reports whether the file still looks like what was last published. A
// stat per tick is cheap; the write it avoids is not.
// A path that cannot be resolved reports "not on disk", which makes the caller
// attempt the write — and that is where the resolution failure is reported as
// an error rather than swallowed as a skipped tick.
func (w *StatusWriter) onDisk() bool {
	path, err := StatusPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() == w.size
}

// ReadStatus reads the published document. ok is false with no error when there
// is nothing to read, which is the ordinary state of a machine where no daemon
// has ever run.
//
// A document that exists but does not parse IS an error. Folding it into
// "nothing to read" would hide a store that is damaged behind a state that is
// normal, which is the same mistake the pidfile reader refuses to make.
//
// Unknown fields are ignored, which encoding/json does by default and which
// TestReadStatusIgnoresUnknownFields pins deliberately: a DisallowUnknownFields
// "hardening" here would break every older reader the first time a field is
// added, and it is one line away at all times.
//
// It takes NO LOCK, and must not grow one. The three-file store layout in this
// package's doc comment puts this file in the never-locked column, and on
// Windows LockFileEx locks are MANDATORY: a reader holding one makes the
// daemon's next rename fail outright. The write is a rename, so a reader sees
// one whole version of the document or another — and catching the pre-rename
// inode is legitimate rather than something to guard against.
func ReadStatus() (Status, bool, error) {
	root, err := storeRoot()
	if err != nil {
		return Status{}, false, err
	}
	body, err := os.ReadFile(filepath.Join(root, StatusFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Status{}, false, nil
		}
		return Status{}, false, fmt.Errorf("reading %s: %w", StatusFileName, err)
	}
	var s Status
	if err := json.Unmarshal(body, &s); err != nil {
		return Status{}, false, fmt.Errorf("parsing %s: %w", StatusFileName, err)
	}
	// A document with no schemaVersion was not written by any ccdad. Anything
	// from 1 upwards is accepted, including versions from the future: refusing
	// one is exactly the break the additive contract exists to prevent.
	if s.SchemaVersion < 1 {
		return Status{}, false, fmt.Errorf("%s carries no schemaVersion, so it was not written by ccdad", StatusFileName)
	}
	return s, true, nil
}

// SweepStatusTemps removes temp files left beside status.json, and beside the
// usage history, by a rename that never completed.
//
// WriteFileAtomic's own comment calls an orphaned temp acceptable, and at the
// rate it is called elsewhere it is. At one write per second on Windows, where
// a scanner holding the temp file open is what strands it, "rare" becomes
// "daily". The daemon sweeps at startup, AFTER taking the singleton — which is
// what makes it safe for status.json, because the singleton is the proof that no
// other daemon is mid-rename.
//
// The history is swept on a NARROWER argument, and the singleton is not it:
// `ccdad list --refresh` appends to the series from a process that holds no
// singleton, so a temp file there can belong to a live stranger just as
// usage.json's can. What differs is what each side costs. A history temp exists
// for the microseconds between one write and one rename, at most once per poll,
// and losing that race drops a single sample out of hundreds — while an orphan
// there is otherwise collected by nothing, because that file has no other
// sweeper. usage.json is written by every command that takes a reading, and a
// lost cache write costs a poll's worth of freshness for every reader at once,
// so its temps are still left alone.
func SweepStatusTemps() error {
	root, err := storeRoot()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range []string{StatusFileName, history.FileName} {
		// The glob is DERIVED from the writer's own pattern rather than
		// spelled again here. It was a second literal once, under a comment
		// asserting it matched WriteFileAtomic's — and nothing executed that
		// assertion, so changing the writer's left this sweeping nothing with
		// every test in the repository still green.
		matches, gerr := filepath.Glob(filepath.Join(root, cclink.TempPattern(name)))
		if gerr != nil {
			// Glob only fails on a malformed pattern, and both of these are
			// built from constants.
			return fmt.Errorf("scanning for %s temp files: %w", name, gerr)
		}
		for _, path := range matches {
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				errs = append(errs, rerr)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sweeping temp files: %w", errors.Join(errs...))
	}
	return nil
}

// DaemonState is the answer to "is a daemon running", with the third outcome a
// probe that could not answer requires: "cannot tell" is not "no".
type DaemonState uint8

const (
	// DaemonUnknown is "cannot determine" — the lock could not be probed. It is
	// never folded into DaemonStopped: a supervisor gating on that would respawn
	// forever on a filesystem where locks do not work.
	DaemonUnknown DaemonState = iota
	// DaemonStopped is a definite no.
	DaemonStopped
	// DaemonRunning is a definite yes.
	DaemonRunning
)

func (s DaemonState) String() string {
	switch s {
	case DaemonStopped:
		return "stopped"
	case DaemonRunning:
		return "running"
	default:
		return "unknown"
	}
}

// Report is what the published document says, together with whether the daemon
// that wrote it is still there.
type Report struct {
	State     DaemonState
	Status    Status
	HasStatus bool
	// StatusErr is why the document could not be read, when one existed. It is
	// carried rather than returned because a damaged status file must not cost
	// the caller the liveness answer, which is the part a dashboard cannot
	// degrade without. `ccdad doctor` is what prints it.
	StatusErr error
}

// Observe reads the published document and decides, against the singleton,
// whether it describes a daemon that is still alive.
//
// The cross-check is the point. A crashed daemon leaves a perfectly valid
// status.json behind whose numbers then age in silence, and nothing inside the
// document can report that — not generatedAt, which the unchanged-bytes skip
// makes a change stamp rather than a heartbeat, and not pid, which may have been
// recycled onto an unrelated process. Only the singleton knows, because the
// kernel releases it when the process dies.
//
// An error means the probe could not answer. State is DaemonUnknown then, and
// the document is still reported: it is the liveness verdict that is missing,
// not the contents.
func Observe() (Report, error) {
	var r Report
	s, ok, serr := ReadStatus()
	r.Status, r.HasStatus, r.StatusErr = s, ok, serr

	held, err := SingletonHeld()
	if err != nil {
		r.State = DaemonUnknown
		return r, err
	}
	if held {
		r.State = DaemonRunning
	} else {
		r.State = DaemonStopped
	}
	return r, nil
}
