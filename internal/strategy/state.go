package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// The engine's anti-flap state, on disk.
//
// The cooldown and the quarantine are not derivable from anything else, and
// they are the two anti-flap mechanisms whose whole job is to remember
// something across time. A daemon that auto-restarts from any CLI command
// would reset an in-memory cooldown on every restart and become the exact
// switch storm the cooldown exists to prevent, so this lives in a file rather
// than in a process.
//
// It is a SEPARATE file from usage.json with its own lock. The usage cache is
// written by the poller fleet on every reading; this is written only when the
// engine actually moves or quarantines. Sharing one document would put an
// engine-rate writer behind a poller-rate lock for no gain.

const (
	StateFileName = "strategy.json"
	// stateLockDir is a DIRECTORY, because that is what cclock's mutex is.
	stateLockDir = "strategy.json.lock"

	// stateLockStale mirrors the usage cache's: a state write is a sub-second
	// operation, so this only ever matters after a crash, and it must stay at
	// least twice cclock's touch interval or a live holder's lock goes stale by
	// its own definition between two touches.
	stateLockStale = 30 * time.Second
)

// StatePath is where the state lives.
func StatePath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, StateFileName), nil
}

func stateLockPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, stateLockDir), nil
}

// Quarantine is one account held out of auto-rotation by the engine.
//
// It carries an expiry AND can be cleared explicitly, and it needs both. Only
// re-authentication actually fixes a dead refresh token, so an expiry alone
// would put a dead account back into rotation forever; but a quarantine that
// only an explicit clear could lift would make a misclassified failure
// permanent, and nothing else in ccdad would ever notice.
type Quarantine struct {
	// Since is when the account was quarantined.
	Since time.Time `json:"since"`
	// Until is when the quarantine lapses on its own. The expiry is a safety
	// valve rather than a healing time: a genuinely dead token comes back,
	// fails one refresh, and is quarantined again.
	Until time.Time `json:"until"`
	// Reason is why, kept because it reaches a user-facing notification.
	Reason string `json:"reason,omitempty"`
}

// Active reports whether this quarantine still holds at now.
//
// A zero Until never lapses. That is not a degradation: a hand-edited or
// truncated file that loses the timestamp must fail towards holding the account
// OUT of rotation, because the only reason an account is in here is that its
// credential did not work.
func (q Quarantine) Active(now time.Time) bool {
	return q.Until.IsZero() || now.Before(q.Until)
}

// stateFile is the on-disk shape.
type stateFile struct {
	// Version lets a later release migrate rather than guess.
	Version int `json:"version"`
	// LastSwitchAt is when the engine last moved. The zero time means never,
	// which is not the same as "long ago" only in that no cooldown is in force.
	LastSwitchAt time.Time `json:"last_switch_at,omitempty"`
	// LastSwitchTo is the account it moved to, kept so `ccdad status` can say
	// what the cooldown is protecting.
	LastSwitchTo string `json:"last_switch_to,omitempty"`
	// The Codex lane's own pair. It is a SECOND pair rather than a second
	// document because the anti-flap state is one file with one lock, and it is
	// not a shared pair because the two lanes move for different reasons: a
	// Claude rotation says nothing about which account should serve codex, and
	// one stamp would have each lane serving the other's cooldown -- a Codex
	// repoint holding Claude Code's login still five minutes later, and the
	// reverse. Absent in a document written before this build, which reads as
	// "the Codex lane has never moved", which is true of such a machine.
	CodexLastSwitchAt time.Time `json:"codex_last_switch_at,omitempty"`
	CodexLastSwitchTo string    `json:"codex_last_switch_to,omitempty"`
	// Quarantine is keyed by account UUID and never by idx.
	// store.sortAndReindex recompacts idx on every removal, so a file keyed on
	// it would quarantine a different account after any `ccdad remove`.
	Quarantine map[string]Quarantine `json:"quarantine,omitempty"`
}

// State is the parsed anti-flap state.
type State struct {
	data    stateFile
	loadErr error
}

// NewState builds an empty in-memory state. It touches no disk, which is what
// makes the engine testable without one.
func NewState() *State {
	return &State{data: stateFile{Version: 1, Quarantine: map[string]Quarantine{}}}
}

// LoadError is why the state came back empty when a file did exist. It is not
// fatal — an unreadable state file means no cooldown and no quarantine, which
// is the state a first run is in — but `ccdad doctor` should be able to say so
// rather than having the corruption stay invisible.
func (s *State) LoadError() error { return s.loadErr }

// LastSwitch is when the engine last moved and where to.
func (s *State) LastSwitch() (time.Time, string) {
	return s.data.LastSwitchAt, s.data.LastSwitchTo
}

// RecordSwitch stamps a completed switch. The caller records it AFTER the
// credential swap succeeded: a cooldown earned by a switch that failed would
// hold the engine off the retry.
func (s *State) RecordSwitch(uuid string, at time.Time) {
	s.data.LastSwitchAt = at
	s.data.LastSwitchTo = uuid
}

// CodexLastSwitch is when the Codex lane last repointed the serving account and
// where to.
func (s *State) CodexLastSwitch() (time.Time, string) {
	return s.data.CodexLastSwitchAt, s.data.CodexLastSwitchTo
}

// RecordCodexSwitch stamps a completed Codex repoint. The rule RecordSwitch
// states applies here unchanged: the caller records it AFTER the pointer was
// written, because a cooldown earned by a repoint that failed would hold the
// lane off its own retry.
func (s *State) RecordCodexSwitch(uuid string, at time.Time) {
	s.data.CodexLastSwitchAt = at
	s.data.CodexLastSwitchTo = uuid
}

// ForProvider is this state as one lane sees it.
//
// The anti-flap gates read LastSwitch, and there is exactly one implementation
// of them. Rather than give the Codex lane a second cooldown gate that would be
// free to drift from the first, the Codex view PRESENTS the codex pair under
// the name the gates already read.
//
// It is a shallow copy on purpose, and the quarantine map is therefore SHARED.
// This is a read view for a decision, not a handle to write through: the two
// writers of this document both go through WithState, which loads its own
// State. A caller that mutated a quarantine on the returned value would be
// mutating the original's map, which is why nothing does.
func (s *State) ForProvider(p provider.ID) *State {
	if p != provider.Codex {
		return s
	}
	out := &State{data: s.data, loadErr: s.loadErr}
	out.data.LastSwitchAt = s.data.CodexLastSwitchAt
	out.data.LastSwitchTo = s.data.CodexLastSwitchTo
	return out
}

// CooldownRemaining is how long the anti-flap cooldown still has to run, and
// whether one is in force at all.
//
// A LastSwitchAt in the future is a clock that moved backwards rather than a
// switch that has not happened yet, and it is deliberately still honoured: the
// conservative direction here is to wait, and the wait is bounded by the
// cooldown itself once the clock settles.
func (s *State) CooldownRemaining(now time.Time, d time.Duration) (time.Duration, bool) {
	if s.data.LastSwitchAt.IsZero() || d <= 0 {
		return 0, false
	}
	until := s.data.LastSwitchAt.Add(d)
	if !now.Before(until) {
		return 0, false
	}
	return until.Sub(now), true
}

// Quarantined reports an account's live quarantine, if it has one.
func (s *State) Quarantined(uuid string, now time.Time) (Quarantine, bool) {
	q, ok := s.data.Quarantine[uuid]
	if !ok || !q.Active(now) {
		return Quarantine{}, false
	}
	return q, true
}

// Quarantine holds an account out of auto-rotation. Callers must have decided
// the failure warrants it — see ClassifyRefresh, which is the only thing that
// may make that call.
func (s *State) Quarantine(uuid string, now time.Time, d time.Duration, reason string) {
	if s.data.Quarantine == nil {
		s.data.Quarantine = map[string]Quarantine{}
	}
	if d <= 0 {
		d = DefaultQuarantine
	}
	s.data.Quarantine[uuid] = Quarantine{Since: now, Until: now.Add(d), Reason: reason}
}

// ClearQuarantine lifts an account's quarantine and reports whether there was
// one. `ccdad add claude` re-authenticating an existing uuid must call this:
// store.Add updates in place, so without it the user re-logs-in successfully
// and the engine goes on refusing to use the account.
func (s *State) ClearQuarantine(uuid string) bool {
	if _, ok := s.data.Quarantine[uuid]; !ok {
		return false
	}
	delete(s.data.Quarantine, uuid)
	return true
}

// QuarantinedUUIDs lists every account under a live quarantine, in uuid order
// so the answer does not depend on map iteration.
func (s *State) QuarantinedUUIDs(now time.Time) []string {
	out := make([]string, 0, len(s.data.Quarantine))
	for uuid, q := range s.data.Quarantine {
		if q.Active(now) {
			out = append(out, uuid)
		}
	}
	sort.Strings(out)
	return out
}

// Prune drops quarantines for accounts that are no longer managed, so a removed
// account cannot come back quarantined when it is added again under the same
// uuid.
func (s *State) Prune(managed map[string]bool) {
	for uuid := range s.data.Quarantine {
		if !managed[uuid] {
			delete(s.data.Quarantine, uuid)
		}
	}
}

// storeRoot returns ccdad's state directory, refusing a relative one for the
// same reason store.Open does: a relative root puts engine state in whatever
// directory ccdad happened to be run from, a different one each time — which
// for a cooldown means every invocation starts with none.
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

// LoadState reads the state without taking a lock.
//
// No lock is needed to read: every write is a rename, so a reader sees one whole
// version of the document or another, never a torn one. A file that cannot be
// read or parsed degrades to an empty state — no cooldown, no quarantine — and
// records why in LoadError. That degradation is deliberately towards MORE
// switching rather than less: a quarantine that cannot be read is not evidence
// that an account is dead, and refusing to switch at all on a corrupt file
// would park the engine exactly the way anti-flap must never park it: those
// margins bound the rate of switching, not whether the engine can switch at
// all.
func LoadState() (*State, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	s := NewState()

	raw, err := os.ReadFile(filepath.Join(root, StateFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		s.loadErr = fmt.Errorf("reading %s: %w", StateFileName, err)
		return s, nil
	}

	var parsed stateFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		s.loadErr = fmt.Errorf("parsing %s: %w", StateFileName, err)
		return s, nil
	}
	if parsed.Quarantine != nil {
		s.data.Quarantine = parsed.Quarantine
	}
	s.data.LastSwitchAt = parsed.LastSwitchAt
	s.data.LastSwitchTo = parsed.LastSwitchTo
	s.data.CodexLastSwitchAt = parsed.CodexLastSwitchAt
	s.data.CodexLastSwitchTo = parsed.CodexLastSwitchTo
	s.data.Version = parsed.Version
	return s, nil
}

// save writes the state atomically. The caller must hold the lock.
func (s *State) save(root string) error {
	s.data.Version = 1
	encoded, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", StateFileName, err)
	}
	return atomicfile.WriteFile(filepath.Join(root, StateFileName), encoded, 0o600)
}

// WithState runs fn against the state under a cross-process lock and writes back
// what it changed. This is the only safe way to modify it.
//
// An atomic rename alone is not enough. The daemon stamps a cooldown while
// `ccdad switch` clears a quarantine, and both do a read-modify-write of the
// same document: without the lock the second rename silently drops the first
// one's change — and the change it drops is a cooldown, which is a switch storm.
//
// fn returning an error leaves the file exactly as it was.
func WithState(timeout time.Duration, fn func(*State) error) (err error) {
	root, rerr := storeRoot()
	if rerr != nil {
		return rerr
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}

	lock, aerr := cclock.Acquire(filepath.Join(root, stateLockDir), cclock.Options{
		Stale:   stateLockStale,
		Timeout: timeout,
	})
	if aerr != nil {
		return fmt.Errorf("locking the engine state: %w", aerr)
	}
	// Release's return value is part of the answer, not noise: the re-stat it
	// performs is the only check that can see a takeover in the window between
	// the touch goroutine's last tick and now, and discarding it would report
	// success for exactly the write that raced.
	defer func() { err = errors.Join(err, lock.Release()) }()

	s, err := LoadState()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	return s.save(root)
}

// RefreshOutcome is what a token-refresh failure means for the engine.
//
// Every TokenErrorKind gets its own value, and only one of them quarantines.
// The quarantine has exactly one trigger, a refresh token the server
// rejected, and collapsing these would fire it on anything: a transport
// failure quarantines every account the first time the laptop sleeps, and a
// bad status does it the first time Anthropic returns a 503.
type RefreshOutcome uint8

const (
	// RefreshOK is no failure at all.
	RefreshOK RefreshOutcome = iota
	// RefreshUnreachable is oauth.TokenErrorTransport: the request never got an
	// HTTP answer. That is the network, not the account.
	RefreshUnreachable
	// RefreshDead is oauth.TokenErrorInvalidCode: the refresh token itself was
	// rejected. This is the ONLY outcome that quarantines.
	RefreshDead
	// RefreshScopeRefused is oauth.TokenErrorInvalidScope. The credential is
	// live and the request was wrong, so quarantining the account would blame
	// it for ccdad's own bug.
	RefreshScopeRefused
	// RefreshUpstream is oauth.TokenErrorStatus: the endpoint answered, badly.
	RefreshUpstream
	// RefreshUnknown is an error that is not a token-endpoint failure at all —
	// a context deadline, a store read. It is not evidence about the token.
	RefreshUnknown
)

func (o RefreshOutcome) String() string {
	switch o {
	case RefreshOK:
		return "ok"
	case RefreshUnreachable:
		return "unreachable"
	case RefreshDead:
		return "dead-refresh-token"
	case RefreshScopeRefused:
		return "scope-refused"
	case RefreshUpstream:
		return "upstream-error"
	case RefreshUnknown:
		return "unknown-error"
	}
	return "unknown"
}

// Quarantines reports whether this outcome may quarantine the account. It is a
// method rather than a comparison at every call site so that adding a kind
// cannot quietly add a way to quarantine.
func (o RefreshOutcome) Quarantines() bool { return o == RefreshDead }

// ClassifyRefresh maps a token-refresh failure onto the engine's answer. It is
// the only thing that may decide an account is dead.
func ClassifyRefresh(err error) RefreshOutcome {
	if err == nil {
		return RefreshOK
	}
	var te *oauth.TokenError
	if !errors.As(err, &te) {
		return RefreshUnknown
	}
	switch te.Kind {
	case oauth.TokenErrorTransport:
		return RefreshUnreachable
	case oauth.TokenErrorInvalidCode:
		return RefreshDead
	case oauth.TokenErrorInvalidScope:
		return RefreshScopeRefused
	case oauth.TokenErrorStatus:
		return RefreshUpstream
	}
	return RefreshUnknown
}

// codexStampTimeout bounds the wait for the state lock when a Codex repoint
// stamps its cooldown. It is the same five seconds internal/switcher waits for
// the Claude stamp: the write is sub-second, so anything longer is a lock a
// crashed process left behind, and cclock's stale rule is what clears that.
const codexStampTimeout = 5 * time.Second

// RecordCodexSwitch stamps the Codex anti-flap cooldown after a repoint has
// succeeded. It is the sibling of switcher.RecordSwitch and it lives HERE
// rather than there because internal/codexswitch is the only caller and that
// package must not be able to reach internal/switcher at all -- the import gate
// on its dependency closure is what makes "a Codex repoint can never install a
// Claude credential" a property rather than a promise.
//
// An EXPLICIT `ccdad switch <codex>` stamps it too, for the reason the Claude
// side does: the user has just chosen an account, and a lane evaluating ten
// seconds later must not immediately override the choice.
func RecordCodexSwitch(uuid string) error {
	err := WithState(codexStampTimeout, func(st *State) error {
		st.RecordCodexSwitch(uuid, time.Now())
		return nil
	})
	if err != nil {
		return fmt.Errorf("the codex auto-switch cooldown could not be recorded: %w", err)
	}
	return nil
}
