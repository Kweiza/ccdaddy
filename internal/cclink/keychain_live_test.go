package cclink

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func hash8(t *testing.T, s string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	return "-" + hex.EncodeToString(sum[:])[:8]
}

// The derivation is a0()'s, and every row here is one of its branches. The
// machine this was written on carries the unsuffixed item with neither variable
// exported, which is the first row.
func TestLiveKeychainItemFollowsTheInstalledBuildsRules(t *testing.T) {
	const base = "Claude Code-credentials"
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"neither variable exported", []string{"USER=kweiza"}, base},
		{
			"a scoped credential root is hashed, and outranks the config dir",
			[]string{"USER=kweiza", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/tmp/sess", "CLAUDE_CONFIG_DIR=/tmp/other"},
			base + hash8(t, "/tmp/sess"),
		},
		{
			// DEFINEDNESS, not truthiness: a0 tests `e!==void 0` and then `!e`,
			// so an exported empty value selects the UNSUFFIXED item even
			// though CLAUDE_CONFIG_DIR is set and non-empty.
			"an exported but empty credential root selects the unsuffixed item",
			[]string{"USER=kweiza", "CLAUDE_SECURESTORAGE_CONFIG_DIR=", "CLAUDE_CONFIG_DIR=/tmp/other"},
			base,
		},
		{
			"the config dir is hashed only when the credential root is unset",
			[]string{"USER=kweiza", "CLAUDE_CONFIG_DIR=/tmp/other"},
			base + hash8(t, "/tmp/other"),
		},
		{
			// Not an equality test against the default path: the value is
			// hashed, so the literal default still produces a suffixed item.
			"the literal default config dir is still hashed",
			[]string{"USER=kweiza", "CLAUDE_CONFIG_DIR=/Users/kweiza/.claude"},
			base + hash8(t, "/Users/kweiza/.claude"),
		},
		{
			"a custom OAuth URL stamps the suffix onto the base",
			[]string{"USER=kweiza", "CLAUDE_CODE_CUSTOM_OAUTH_URL=https://example.test"},
			"Claude Code-custom-oauth-credentials",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LiveKeychainItem(tc.env).Service; got != tc.want {
				t.Fatalf("Service = %q, want %q", got, tc.want)
			}
		})
	}
}

// The value is NFC-normalized before hashing, so a decomposed spelling of the
// same directory names ONE item rather than two.
func TestLiveKeychainItemNormalizesBeforeHashing(t *testing.T) {
	decomposed := "/tmp/café"
	composed := "/tmp/café"
	a := LiveKeychainItem([]string{"USER=u", "CLAUDE_SECURESTORAGE_CONFIG_DIR=" + decomposed}).Service
	b := LiveKeychainItem([]string{"USER=u", "CLAUDE_SECURESTORAGE_CONFIG_DIR=" + composed}).Service
	if a != b {
		t.Fatalf("decomposed named %q and composed named %q; a0 normalizes, so they are one item", a, b)
	}
	if want := "Claude Code-credentials" + hash8(t, composed); a != want {
		t.Fatalf("Service = %q, want the composed hash %q", a, want)
	}
}

// xC() validates the account name and this derivation must too -- unlike the
// legacy one, whose header says why it must NOT.
func TestLiveKeychainItemAccountFollowsTheInstalledBuildsPattern(t *testing.T) {
	for _, tc := range []struct{ user, want string }{
		{"kweiza", "kweiza"},
		{"first.last_1-2", "first.last_1-2"},
		{"has space", keychainFallbackAccount},
		{"ünïcode", keychainFallbackAccount},
	} {
		if got := LiveKeychainItem([]string{"USER=" + tc.user}).Account; got != tc.want {
			t.Fatalf("USER=%q gave account %q, want %q", tc.user, got, tc.want)
		}
	}
}

// The legacy hunt and the live derivation disagree about exactly one variable,
// and this pins the disagreement so neither can be "tidied" into the other.
func TestLegacyHuntIgnoresTheCredentialRootAndTheLiveDerivationDoesNot(t *testing.T) {
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/tmp/sess")
	t.Setenv("USER", "kweiza")
	os := "CLAUDE_SECURESTORAGE_CONFIG_DIR=/tmp/sess"
	if legacy := CredentialKeychainItem().Service; legacy != "Claude Code-credentials" {
		t.Fatalf("the legacy hunt read the credential root: %q", legacy)
	}
	if live := LiveKeychainItem([]string{"USER=kweiza", os}).Service; live == "Claude Code-credentials" {
		t.Fatal("the live derivation ignored the credential root")
	}
}
