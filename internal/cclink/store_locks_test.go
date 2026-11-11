package cclink

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// lockedSpawn is one keychain spawn as a switch made it: the subcommand, the
// bound it was given, and whether Claude Code's credential locks were held.
type lockedSpawn struct {
	op     string
	bound  time.Duration
	locked bool
}

// credentialLocksHeld reports whether somebody holds Claude Code's credential
// locks right now, by trying to take them: a holder in this process is a lock
// directory that already exists, as it would be for Claude Code.
func credentialLocksHeld(t *testing.T) bool {
	t.Helper()
	h, err := cclock.AcquireCredentials(20 * time.Millisecond)
	if err != nil {
		return true
	}
	if err := h.Release(); err != nil {
		t.Fatalf("releasing the probe's locks: %v", err)
	}
	return false
}

// recordingKeychain is an item holding initial whose every spawn is recorded,
// and whose writes land and then report that they ran out of time -- the
// 2026-11-10 write, the one that spends the most time in `security`.
//
// readBack is how long the first read after a write takes, honouring its
// deadline: a keychain still slow from the write it was just asked for. The
// item runs in this process, so no spawn costs the re-exec a fixture process
// does, and a bound well under a second still means what it says.
func recordingKeychain(t *testing.T, initial string, readBack time.Duration) *[]lockedSpawn {
	t.Helper()
	var mu sync.Mutex
	item, wrote, readBackDone := initial, false, false
	var spawns []lockedSpawn
	t.Cleanup(SetSecurityRunnerForTest(func(ctx context.Context, args ...string) (SecurityAnswer, error) {
		deadline, _ := ctx.Deadline()
		s := lockedSpawn{op: args[0], bound: time.Until(deadline), locked: credentialLocksHeld(t)}
		mu.Lock()
		spawns = append(spawns, s)
		slow := args[0] == "find-generic-password" && wrote && !readBackDone
		if slow {
			readBackDone = true
		}
		mu.Unlock()
		if slow {
			select {
			case <-time.After(readBack):
			case <-ctx.Done():
				return SecurityAnswer{}, ctx.Err()
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if args[0] != "add-generic-password" {
			return SecurityAnswer{Stdout: item}, nil
		}
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "-X" {
				payload, err := hex.DecodeString(args[i+1])
				if err != nil {
					t.Errorf("bad -X payload: %v", err)
				}
				item, wrote = string(payload), true
			}
		}
		return SecurityAnswer{}, context.DeadlineExceeded
	}))
	return &spawns
}

// CLAUDE CODE'S LOCKS ARE HELD FOR ONE LOOKUP AND ONE WRITE. Its own token
// refresh waits on them, so every spawn under them is time a session can be
// kept from refreshing. The merge-base read is a lookup and has the lookup
// bound; the write has the write bound; and the read-back of a write that ran
// out of time -- which asks the keychain that was just too slow, under the
// write bound -- runs after the locks are released. A landed write's file then
// follows in a second, short locked pass that spends one more lookup. Before,
// all three spawns ran under the locks at the write bound: about 93 s.
func TestASwitchHoldsClaudeCodesLocksForOneLookupAndOneWrite(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	spawns := recordingKeychain(t, oldLogin, 0)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v, want nil: the write landed", err)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the file did not follow the landed write:\n%s", got)
	}

	// The spawns in order, with the locks each ran under. A locked stretch is
	// one hold of Claude Code's locks; each is bounded by lockedKeychainBound.
	var held time.Duration
	readBack := false
	for i, s := range *spawns {
		switch {
		case s.op == "find-generic-password" && !s.locked:
			if s.bound <= keychainReadTimeout {
				t.Errorf("spawn %d: the read-back has the lookup bound (%s); it asks the keychain the write was "+
					"too slow for", i, s.bound)
			}
			readBack = true
			held = 0
			continue
		case !s.locked:
			t.Errorf("spawn %d: %s ran without Claude Code's locks", i, s.op)
		case s.op == "find-generic-password" && s.bound > keychainReadTimeout:
			t.Errorf("spawn %d: a read under Claude Code's locks has %s, more than the lookup bound %s",
				i, s.bound, keychainReadTimeout)
		}
		held += s.bound + keychainWaitDelay
		if held > lockedKeychainBound() {
			t.Errorf("spawn %d: %s of keychain time under one hold of the locks, past lockedKeychainBound %s",
				i, held, lockedKeychainBound())
		}
	}
	if !readBack {
		t.Fatalf("no read-back ran with the locks released: %+v", *spawns)
	}
}

// The bound itself, at today's values: a lookup and a write, each with the wait
// for its pipes. What it replaced was three spawns at the write bound, 93 s;
// this pins it to well under half of that, so a later change that puts a spawn
// back under the locks has to say so here.
func TestTheKeychainTimeUnderClaudeCodesLocksIsOneLookupAndOneWrite(t *testing.T) {
	want := (keychainReadTimeout + keychainWaitDelay) + (keychainWriteTimeout + keychainWaitDelay)
	if got := lockedKeychainBound(); got != want {
		t.Fatalf("lockedKeychainBound = %s, want one lookup and one write: %s", got, want)
	}
	if got := lockedKeychainBound(); got > 40*time.Second {
		t.Fatalf("lockedKeychainBound = %s; Claude Code's refresh can be held off for that long", got)
	}
}

// The read-back has the WRITE bound, not the lookup bound: it asks the same
// slow keychain that just failed to answer a write in time. Here the read-back
// takes 1.5 s, a lookup is allowed 0.3 s and a write 6 s, so a read-back held
// to the lookup bound cannot confirm the write that landed. The lookups before
// and after it -- the merge base, and the item checked again before the file
// follows -- answer at once.
func TestTheReadBackIsHeldToTheWriteBound(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	recordingKeychain(t, oldLogin, 1500*time.Millisecond)
	saved := [2]time.Duration{keychainReadTimeout, keychainWriteTimeout}
	t.Cleanup(func() { keychainReadTimeout, keychainWriteTimeout = saved[0], saved[1] })
	keychainReadTimeout, keychainWriteTimeout = 300*time.Millisecond, 6*time.Second

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v: the read-back did not get the write bound", err)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the file was not written after a confirmed write:\n%s", got)
	}
}

// The locks are released for the read-back, so the item can move on before the
// second pass takes them again -- Claude Code's own /login, say. The file is
// then not put on the login the item has already replaced: that is an outcome
// nobody knows, which the next pass of the split repair settles.
func TestALandedWriteWhoseItemMovesOnBeforeTheFileFollowsIsUnknown(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	const elsewhere = `{"claudeAiOauth":{"accessToken":"AT-other","refreshToken":"RT-other"}}`
	var mu sync.Mutex
	item, reads := oldLogin, 0
	t.Cleanup(SetSecurityRunnerForTest(func(_ context.Context, args ...string) (SecurityAnswer, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[0] == "add-generic-password" {
			for i := 1; i+1 < len(args); i++ {
				if args[i] == "-X" {
					payload, _ := hex.DecodeString(args[i+1])
					item = string(payload)
				}
			}
			return SecurityAnswer{}, context.DeadlineExceeded
		}
		reads++
		answer := item
		if reads == 2 {
			// The read-back finds the write; Claude Code replaces it at once.
			item = elsewhere
		}
		return SecurityAnswer{Stdout: answer}, nil
	}))

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)})
	if !errors.Is(err, ErrLoginOutcomeUnknown) {
		t.Fatalf("Activate = %v, want ErrLoginOutcomeUnknown", err)
	}
	if got := fileNow(t, file); strings.Contains(got, "RT-new") {
		t.Fatalf("the file followed a write the item no longer holds:\n%s", got)
	}
}
