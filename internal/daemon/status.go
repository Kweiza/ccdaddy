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
)

// StatusSchemaVersion is the version stamped into every document this binary
// writes. §9.4's contract is ADDITIVE: fields are added, never repurposed or
// removed, so a reader of any vintage can read a document of any vintage by
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

// statusFilePerm matches the rest of the store. §10.3: Windows gets no chmod,
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
	// granted or reset before the advertised timestamp (§7.4).
	StateExhausted AccountState = "exhausted"
	// StateQuarantined is held out of rotation by §7.2's dead-refresh-token rule.
	StateQuarantined AccountState = "quarantined"
	// StateDisabled was taken out of rotation by the user.
	StateDisabled AccountState = "disabled"
	// StateUnknown is an account whose usage could not be read. It is NOT an
	// empty account (§7.2), and it must never render as 0%.
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
// §8.4 claims `ccdad list` and `ccdad status --json` can never disagree because
// the daemon and the CLI read the same cache. That is a claim about usage.json,
// not about this file, and it only stays true if it is enforced here: if this
// document also carried utilization percentages, a reader taking quota from
// status.json while `list` takes it from usage.json would disagree with itself
// mid-tick, for no reason other than which file it happened to open.
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
	// started from inside `ccdad run --full-profile` resolves a per-session
	// directory rather than ~/.claude and then manages that one for the rest of
	// its life; the daemon is behaving correctly and every other file on the
	// machine looks normal, so a reader comparing this against its own
	// resolution is the only way anyone finds out. `ccdad doctor` is that
	// reader.
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
}

// NewStatusWriter returns a writer that has published nothing, so its first
// Write always writes.
func NewStatusWriter() *StatusWriter { return &StatusWriter{} }

// Write publishes s, stamping the schema version and — only when the content
// changed — the time. It reports whether anything was written.
func (w *StatusWriter) Write(s Status, now time.Time) (bool, error) {
	root, err := storeRoot()
	if err != nil {
		return false, err
	}

	s.SchemaVersion = StatusSchemaVersion
	s.GeneratedAt = time.Time{}
	probe, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", StatusFileName, err)
	}
	if w.published && bytes.Equal(probe, w.last) && w.onDisk() {
		return false, nil
	}

	s.GeneratedAt = now
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
// It takes NO LOCK, and must not grow one. §8.1 puts this file in the
// never-locked column, and on Windows LockFileEx locks are MANDATORY: a reader
// holding one makes the daemon's next rename fail outright. The write is a
// rename, so a reader sees one whole version of the document or another — and
// catching the pre-rename inode is legitimate rather than something to guard
// against.
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

// SweepStatusTemps removes temp files left beside status.json by a rename that
// never completed.
//
// WriteFileAtomic's own comment calls an orphaned temp acceptable, and at the
// rate it is called elsewhere it is. At one write per second on Windows, where
// a scanner holding the temp file open is what strands it, "rare" becomes
// "daily". The daemon sweeps at startup, AFTER taking the singleton — which is
// what makes it safe, because the singleton is the proof that no other daemon is
// mid-rename.
//
// It sweeps this file's temps and nothing else. usage.json's temps belong to a
// writer that may be a live CLI process holding the cache lock, and removing one
// of those would race a stranger's rename rather than clean up after a dead one.
func SweepStatusTemps() error {
	root, err := storeRoot()
	if err != nil {
		return err
	}
	// The pattern is WriteFileAtomic's own: filepath.Base(path) + ".tmp-*".
	matches, err := filepath.Glob(filepath.Join(root, StatusFileName+".tmp-*"))
	if err != nil {
		// Glob only fails on a malformed pattern, and this one is a constant.
		return fmt.Errorf("scanning for %s temp files: %w", StatusFileName, err)
	}
	var errs []error
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sweeping %s temp files: %w", StatusFileName, errors.Join(errs...))
	}
	return nil
}

// DaemonState is the answer to "is a daemon running", with the third outcome
// §8.2 requires.
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
