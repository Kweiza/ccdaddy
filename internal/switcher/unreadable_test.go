package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// breakLiveStore makes cclink.Load fail the way a locked keychain does: an
// error rather than an empty answer. The symlink is the portable stand-in --
// openCredentialsFile opens O_NOFOLLOW and refuses it, which is the same shape
// as errSecInteractionNotAllowed and reachable off macOS.
func breakLiveStore(t *testing.T) {
	t.Helper()
	path := credsPath(t)
	_ = os.Remove(path)
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := cclink.Load(); !errors.Is(err, cclink.ErrSymlink) {
		t.Fatalf("precondition: cclink.Load must fail, got %v", err)
	}
}

// THE REGRESSION. A store that could not be read used to be handed to
// LiveStateOf as a nil blob, which answers LiveNone -- "nobody is logged in,
// and a swap has nothing to overwrite". That is the most dangerous of the four
// readings: Claude Code's own combinator falls back to the credentials file, so
// a session can be running perfectly well underneath a store ccdad cannot see.
func TestEvaluateSaysUnreadableRatherThanNobodyLive(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	breakLiveStore(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	ev, err := Evaluate(s, EvalOptions{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if ev.LiveState == LiveNone {
		t.Fatal("an unreadable store was reported as 'nobody is logged in'")
	}
	if ev.LiveState != LiveUnreadable {
		t.Fatalf("LiveState = %v, want LiveUnreadable", ev.LiveState)
	}
	if ev.LiveErr == nil {
		t.Fatal("LiveErr is nil; the reason the store could not be read must travel")
	}
	if ev.LiveKnown {
		t.Fatal("LiveKnown is true for a store that could not be read")
	}
}

// A readable store still answers the other three, so the new state cannot be
// reached by anything but a failed read.
func TestEvaluateStillDistinguishesTheOtherThree(t *testing.T) {
	isolate(t)
	a := seed(t, "u-1", "one@example.com")

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	writeLive(t, `{}`)
	if ev, err := Evaluate(s, EvalOptions{}); err != nil || ev.LiveState != LiveNone {
		t.Fatalf("empty file: LiveState = %v (err %v), want LiveNone", ev.LiveState, err)
	}
	liveAs(t, a.UUID)
	if ev, err := Evaluate(s, EvalOptions{}); err != nil || ev.LiveState != LiveManaged {
		t.Fatalf("managed login: LiveState = %v (err %v), want LiveManaged", ev.LiveState, err)
	}
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-X","refreshToken":"RT-STRANGER"}}`)
	if ev, err := Evaluate(s, EvalOptions{}); err != nil || ev.LiveState != LiveUnattributed {
		t.Fatalf("stranger's login: LiveState = %v (err %v), want LiveUnattributed", ev.LiveState, err)
	}
}

// Each state says something a reader can act on, and the two new-ish ones must
// not read alike: one waits for an identity, the other for the machine.
func TestLiveStateNamesAllFour(t *testing.T) {
	seen := map[string]bool{}
	for _, st := range []LiveState{LiveNone, LiveManaged, LiveUnattributed, LiveUnreadable} {
		name := st.String()
		if name == "unknown" || seen[name] {
			t.Fatalf("LiveState(%d).String() = %q, which is unnamed or a duplicate", st, name)
		}
		seen[name] = true
	}
}
