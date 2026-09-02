package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A probe spends a turn of quota against Claude Code to wake a window. There
// is no such thing for a Codex account: nothing ccdad runs is Codex's client,
// and the windows a probe wakes are Claude's.
//
// Nothing else in this function catches a Codex account. The credential test
// looks for a ccdadToken record and a Codex blob has none, the install and
// displaced-auth gates say nothing about a provider, and an entry with no probe
// history is allowed to probe -- so today probeSkip falls all the way through
// and the turn is spent. The table below is for the second half of the claim:
// the check has to sit ahead of the --force early return too.
func TestProbeSkipRefusesACodexAccountFirst(t *testing.T) {
	// isolate FIRST, and it is not decoration. probeSkip reaches
	// refuseDisplacedAuth before the --force early return, and that gate reads
	// ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN, CLAUDE_BG_AUTH_SNAPSHOT_PATH,
	// CLAUDE_CODE_SIMPLE and the apiKeyHelper setting under CLAUDE_CONFIG_DIR.
	// On a developer shell exporting any of them this test would answer out of
	// that shell rather than out of the code under test.
	isolate(t)
	codex := store.Account{UUID: "u-codex", Email: "c@example.com", Provider: provider.Codex}
	blob := cclink.Blob{"codexOAuth": json.RawMessage(`{"access_token":"AT","refresh_token":"RT"}`)}

	for _, tc := range []struct {
		name string
		o    probeOptions
	}{
		{"ordinary", probeOptions{window: usage.WindowFiveHour, now: time.Now()}},
		// The daemon spawns `probe --uuid <u> --force` as a child, so the
		// force branch is a real caller and not a flag a person types by hand.
		// A check placed after the early return would let exactly that child
		// through.
		{"forced", probeOptions{window: usage.WindowFiveHour, now: time.Now(), force: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why, skip := probeSkip(codex, blob, usage.Entry{}, tc.o)
			if !skip {
				t.Fatalf("probeSkip() = %q, false; want it to refuse a Codex account", why)
			}
			if !strings.Contains(why, "Codex") {
				t.Errorf("probeSkip() said %q, want it to name the provider rather than the credential", why)
			}
			if strings.Contains(why, "OAuth login") {
				t.Errorf("probeSkip() answered with the credential sentence: %q", why)
			}
		})
	}
}

// A Claude account with an ordinary login is untouched by the new check: it
// falls through to the gates it always fell through to.
func TestProbeSkipStillLetsAClaudeAccountThrough(t *testing.T) {
	// isolate for the reason above: without it a shell that exports one of the
	// auth variables fails this test with a displaced-auth refusal.
	isolate(t)
	claude := store.Account{UUID: "u-1", Email: "a@example.com", Provider: provider.Claude}
	o := probeOptions{window: usage.WindowFiveHour, now: time.Now(), force: true}
	if why, skip := probeSkip(claude, credsFor("RT-u-1"), usage.Entry{}, o); skip {
		t.Fatalf("probeSkip() refused a forced Claude probe: %q", why)
	}
}
