package ccpath

import (
	"os"
	"path/filepath"
	"testing"
)

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
