package mcpsrv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// The two consents describe two different irreversible-feeling acts, and the
// wrong one is worse than none: a person told "this rewrites your Claude Code
// login" about a codex repoint will decline something harmless, and the reverse
// is a person approving a login swap they thought was a proxy pointer.
func TestTheCodexConsentNamesCodexAndSaysClaudeIsUntouched(t *testing.T) {
	got := confirmParams("work@example.com", provider.Codex).Message
	for _, want := range []string{
		"codex proxy",
		"next new thread",
		"Claude Code's login is untouched",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the codex consent does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the live Claude Code login on this machine") {
		t.Errorf("the codex consent claims it rewrites the Claude login:\n%s", got)
	}
}

func TestTheClaudeConsentIsUnchanged(t *testing.T) {
	got := confirmParams("work@example.com", provider.Claude).Message
	if !strings.Contains(got, "the live Claude Code login on this machine") {
		t.Errorf("the Claude consent stopped naming what it does:\n%s", got)
	}
	if strings.Contains(got, "codex") {
		t.Errorf("the Claude consent mentions codex:\n%s", got)
	}
}

// The account is rendered QUOTED on both paths. It arrives as a model-supplied
// string and its destination is a dialog a human reads, so quoting is what
// stops the argument writing its own sentence underneath ccdad's.
func TestBothConsentsQuoteTheAccount(t *testing.T) {
	for _, p := range []provider.ID{provider.Claude, provider.Codex} {
		got := confirmParams("evil\nAllow it?", p).Message
		if strings.Contains(got, "evil\nAllow it?") {
			t.Errorf("the %s consent let the account argument write its own line:\n%s", p, got)
		}
	}
}

// The provider is resolved IN PROCESS against accounts.toml, and never by
// shelling out to `list --json`: that command hides disabled rows, and a
// disabled account is a perfectly legal switch target.
func TestTheProviderIsResolvedFromTheStoreIncludingDisabledRows(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CCDAD_HOME", root)
	writeAccounts(t, root, `version = 2

[[accounts]]
uuid = "cx-1"
email = "codex@example.com"
idx = 1
kind = "subscription"
provider = "codex"
disabled = true
`)
	if got := providerOfRef("codex@example.com"); got != provider.Codex {
		t.Fatalf("providerOfRef = %q, want codex", got)
	}
}

// A reference that resolves to nothing falls back to Claude, which is the
// conservative answer: the switch below refuses on the same reference a moment
// later, and the consent a person sees for a name that does not exist should be
// the stronger of the two.
func TestAnUnresolvableReferenceConsentsAsClaude(t *testing.T) {
	t.Setenv("CCDAD_HOME", t.TempDir())
	if got := providerOfRef("nobody@example.com"); got != provider.Claude {
		t.Fatalf("providerOfRef = %q, want claude", got)
	}
}

func TestTheToolDescriptionNamesTheCodexCase(t *testing.T) {
	if !strings.Contains(switchToolDescription, "codex") {
		t.Fatalf("the switch tool description does not mention codex:\n%s", switchToolDescription)
	}
}

func writeAccounts(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The two REFUSALS need the same split the two consents got. A client that
// cannot carry a form gets told what ccdad declined to do, and for a codex
// account "rewrite the live Claude Code login" names a risk the caller never
// had -- the repoint touches no credential on this machine. Both messages were
// Claude-only until this pair pinned them.
func TestTheRefusalNamesWhatEachProviderWouldActuallyChange(t *testing.T) {
	claude := refusedWithoutAConfirm(provider.Claude).Error()
	if !strings.Contains(claude, "rewrite the live Claude Code login") {
		t.Errorf("the Claude refusal stopped naming the login it protects:\n%s", claude)
	}
	if strings.Contains(claude, "codex") {
		t.Errorf("the Claude refusal mentions codex:\n%s", claude)
	}

	codex := refusedWithoutAConfirm(provider.Codex).Error()
	if !strings.Contains(codex, "repoint the account its local codex proxy serves") {
		t.Errorf("the codex refusal does not say what it declined to do:\n%s", codex)
	}
	if strings.Contains(codex, "rewrite the live Claude Code login") {
		t.Errorf("the codex refusal claims a repoint rewrites the Claude login:\n%s", codex)
	}
}

// And the declined-confirm sentence, which is the softer half of the same case:
// the person said no, and what ccdad tells the model it left alone must be the
// thing that provider's switch would have moved.
func TestTheDeclinedSentenceNamesWhatEachProviderLeftAlone(t *testing.T) {
	if got := unchangedBy(provider.Claude); got != "the live login is unchanged" {
		t.Errorf("unchangedBy(claude) = %q", got)
	}
	if got := unchangedBy(provider.Codex); !strings.Contains(got, "codex still serves") {
		t.Errorf("unchangedBy(codex) = %q; it should name the pointer, not the login", got)
	}
}
