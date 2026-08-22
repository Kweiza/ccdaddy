package identity_test

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
)

// identity and cclink each spell the approval rule, because identity must not
// depend on the package that writes Claude Code's files. Two spellings of one
// rule drift, and the drift is invisible: cclink WRITES the entry and identity
// MATCHES against it, so a divergence produces a config ccdad wrote and ccdad
// then fails to recognise. This is the test that comment promises.
//
// It lives in identity_test rather than identity so importing cclink cannot
// become a real dependency by accident.
func TestAPIKeyApprovalAgreesWithCclink(t *testing.T) {
	for _, key := range []string{
		"sk-ant-api03-HEADHEADzzzzTAILTAILTAIL",
		"  sk-ant-api03-WHITESPACE-AROUND-IT  ",
		"short",
		"",
		"exactly-twenty-chars",
	} {
		if got, want := identity.APIKeyApproval(key), cclink.APIKeyApproval(key); got != want {
			t.Errorf("APIKeyApproval(%q): identity = %q, cclink = %q", key, got, want)
		}
	}
}

// The environment key wins outright in a non-interactive session and needs an
// approval entry in an interactive one. Both halves are asserted from the same
// inputs, because the pair is the rule -- a model that always required approval
// would report "not managed" for a `claude -p` that is using the key, and one
// that never required it would report the opposite for an interactive session.
func TestEnvKeyApprovalIsAnInteractiveGateOnly(t *testing.T) {
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	unapproved := identity.APIKeyEnvironment{EnvKey: key, Interactive: true}
	if got, source := unapproved.Resolve(); source == identity.APIKeyEnv {
		t.Errorf("interactive Resolve() = %q from %v; an unapproved key is refused", got, source)
	}
	if !unapproved.EnvKeyNeedsApproval() {
		t.Error("EnvKeyNeedsApproval() = false for an unapproved interactive key")
	}

	scripted := unapproved
	scripted.Interactive = false
	if got, source := scripted.Resolve(); source != identity.APIKeyEnv || got != key {
		t.Errorf("non-interactive Resolve() = %q from %v; want the key outright", got, source)
	}

	approved := unapproved
	approved.Approved = []string{identity.APIKeyApproval(key)}
	if got, source := approved.Resolve(); source != identity.APIKeyEnv || got != key {
		t.Errorf("approved interactive Resolve() = %q from %v; want the key", got, source)
	}
	if approved.EnvKeyNeedsApproval() {
		t.Error("EnvKeyNeedsApproval() = true for an approved key")
	}
}

// A stored key is the LOWEST-priority source, so anything else that resolves
// beats it. Ordered pairs rather than one case each, because the property is
// the ORDER and a per-source test would pass against any permutation.
func TestResolveOrdersTheSourcesAsClaudeCodeDoes(t *testing.T) {
	const env = "sk-ant-api03-FROM-THE-ENVIRONMENT"
	base := identity.APIKeyEnvironment{
		Interactive: true,
		ManagedKey:  "sk-ant-api03-STORED",
	}

	if _, source := base.Resolve(); source != identity.APIKeyManaged {
		t.Fatalf("with only a stored key, source = %v, want APIKeyManaged", source)
	}

	helper := base
	helper.Helper = true
	if _, source := helper.Resolve(); source != identity.APIKeyHelper {
		t.Errorf("apiKeyHelper did not beat the stored key: source = %v", source)
	}

	fd := helper
	fd.FileDescriptorKey = true
	if _, source := fd.Resolve(); source != identity.APIKeyFileDescriptor {
		t.Errorf("the file descriptor did not beat apiKeyHelper: source = %v", source)
	}

	envWins := fd
	envWins.EnvKey = env
	envWins.Approved = []string{identity.APIKeyApproval(env)}
	if key, source := envWins.Resolve(); source != identity.APIKeyEnv || key != env {
		t.Errorf("ANTHROPIC_API_KEY did not beat the file descriptor: %q from %v", key, source)
	}

	// Bare mode reads the environment and the helper and nothing else -- a
	// stored key is not consulted at all.
	bare := base
	bare.Bare = true
	if key, source := bare.Resolve(); source != identity.APIKeyNone || key != "" {
		t.Errorf("bare mode used the stored key: %q from %v", key, source)
	}
}

// Only the three sources Claude Code's BE() names turn the OAuth path off. A
// stored key does not, which is the rule the whole activation design rests on.
func TestOnlyEnvironmentSourcesDisplaceOAuth(t *testing.T) {
	for source, want := range map[identity.APIKeySource]bool{
		identity.APIKeyNone:           false,
		identity.APIKeyEnv:            true,
		identity.APIKeyFileDescriptor: true,
		identity.APIKeyHelper:         true,
		identity.APIKeyManaged:        false,
	} {
		if got := source.DisplacesOAuth(); got != want {
			t.Errorf("%v.DisplacesOAuth() = %v, want %v", source, got, want)
		}
	}
}
