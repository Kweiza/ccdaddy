package switcher

import (
	"bytes"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/credhome"
)

// standDown makes the claim answer "another store's engine is driving this
// login" for the length of a test.
//
// Only the VERDICT is stood in for. What a claim is, and that it actually
// excludes a second process, is a kernel fact proved in internal/credhome
// against a real second process — a fake cannot establish one. What lives here
// is what the executor DOES with the answer, and that is what these exercise.
func standDown(t *testing.T, store string) {
	t.Helper()
	saved := claimVerdict
	t.Cleanup(func() { claimVerdict = saved })
	claimVerdict = func() credhome.Verdict {
		return credhome.Verdict{StandDown: true, Owner: credhome.Owner{Store: store, PID: 4242}}
	}
}

// The unattended half: an engine nobody is watching does not fight another
// store's engine for the login, because the two would undo each other forever.
func TestUnattendedStandsDownWhenAnotherStoreDrivesTheLogin(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	standDown(t, "/other/ccdad")

	before := readLive(t)
	s := openStore(t)
	res, err := Execute(s, Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Contended {
		t.Fatalf("outcome = %v, want Contended", res.Outcome)
	}
	// The file, not just the outcome. An outcome is a label; what matters is
	// that nothing was written, and only the bytes can say so.
	if !bytes.Equal(before, readLive(t)) {
		t.Errorf("the credentials file was rewritten by a stand-down:\nbefore %s\nafter  %s", before, readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got == "u-2" {
		t.Error("a stand-down recorded the target as active")
	}
	if res.Claim.Owner.Store != "/other/ccdad" {
		t.Errorf("Result.Claim.Owner.Store = %q, want the holder to be carried to the caller", res.Claim.Owner.Store)
	}
}

// Contended is decided BEFORE Raced, and the ordering is a choice rather than
// an accident.
//
// Both preconditions are true at once in the ordinary case — another store's
// engine holding the login is the likeliest reason the live account moved — and
// the two answers send the operator to different places. "The login changed
// underneath us" describes a symptom and suggests waiting; "another engine is
// driving this login" names the cause and says what to change.
//
// The fixture makes them disagree on purpose: the caller decided against u-1
// and the file says u-9, so a Raced-first implementation returns Raced here.
func TestContendedOutranksRaced(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-9", "nine@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-9")
	standDown(t, "/other/ccdad")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == Raced {
		t.Fatal("outcome = Raced, want Contended — the race is the other engine's doing, and saying so " +
			"is what tells the operator which machine to change")
	}
	if res.Outcome != Contended {
		t.Fatalf("outcome = %v, want Contended", res.Outcome)
	}
}

// The attended half: a human typed the command and is watching, so the switch
// happens and the warning rides along. Refusing here would decline what somebody
// just asked for on account of a machine they can see.
func TestAttendedSwitchesAnywayAndCarriesTheWarning(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	standDown(t, "/other/ccdad")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want Switched — an attended switch is never refused for this", res.Outcome)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2")) {
		t.Fatalf("the attended switch did not install the target: %s", readLive(t))
	}
	if !res.Claim.StandDown {
		t.Error("Result.Claim did not carry the warning, so the caller has nothing to print")
	}
}

// Already-on outranks the claim. The target IS the login, so there is nothing
// for two engines to disagree about, and reporting contention would send a user
// to change a machine over a switch that was never going to write anything.
func TestAlreadyOnOutranksContended(t *testing.T) {
	isolate(t)
	target := seed(t, "u-1", "one@example.com")
	liveAs(t, "u-1")
	standDown(t, "/other/ccdad")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyOn {
		t.Fatalf("outcome = %v, want AlreadyOn", res.Outcome)
	}
}

// Contended has to render as something other than the default. Outcome.String's
// default arm says "the switch did not complete", which is true of every
// stand-down and tells an operator nothing about which one this is.
func TestContendedRendersItsOwnSentence(t *testing.T) {
	got := Contended.String()
	if got == NotSwitched.String() {
		t.Fatalf("Contended.String() = %q, which is the default arm's sentence", got)
	}
	if !bytes.Contains([]byte(got), []byte("store")) {
		t.Errorf("Contended.String() = %q, want it to say another STORE is involved — that is the one "+
			"fact distinguishing it from every other stand-down", got)
	}
}
