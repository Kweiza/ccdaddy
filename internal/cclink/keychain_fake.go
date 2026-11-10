package cclink

import (
	"context"
	"encoding/hex"
	"sync"
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
	mu      sync.Mutex
	secret  string
	present bool
	wrote   bool

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
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "find-generic-password":
		if f.ReadsFailAfterWrite && f.wrote {
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
