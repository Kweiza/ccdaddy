// Package cclinktest is an in-memory macOS Keychain item for tests in other
// packages, the way net/http/httptest is a server for them. It is imported only
// by _test.go files, so nothing in it reaches the ccdad binary.
//
// It exists because the behaviour that depends on the keychain -- the daemon's
// split-login repair, `ccdad switch`'s no-op rule, doctor's split row -- lives
// in internal/switcher, internal/daemon and internal/cli, and a test there has
// no other way to put an item on a machine without touching the developer's
// own keychain. cclink's exec-layer tests run a fixture `security` process
// instead, which is what exercises that layer; this replaces it whole
// (cclink.SetSecurityRunnerForTest), so every line ABOVE it -- the bounds, the
// read-back of an unconfirmed write, the item-first order -- runs as it does on
// a Mac.
package cclinktest

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// The exit statuses `security` answers with, which this item speaks because
// cclink classifies them: errSecItemNotFound is an answer, and
// errSecInteractionNotAllowed -- a keychain refusing a non-interactive lookup
// or write -- is a failure.
const (
	itemNotFound          = 44
	interactionNotAllowed = 36
)

// Keychain is one in-memory credential item. The derived item name is not
// consulted: every lookup and write lands on the same secret.
type Keychain struct {
	mu        sync.Mutex
	secret    string
	present   bool
	wrote     bool
	writes    int
	readsFail bool
	reads     int

	// OnWrite, when set, runs at the start of every write, outside the item's
	// lock: a test advances its clock there to stand for a write that takes as
	// long as a slow keychain's does.
	OnWrite func()
	// OnRead, when set, runs at the start of every lookup, outside the item's
	// lock: a test lands another process's change there, at a moment no timing
	// could arrange.
	OnRead func()

	// WriteLandsThenTimesOut makes every write store its payload and then
	// report that it ran out of time -- the 2026-11-10 incident.
	WriteLandsThenTimesOut bool
	// WriteTimesOutUnlanded makes every write store NOTHING and report that it
	// ran out of time: the same failure, with the other outcome behind it.
	WriteTimesOutUnlanded bool
	// WritesRefused makes every write store nothing and ANSWER that it failed,
	// the way a keychain refusing a non-interactive write does (exit 36).
	WritesRefused bool
	// ReadsFailAfterWrite makes every read after the first write fail the way a
	// keychain refusing a non-interactive lookup does (exit 36), so a write
	// that landed cannot be confirmed.
	ReadsFailAfterWrite bool
}

// NewKeychain is an item holding secret (compact JSON, as Claude Code writes
// it).
func NewKeychain(secret string) *Keychain {
	return &Keychain{secret: secret, present: true}
}

// Install makes k this process's keychain for the rest of t, and restores the
// one it replaced when t ends.
func Install(t testing.TB, k *Keychain) {
	t.Helper()
	t.Cleanup(cclink.SetSecurityRunnerForTest(k.run))
}

// Isolate is what a test package's TestMain calls before anything else, role
// dispatch included: an empty Keychain -- no item, which is what a sandboxed
// Claude Code home has on any machine -- becomes the default, and spawning the
// real /usr/bin/security from then on panics, so a test that reaches the
// developer's own keychain fails loudly instead of reading it. A test that
// needs an item installs its own with Install, and its cleanup restores this
// default.
func Isolate() {
	cclink.SetSecurityRunnerForTest((&Keychain{}).run)
	cclink.ForbidRealSecurityForTest()
}

// Isolated reports whether Isolate has run in this process: a test
// re-executing its own binary asks it of the child, to prove the child cannot
// reach the real keychain either.
func Isolated() bool { return cclink.RealSecurityForbidden() }

// Secret is what the item holds now, and whether there is an item.
func (k *Keychain) Secret() (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.secret, k.present
}

// Reads is how many lookups the item has been asked for.
func (k *Keychain) Reads() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reads
}

// Writes is how many writes the item has been asked for.
func (k *Keychain) Writes() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.writes
}

// Set replaces what the item holds, as Claude Code writing it would.
func (k *Keychain) Set(secret string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.secret, k.present = secret, true
}

// Heal turns every failure mode off: the keychain answers normally from now on,
// the way a loaded Mac does once its load drops.
func (k *Keychain) Heal() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.WriteLandsThenTimesOut, k.WriteTimesOutUnlanded, k.WritesRefused, k.ReadsFailAfterWrite = false, false, false, false
	k.readsFail = false
}

// FailReads makes every read fail (exit 36) while on is true, as a keychain
// that has locked does.
func (k *Keychain) FailReads(on bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.readsFail = on
}

// run answers one subcommand the way /usr/bin/security would. A write that
// "runs out of time" returns context.DeadlineExceeded, which is how a runner
// says it gave up at its deadline (cclink.SecurityRunner).
func (k *Keychain) run(_ context.Context, args ...string) (cclink.SecurityAnswer, error) {
	if args[0] == "add-generic-password" && k.OnWrite != nil {
		k.OnWrite()
	}
	if args[0] == "find-generic-password" && k.OnRead != nil {
		k.OnRead()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch args[0] {
	case "find-generic-password":
		k.reads++
		if k.readsFail || (k.ReadsFailAfterWrite && k.wrote) {
			return cclink.SecurityAnswer{Code: interactionNotAllowed}, nil
		}
		if !k.present {
			return cclink.SecurityAnswer{Code: itemNotFound}, nil
		}
		for _, a := range args {
			if a == "-w" {
				return cclink.SecurityAnswer{Stdout: k.secret + "\n"}, nil
			}
		}
		return cclink.SecurityAnswer{}, nil
	case "add-generic-password":
		k.writes++
		if k.WritesRefused {
			return cclink.SecurityAnswer{Code: interactionNotAllowed}, nil
		}
		if k.WriteTimesOutUnlanded {
			k.wrote = true
			return cclink.SecurityAnswer{}, context.DeadlineExceeded
		}
		for i := 1; i+1 < len(args); i++ {
			if args[i] != "-X" {
				continue
			}
			payload, err := hex.DecodeString(args[i+1])
			if err != nil {
				return cclink.SecurityAnswer{Code: 1, Stderr: "bad -X payload"}, nil
			}
			k.secret, k.present, k.wrote = string(payload), true, true
		}
		if k.WriteLandsThenTimesOut {
			return cclink.SecurityAnswer{}, context.DeadlineExceeded
		}
		return cclink.SecurityAnswer{}, nil
	case "delete-generic-password":
		k.secret, k.present = "", false
		return cclink.SecurityAnswer{}, nil
	}
	return cclink.SecurityAnswer{Code: 1, Stderr: "the fake keychain does not know " + args[0]}, nil
}
