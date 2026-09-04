package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// bothLogins is what a message about an empty fleet has to hand over. Neither
// half is the default: `ccdad add` names no provider and exits 2, and either
// leaf on its own sends half the readers of this sentence to the wrong login.
var bothLogins = []string{"ccdad add claude", "ccdad add codex"}

// TestTheNoAccountsAdviceNamesBothProviders covers the sentences that fire when
// there is no account AT ALL, on any provider.
//
// These are the four the rename gets wrong in the one direction that is
// invisible: replacing `ccdad add` with `ccdad add claude` everywhere is right
// in every message that already knows it is talking about a Claude account, and
// wrong in exactly these -- they cannot know, because there is nothing in the
// store to know it from. A codex-only user who has just wiped their store, or
// who is reading `ccdad own` on a fresh machine, would be sent to a browser
// login for the other provider and end up with an account they did not want.
//
// The sentences are read through the commands rather than as literals, because
// a literal that no path reaches is not advice. `ccdad status` and `ccdad
// runway` write theirs to stderr as a notice; `ccdad own` refuses at exit 2.
func TestTheNoAccountsAdviceNamesBothProviders(t *testing.T) {
	// `ccdad own` with a reference names the reference in the refusal, so the
	// empty-store arm has to be reached with no arguments at all -- which is a
	// QUESTION on a store that has accounts, and this refusal on one that
	// does not.
	surfaces := map[string][]string{
		"status": {"status"},
		"runway": {"runway"},
		"own":    {"own"},
	}
	for name, argv := range surfaces {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			suppressAutoStart(t)

			_, _, stderr, top := runRoot(t, argv...)
			said := stderr + top
			for _, want := range bothLogins {
				if !strings.Contains(said, want) {
					t.Errorf("`ccdad %s` on an empty store does not name `%s`; it cannot know which "+
						"provider the reader wants, so it has to offer both:\n%s",
						strings.Join(argv, " "), want, said)
				}
			}
		})
	}
}

// TestTheCodexAdviceNamesTheLoginThatExists holds the other half: a message
// that DOES know it is talking about a Codex account must name the Codex login,
// and must not still be spelling it the way one release spelled it.
//
// The scripts package walks every literal in the tree and refuses one the
// command tree cannot walk, which is what caught these. This asks the narrower
// question the walk cannot: that the replacement is the CODEX login and not
// Claude's, which resolves just as well and would send a user whose codex grant
// has died into a browser.
func TestTheCodexAdviceNamesTheLoginThatExists(t *testing.T) {
	isolate(t)

	// `ccdad switch --provider codex` on a store with no codex account at all.
	_, _, stderr, top := runRoot(t, "switch", "--provider", "codex")
	said := stderr + top
	if !strings.Contains(said, "ccdad add codex") {
		t.Errorf("`ccdad switch --provider codex` on a codex-less store does not name the codex login:\n%s", said)
	}

	// The two long helps that carry the same advice, read from the commands
	// rather than from a run: `ccdad run`'s refusal of `codex login` is
	// reachable only with a daemon and a routed session.
	longs := map[string]string{
		"ccdad run":       subcommandOf(t, NewRootCmd(), "run").Long,
		"ccdad add codex": newAddCodexCmd().Long,
	}
	for name, text := range longs {
		if strings.Contains(text, "ccdad codex add") {
			t.Errorf("%s's help still spells the codex login `ccdad codex add`, which exits 2:\n%s", name, text)
		}
	}
}

// TestTheClaudeOnlyAdviceNamesTheClaudeLogin is the mirror of the two above,
// for the messages that know they are about a Claude credential: the browser
// login is `ccdad add claude` now, and the bare group it used to be spelled as
// runs nothing.
//
// Each of these three sentences is read by somebody whose fleet has already
// stopped rotating, which is the worst moment to hand out a command line that
// answers with a usage error.
func TestTheClaudeOnlyAdviceNamesTheClaudeLogin(t *testing.T) {
	// The login-store arm is the one that carries the advice: an environment
	// token gets the blunt sentence instead, with no command line in it.
	notice := unattributedNotice(switcher.Attribution{Via: "the credentials file", FromLoginStore: true})
	said := map[string]string{
		"the no-login-path refusal": noLoginPathRefusal().Error(),
		"the unattributed notice":   notice,
		"add-token's help":          subcommandOf(t, NewRootCmd(), "add-token").Long,
	}
	for name, text := range said {
		if !strings.Contains(text, "ccdad add claude") {
			t.Errorf("%s does not name the Claude login:\n%s", name, text)
		}
	}
}
