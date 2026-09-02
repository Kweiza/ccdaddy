package switcher

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedCodex stores an account whose only credential is a Codex record. It is
// the shape that must never reach any of the three Claude paths below.
func seedCodex(t *testing.T, uuid, email string) store.Account {
	t.Helper()
	s := openStore(t)
	a := store.Account{UUID: uuid, Email: email, Provider: provider.Codex}
	blob := cclink.Blob{"codexOAuth": json.RawMessage(
		`{"access_token":"AT-` + uuid + `","refresh_token":"RT-` + uuid + `","account_id":"acct","user_id":"` + uuid + `"}`)}
	if err := s.Add(a, blob); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("seedCodex: %s did not land in the store", uuid)
	}
	return got
}

// The candidate builder is asked for one provider and answers about that
// provider only, in BOTH directions.
//
// The Codex direction is the one that costs money if it is wrong: the engine
// would rank an account with no Claude login, win with it, and hand it to the
// switch that rewrites Claude Code's credentials file. The Claude direction
// matters for the mirror reason -- the Codex lane's own builder must not be
// handed a Claude account to spend a Codex quota with.
func TestTheCandidateBuilderAnswersAboutOneProviderOnly(t *testing.T) {
	isolate(t)
	seed(t, "u-claude-1", "a@example.com")
	seed(t, "u-claude-2", "b@example.com")
	seedCodex(t, "u-codex-1", "c@example.com")

	s := openStore(t)
	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		p    provider.ID
		want []string
	}{
		{"claude", provider.Claude, []string{"u-claude-1", "u-claude-2"}},
		{"codex", provider.Codex, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := engineCandidates(s, s.Accounts(), cache, tc.p)
			if len(got) != len(tc.want) {
				t.Fatalf("engineCandidates(%s) returned %d candidates, want %d", tc.p, len(got), len(tc.want))
			}
			for i, c := range got {
				if c.UUID != tc.want[i] {
					t.Errorf("candidate %d = %s, want %s", i, c.UUID, tc.want[i])
				}
			}
		})
	}
}

// Installable is the skip engineCandidates applies first, and it must keep
// refusing a Codex blob on its own.
//
// This is the load-bearing half of the Codex lane's design: Installable drops
// every codexOAuth blob before any provider check runs, which is exactly why
// the Codex lane cannot reuse engineCandidates and builds its own list.
func TestInstallableStillRefusesACodexBlob(t *testing.T) {
	blob := cclink.Blob{"codexOAuth": json.RawMessage(`{"access_token":"AT","refresh_token":"RT"}`)}
	if Installable(blob, nil) {
		t.Fatal("Installable accepted a Codex credential; there is no Claude login in one to install")
	}
}

// Evaluate's zero Provider is Claude, so the five existing call sites stay on
// today's path without naming a provider.
func TestEvaluateDefaultsToClaude(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "a@example.com")
	seedCodex(t, "u-codex-1", "c@example.com")

	s := openStore(t)
	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	var zero EvalOptions
	if got := engineCandidates(s, s.Accounts(), cache, zero.provider()); len(got) != 1 || got[0].UUID != "u-1" {
		t.Fatalf("a zero EvalOptions built %+v, want only the Claude account", got)
	}
}

// The switch itself refuses a Codex target BEFORE it reads a credential.
//
// Ahead of the read for a reason: the Codex account's credential file exists
// and is readable, so the read succeeds and the refusal that follows would be
// "this account has no Claude Code login" -- a true sentence that sends a user
// to `ccdad add` for an account that is logged in perfectly well.
func TestExecuteRefusesANonClaudeTarget(t *testing.T) {
	isolate(t)
	target := seedCodex(t, "u-codex-1", "c@example.com")

	s := openStore(t)
	res, err := Execute(s, Request{Target: target})
	if !errors.Is(err, ErrNotClaude) {
		t.Fatalf("Execute() = %v, want ErrNotClaude", err)
	}
	if res.Outcome != NotSwitched {
		t.Fatalf("Outcome = %v, want NotSwitched", res.Outcome)
	}
}

// Controller addition: LiveStateOf pins two properties this task's other
// tests do not exercise directly, so a later part cannot quietly change
// either. Both already hold with NO production change -- this test is added
// to fix the behaviour in place, not to drive new code.
//
//   - A live file with no login answers LiveNone, regardless of who is in the
//     pool.
//   - A live Claude login already in the pool never attributes to a Codex row
//     that happens to share it: CredentialIdentity reads claudeAiOauth, which
//     the Codex blob seedCodex stores (codexOAuth) never has, so the two
//     identities can never compare equal.
func TestLiveStateOfNeverAttributesACodexAccount(t *testing.T) {
	isolate(t)
	claude := seed(t, "u-claude-1", "a@example.com")
	seedCodex(t, "u-codex-1", "c@example.com")

	s := openStore(t)
	accounts := s.Accounts()

	if _, state := LiveStateOf(nil, accounts, s.Credentials); state != LiveNone {
		t.Fatalf("LiveStateOf(nil live) state = %v, want LiveNone", state)
	}

	got, state := LiveStateOf(liveLogin("RT-"+claude.UUID), accounts, s.Credentials)
	if got.Provider == provider.Codex {
		t.Fatalf("LiveStateOf attributed a live Claude login to the Codex account %+v (state=%v)", got, state)
	}
}
