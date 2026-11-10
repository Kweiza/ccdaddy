package cclink

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// FakeKeychain is an in-memory credential item, for tests in OTHER packages.
// Production code must never install one.
//
// It exists because the behaviour that depends on the keychain -- the daemon's
// split-login repair, `ccdad switch`'s no-op rule, doctor's split row -- lives
// in internal/switcher, internal/daemon and internal/cli, and a test there has
// no other way to put an item on a machine without touching the developer's
// own keychain. cclink's own tests run a fixture `security` process instead,
// which is what exercises the exec layer; this replaces that layer whole, the
// way credhome.SetTryLockForTest replaces a lock primitive, so every line ABOVE
// it -- the bounds, the read-back of an unconfirmed write, the item-first
// order -- runs as it does on a Mac.
//
// It is one item. The derived item name is not consulted: every lookup and
// write lands on the same secret.
type FakeKeychain struct {
	mu        sync.Mutex
	secret    string
	present   bool
	wrote     bool
	writes    int
	readsFail bool
	reads     int

	// OnWrite, when set, runs at the start of every write, outside the
	// fake's lock: a test advances its clock there to stand for a write that
	// takes as long as a slow keychain's does.
	OnWrite func()

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

// NewFakeKeychain is an item holding secret (compact JSON, as Claude Code
// writes it).
func NewFakeKeychain(secret string) *FakeKeychain {
	return &FakeKeychain{secret: secret, present: true}
}

// Secret is what the item holds now, and whether there is an item.
func (f *FakeKeychain) Secret() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secret, f.present
}

// Reads is how many lookups the item has been asked for.
func (f *FakeKeychain) Reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// Writes is how many writes the item has been asked for.
func (f *FakeKeychain) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// Set replaces what the item holds, as Claude Code writing it would.
func (f *FakeKeychain) Set(secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secret, f.present = secret, true
}

// Heal turns every failure mode off: the keychain answers normally from now on,
// the way a loaded Mac does once its load drops.
func (f *FakeKeychain) Heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WriteLandsThenTimesOut, f.WriteTimesOutUnlanded, f.WritesRefused, f.ReadsFailAfterWrite = false, false, false, false
	f.readsFail = false
}

// FailReads makes every read fail (exit 36) while on is true, as a keychain
// that has locked does.
func (f *FakeKeychain) FailReads(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readsFail = on
}

// IsolateKeychainForTests is what a test package's TestMain calls before
// m.Run: an empty FakeKeychain -- no item, which is what a sandboxed Claude
// Code home has on any machine -- becomes the default, and spawning the real
// /usr/bin/security from then on panics, so a test that reaches the
// developer's own keychain fails loudly instead of reading it. A test that
// needs an item installs its own fake with SetKeychainForTest, and its
// cleanup restores this default. Production code must never call it.
func IsolateKeychainForTests() {
	SetKeychainForTest(&FakeKeychain{})
	realSecurityForbidden.Store(true)
}

// realSecurityForbidden is set by IsolateKeychainForTests.
var realSecurityForbidden atomic.Bool

// KeychainIsolatedForTests reports whether IsolateKeychainForTests has run in
// this process: a test re-executing its own binary asks it of the child, to
// prove the child cannot reach the real keychain either.
func KeychainIsolatedForTests() bool { return realSecurityForbidden.Load() }

// refuseRealSecurity panics when a test package has forbidden the real binary.
func refuseRealSecurity(args ...string) {
	if realSecurityForbidden.Load() {
		panic(fmt.Sprintf("a test spawned the real %s %v: the keychain is isolated for tests "+
			"(cclink.IsolateKeychainForTests); install a FakeKeychain instead", securityPath, args))
	}
}

// SetKeychainForTest makes f this process's keychain and returns a function
// restoring the previous one. Production code must never call it.
func SetKeychainForTest(f *FakeKeychain) (restore func()) {
	securityImplMu.Lock()
	prev := securityImpl
	securityImpl = f.run
	securityImplMu.Unlock()
	return func() {
		securityImplMu.Lock()
		securityImpl = prev
		securityImplMu.Unlock()
	}
}

// run answers one subcommand the way /usr/bin/security would.
func (f *FakeKeychain) run(_ context.Context, _ time.Duration, args ...string) (securityResult, error) {
	if args[0] == "add-generic-password" && f.OnWrite != nil {
		f.OnWrite()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "find-generic-password":
		f.reads++
		if f.readsFail || (f.ReadsFailAfterWrite && f.wrote) {
			return securityResult{code: 36}, nil
		}
		if !f.present {
			return securityResult{code: securityNotFoundCode}, nil
		}
		for _, a := range args {
			if a == "-w" {
				return securityResult{stdout: f.secret + "\n"}, nil
			}
		}
		return securityResult{}, nil
	case "add-generic-password":
		f.writes++
		if f.WritesRefused {
			return securityResult{code: 36}, nil
		}
		if f.WriteTimesOutUnlanded {
			f.wrote = true
			return securityResult{}, &KeychainError{Op: args[0], failure: failTimedOut}
		}
		for i := 1; i+1 < len(args); i++ {
			if args[i] != "-X" {
				continue
			}
			payload, err := hex.DecodeString(args[i+1])
			if err != nil {
				return securityResult{code: 1, stderr: "bad -X payload"}, nil
			}
			f.secret, f.present, f.wrote = string(payload), true, true
		}
		if f.WriteLandsThenTimesOut {
			return securityResult{}, &KeychainError{Op: args[0], failure: failTimedOut}
		}
		return securityResult{}, nil
	case "delete-generic-password":
		f.secret, f.present = "", false
		return securityResult{}, nil
	}
	return securityResult{code: 1, stderr: "the fake keychain does not know " + args[0]}, nil
}
