package switcher

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// The loop this guard exists to stop, in one test.
//
// Installing a credential Claude Code would refresh on sight makes Claude Code
// refresh it on sight. That rotation moves the refresh token out from under the
// copy ccdad still holds, attribution stops matching, and the next evaluation
// reads "nobody is live" and installs the same dead snapshot again.
func TestExecuteRefusesToInstallACredentialClaudeCodeWouldRefresh(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seedExpiring(t, "u-2", "two@example.com", time.Minute)
	liveAs(t, "u-1")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "u-1", Unattended: true, Now: at(fixedNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Stale {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Stale)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatalf("a stale credential reached the live file: %s", readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got == "u-2" {
		t.Fatal("a refused switch still recorded the target as active")
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a refused switch stamped the anti-flap cooldown")
	}
}

// The boundary is Claude Code's, not one of ccdad's own choosing: a credential
// further out than SelfRefreshThreshold is one Claude Code will simply use.
func TestExecuteInstallsACredentialClaudeCodeWouldUse(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seedExpiring(t, "u-2", "two@example.com", cclink.SelfRefreshThreshold+time.Minute)
	liveAs(t, "u-1")

	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "u-1", Unattended: true, Now: at(fixedNow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2")) {
		t.Fatalf("the credentials file does not hold the target's login: %s", readLive(t))
	}
}

// Refusing is the floor, not the answer. A caller that can refresh the stored
// grant gets the switch it asked for, and what lands is the fresh pair.
func TestExecuteInstallsWhatFreshenReturned(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seedExpiring(t, "u-2", "two@example.com", time.Minute)
	liveAs(t, "u-1")

	var asked []string
	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "u-1", Unattended: true, Now: at(fixedNow),
		Freshen: func(uuid string) (cclink.Blob, error) {
			asked = append(asked, uuid)
			return expiringBlob("RT-u-2-rotated", 8*time.Hour), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if len(asked) != 1 || asked[0] != "u-2" {
		t.Fatalf("Freshen was asked for %v, want one call for u-2", asked)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2-rotated")) {
		t.Fatalf("the credentials file does not hold the freshened login: %s", readLive(t))
	}
}

// A refresh that fails, or that comes back still inside the threshold, leaves
// the account uninstallable. Writing it anyway is the whole bug.
func TestExecuteRefusesWhenFreshenCouldNotHelp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		freshen func(string) (cclink.Blob, error)
	}{
		{"the refresh failed", func(string) (cclink.Blob, error) {
			return nil, errors.New("the token endpoint could not be reached")
		}},
		{"the refresh returned a credential that is still stale", func(string) (cclink.Blob, error) {
			return expiringBlob("RT-u-2", time.Minute), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seed(t, "u-1", "one@example.com")
			target := seedExpiring(t, "u-2", "two@example.com", time.Minute)
			liveAs(t, "u-1")
			before := readLive(t)

			res, err := Execute(openStore(t), Request{
				Target: target, LiveUUID: "u-1", Unattended: true,
				Now: at(fixedNow), Freshen: tc.freshen,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != Stale {
				t.Fatalf("outcome = %v, want %v", res.Outcome, Stale)
			}
			if !bytes.Equal(before, readLive(t)) {
				t.Fatalf("a stale credential reached the live file: %s", readLive(t))
			}
		})
	}
}

// The one account whose grant ccdad must NOT spend from here is the one Claude
// Code is logged in as: rotating it behind a running session is the hazard this
// whole path exists to avoid. An already-on call is a no-op, and a no-op has no
// business touching the token endpoint.
func TestExecuteDoesNotFreshenTheAccountItBelievesIsLive(t *testing.T) {
	isolate(t)
	target := seedExpiring(t, "u-1", "one@example.com", time.Minute)
	liveAs(t, "u-1")

	called := false
	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "u-1", Unattended: true, Now: at(fixedNow),
		Freshen: func(string) (cclink.Blob, error) {
			called = true
			return expiringBlob("RT-u-1", 8*time.Hour), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("Execute spent the live account's refresh grant on an already-on no-op")
	}
	if res.Outcome != AlreadyOn {
		t.Fatalf("outcome = %v, want %v", res.Outcome, AlreadyOn)
	}
}
