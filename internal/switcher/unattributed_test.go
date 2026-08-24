package switcher

import (
	"bytes"
	"testing"
)

// The other half of the loop, and the half that decides WHOSE login gets
// overwritten.
//
// When Claude Code rotates the live login, the file stops matching every stored
// snapshot. Read as "nobody is live" that is a green light: no baseline means no
// hysteresis and no cooldown, the ranking names a target, and the swap installs
// over a session that is running. Read as "cannot tell" it is a stop.
func TestAnUnattendedExecuteStandsDownOnALoginItCannotName(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	// What Claude Code's own refresh leaves behind: u-1's login, carrying the
	// pair the server rotated to, which matches no stored snapshot.
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-rotated","refreshToken":"RT-u-1-rotated",`+
		`"scopes":["user:inference","user:profile"]}}`)
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unattributed {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Unattributed)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatalf("an unattended swap overwrote a login it could not name: %s", readLive(t))
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a refused switch stamped the anti-flap cooldown")
	}
}

// Standing down forever is the other way to break this. A caller that has
// POSITIVELY established the login is not one of ours — the profile endpoint
// answered, and the answer was an account this store does not hold — gets the
// switch, because that is the machine ccdad exists to move off.
func TestAnUnattendedExecuteProceedsOnALoginEstablishedAsForeign(t *testing.T) {
	isolate(t)
	target := seed(t, "u-2", "two@example.com")
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-someone-else","refreshToken":"RT-unmanaged",`+
		`"scopes":["user:inference","user:profile"]}}`)

	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "", Unattended: true, LiveForeign: true,
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

// An empty credentials file is not an unnameable login, it is no login. Reading
// the two the same way would leave a machine nobody is logged in on stuck.
func TestAnUnattendedExecuteProceedsWhenNobodyIsLoggedIn(t *testing.T) {
	isolate(t)
	target := seed(t, "u-2", "two@example.com")
	writeLive(t, `{}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
}

// Attended, this is a report and not a refusal: a human typed the command and
// is watching the result.
func TestAnAttendedExecuteProceedsThroughALoginItCannotName(t *testing.T) {
	isolate(t)
	target := seed(t, "u-2", "two@example.com")
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-rotated","refreshToken":"RT-u-1-rotated",`+
		`"scopes":["user:inference","user:profile"]}}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if res.LiveState != LiveUnattributed {
		t.Fatalf("LiveState = %v, want %v", res.LiveState, LiveUnattributed)
	}
}

// The claim to be careful about: LiveForeign is the caller's finding about the
// file it read, so if the file changed underneath into a MANAGED account's
// login, the finding no longer describes it and the swap stands down as a race
// like any other.
func TestLiveForeignDoesNotSurviveTheFileBecomingManaged(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "", Unattended: true, LiveForeign: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Raced {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Raced)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatal("a stale foreign finding let the swap write anyway")
	}
}
