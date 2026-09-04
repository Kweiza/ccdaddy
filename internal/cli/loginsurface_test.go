package cli

import (
	"strings"
	"testing"
)

// TestTheLoginSurfaceFlagsDoNotContradictTheSurfaceTheySelect pins the one
// place where two correct sentences add up to a wrong answer.
//
// oauth.Surface documents itself precisely: SurfaceClaudeAI is "the right
// answer for Pro, Max, Team, and Enterprise seats", and SurfaceConsole "does
// not mint claude.ai credentials". The --console flag help said "for a
// credit-billed account", and an enterprise seat metered in extra_usage credits
// and nothing else reads itself as exactly that. It is not: such a seat is a
// claude.ai subscription seat whose meter happens to be money, and picking the
// Console surface for it mints a credential from a different issuer.
//
// The failure is silent at the moment it is made — the login succeeds — and
// surfaces later as an account that will not behave, which is the worst shape a
// wrong flag can have. So the two helps are held to naming the SURFACE rather
// than the billing, and the type's own doc is the thing they may not contradict.
func TestTheLoginSurfaceFlagsDoNotContradictTheSurfaceTheySelect(t *testing.T) {
	cmd := newAddClaudeCmd()

	consoleHelp := cmd.Flags().Lookup("console").Usage
	claudeaiHelp := cmd.Flags().Lookup("claudeai").Usage

	// Billing is not the axis these flags select on, and naming it is what sent
	// an enterprise seat to the wrong issuer.
	if strings.Contains(strings.ToLower(consoleHelp), "credit") {
		t.Errorf("--console help = %q; it must not describe itself by BILLING. "+
			"An enterprise seat is credit-billed and still belongs on the claude.ai surface", consoleHelp)
	}
	// It has to say what it actually does, or the flag is only unrecommended
	// rather than explained.
	if !strings.Contains(consoleHelp, "claude.ai") {
		t.Errorf("--console help = %q; it must say it does not mint claude.ai credentials", consoleHelp)
	}
	// The default is the right answer for an enterprise seat, and a reader with
	// one has to be able to see that without opening the source.
	if !strings.Contains(strings.ToLower(claudeaiHelp), "enterprise") {
		t.Errorf("--claudeai help = %q; it must name the seats it covers, enterprise included", claudeaiHelp)
	}
}

// TestTheNoLoginRefusalDoesNotRecommendAnAccountThatCannotBeRanked covers the
// other sentence in this file that reads as advice and is a trap.
//
// With no browser and no terminal, `ccdad add` refused and pointed at
// `ccdad add-token`. That command works, and the account it stores has no
// claudeAiOauth record — so daemon.pollable skips it on every cadence, no
// reading is ever produced, and the account can never be ranked or rotated
// into. `ccdad status` says nothing about it. A person who hit this in a
// container followed the advice and got a fleet that silently does not rotate.
//
// The refusal has to carry that consequence, because the alternative it is
// competing with -- allocate a TTY -- is one line of docker.
func TestTheNoLoginRefusalDoesNotRecommendAnAccountThatCannotBeRanked(t *testing.T) {
	msg := noLoginPathRefusal().Error()

	if !strings.Contains(msg, "add-token") {
		t.Fatalf("refusal = %q; it should still name the fallback that exists", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "rank") {
		t.Errorf("refusal = %q; it must say an add-token account can never be ranked", msg)
	}
	// The refusal fires when stdin is not a terminal, and in a container that is
	// almost always a missing -t rather than a machine with no terminal at all.
	if !strings.Contains(msg, "-t") {
		t.Errorf("refusal = %q; it must name the fix a container reader needs", msg)
	}
}

// TestAddTokenSaysWhatItCostsBeforeSomebodyReachesForIt is the same trap one
// level up from the refusal above.
//
// `add-token` described itself as the thing to "use on a headless machine",
// which is the sentence a person in a container reads right before they create
// an account that can never be ranked. It is also no longer true: `ccdad add
// --no-browser` completes a login with no browser at all, so headless is not
// the distinction. The distinction is whether a REFRESH GRANT comes with the
// credential, and only the OAuth login mints one.
func TestAddTokenSaysWhatItCostsBeforeSomebodyReachesForIt(t *testing.T) {
	long := newAddTokenCmd().Long

	if !strings.Contains(strings.ToLower(long), "rank") {
		t.Errorf("add-token Long = %q\n\nit must say the account it stores can never be ranked", long)
	}
	// The honest alternative has to be named, or the warning reads as "there is
	// no way to do this properly".
	if !strings.Contains(long, "--no-browser") {
		t.Errorf("add-token Long = %q\n\nit must point at the login that DOES mint a grant", long)
	}
}
