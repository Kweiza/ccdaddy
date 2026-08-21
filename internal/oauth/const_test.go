package oauth

import (
	"slices"
	"strings"
	"testing"
)

// Every literal below was read out of the Claude Code 2.1.238 binary and is
// repeated here on purpose. Asserting against the constant itself would make
// this test tautological: a typo in const.go would edit both sides at once.
func TestConstantsMatchClaudeCode(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"ClientID", ClientID, "9d1c250a-e61b-44d9-88ed-5944d1962f5e"},
		{"ClaudeAIAuthorizeURL", ClaudeAIAuthorizeURL, "https://claude.com/cai/oauth/authorize"},
		{"ConsoleAuthorizeURL", ConsoleAuthorizeURL, "https://platform.claude.com/oauth/authorize"},
		{"TokenURL", TokenURL, "https://platform.claude.com/v1/oauth/token"},
		{"ManualRedirectURL", ManualRedirectURL, "https://platform.claude.com/oauth/code/callback"},
		{"APIBaseURL", APIBaseURL, "https://api.anthropic.com"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// Same reasoning, for the scope set: written out again rather than derived from
// Scopes, so a reorder, a drop, or a typo cannot edit both sides at once.
func TestScopesMatchClaudeCodeExactlyAndInOrder(t *testing.T) {
	want := []string{
		"org:create_api_key",
		"user:profile",
		"user:inference",
		"user:sessions:claude_code",
		"user:mcp_servers",
		"user:file_upload",
	}
	if !slices.Equal(Scopes, want) {
		t.Fatalf("Scopes = %q, want %q", Scopes, want)
	}
	if got := strings.Join(want, " "); ScopeString != got {
		t.Fatalf("ScopeString = %q, want %q", ScopeString, got)
	}
}

// The refresh grant's scope set is NOT the authorize set: Claude Code drops
// org:create_api_key there. Written out again for the same reason as above.
func TestRefreshScopesMatchClaudeCodeExactlyAndInOrder(t *testing.T) {
	want := []string{
		"user:profile",
		"user:inference",
		"user:sessions:claude_code",
		"user:mcp_servers",
		"user:file_upload",
	}
	if !slices.Equal(RefreshScopes, want) {
		t.Fatalf("RefreshScopes = %q, want %q", RefreshScopes, want)
	}
	if got := strings.Join(want, " "); RefreshScopeString != got {
		t.Fatalf("RefreshScopeString = %q, want %q", RefreshScopeString, got)
	}
	if slices.Contains(RefreshScopes, "org:create_api_key") {
		t.Fatal("RefreshScopes carries org:create_api_key; Claude Code's refresh does not request it")
	}
}
