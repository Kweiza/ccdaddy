package ccpath

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// decomposedCafe is "caf\u00e9" in NFD form: LATIN SMALL LETTER E (U+0065) followed
// by COMBINING ACUTE ACCENT (U+0301) -- the decomposed form macOS's filesystem
// hands back for accented names. Spelled with explicit \u escapes, not a
// literal accented character, so the decomposition survives any editor or VCS
// filter that re-normalizes source files to NFC.
const decomposedCafe = "caf\u0065\u0301"

// composedCafe is the same string in NFC form: LATIN SMALL LETTER E WITH
// ACUTE (U+00E9) as a single code point.
const composedCafe = "caf\u00e9"

func TestConfigHomeDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")

	if got, want := ConfigHome(), filepath.Join(home, ".claude"); got != want {
		t.Fatalf("ConfigHome() = %q, want %q", got, want)
	}
}

func TestConfigHomeRespectsEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/cc")

	if got, want := ConfigHome(), "/custom/cc"; got != want {
		t.Fatalf("ConfigHome() = %q, want %q", got, want)
	}
}

// CLAUDE_SECURESTORAGE_CONFIG_DIR scopes credentials only. Claude Code checks
// whether it is DEFINED, not whether it is non-empty: a defined-but-empty value
// falls back to ~/.claude rather than to CLAUDE_CONFIG_DIR.
func TestCredentialHomeScopesIndependently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/cc")

	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/custom/creds")
	if got, want := CredentialHome(), "/custom/creds"; got != want {
		t.Fatalf("CredentialHome() with value = %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	if got, want := CredentialHome(), filepath.Join(home, ".claude"); got != want {
		t.Fatalf("CredentialHome() with empty value = %q, want %q", got, want)
	}

	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if got, want := CredentialHome(), "/custom/cc"; got != want {
		t.Fatalf("CredentialHome() unset = %q, want %q", got, want)
	}
}

func TestCredentialsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")

	want := filepath.Join(home, ".claude", ".credentials.json")
	if got := CredentialsPath(); got != want {
		t.Fatalf("CredentialsPath() = %q, want %q", got, want)
	}
}

// The global config sits at the HOME root by default, not inside .claude/.
func TestGlobalConfigPathDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	want := filepath.Join(home, ".claude.json")
	if got := GlobalConfigPath(); got != want {
		t.Fatalf("GlobalConfigPath() = %q, want %q", got, want)
	}
}

func TestGlobalConfigPathPrefersLegacyWhenPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	legacyDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, ".config.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := GlobalConfigPath(); got != legacy {
		t.Fatalf("GlobalConfigPath() = %q, want %q", got, legacy)
	}
}

func TestStoreHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCDAD_HOME", "")

	if got, want := StoreHome(), filepath.Join(home, ".ccdad"); got != want {
		t.Fatalf("StoreHome() = %q, want %q", got, want)
	}

	t.Setenv("CCDAD_HOME", "/opt/ccdad")
	if got, want := StoreHome(), "/opt/ccdad"; got != want {
		t.Fatalf("StoreHome() override = %q, want %q", got, want)
	}
}

// Claude Code NFC-normalizes every derived path. macOS hands back decomposed
// Unicode for accented directory names; without normalization, ccpath would
// resolve to a different path than Claude Code did when it created the file.
func TestConfigHomeNormalizesToNFC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/"+decomposedCafe)

	want := "/custom/" + composedCafe
	if got := ConfigHome(); got != want {
		t.Fatalf("ConfigHome() = %q, want %q (NFC-normalized)", got, want)
	}
}

// CredentialHome's explicit-value branch NFC-normalizes independently of
// ConfigHome's -- it is a separate call site that later tasks derive the
// macOS Keychain service-name hash and legacy lock path from, so it needs its
// own coverage, not just ConfigHome's.
func TestCredentialHomeNormalizesToNFC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/should-not-be-used")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/creds/"+decomposedCafe)

	want := "/creds/" + composedCafe
	if got := CredentialHome(); got != want {
		t.Fatalf("CredentialHome() = %q, want %q (NFC-normalized)", got, want)
	}
}

// The two constants above are the only thing that makes the NFC tests
// meaningful, and they are exactly what an editor or a VCS filter can silently
// renormalize on a future save. If that happens they become byte-identical, the
// NFC tests start comparing a string to itself, and they keep passing with no
// signal. This asserts the premise instead of assuming it.
func TestNFCFixturesDifferInBytes(t *testing.T) {
	if decomposedCafe == composedCafe {
		t.Fatalf("NFC test fixtures are byte-identical (%q); something renormalized them, "+
			"and TestConfigHomeNormalizesToNFC / TestCredentialHomeNormalizesToNFC now prove nothing",
			decomposedCafe)
	}
	if norm.NFC.String(decomposedCafe) != composedCafe {
		t.Fatalf("NFC(decomposedCafe) = %q, want composedCafe %q", norm.NFC.String(decomposedCafe), composedCafe)
	}
}
