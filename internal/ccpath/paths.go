// Package ccpath resolves the files Claude Code reads and writes, mirroring
// Claude Code's own resolution so ccdad touches exactly the same paths.
//
// Rules, from Claude Code 2.1.238:
//
//	An()  = CLAUDE_CONFIG_DIR ?? ~/.claude                  (general config root)
//	IY()  = CLAUDE_SECURESTORAGE_CONFIG_DIR ?? An()         (credential root)
//	global config = <An()>/.config.json if it exists,
//	                else (CLAUDE_CONFIG_DIR ?? $HOME)/.claude.json
//
// Note the asymmetry in the last rule: .claude.json sits at the home directory
// by default, not inside .claude/.
package ccpath

import (
	"os"
	"path/filepath"

	"golang.org/x/text/unicode/norm"
)

// CredentialsFile is the basename of the credential store inside CredentialHome.
const CredentialsFile = ".credentials.json"

// homeDir returns the user's home directory. os.UserHomeDir consults $HOME on
// Unix and %USERPROFILE% on Windows, which is what Claude Code's os.homedir()
// does, so the two agree.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// nfc normalizes to NFC, as Claude Code does to every path it derives. This
// matters on macOS, where the filesystem hands back decomposed forms.
func nfc(s string) string { return norm.NFC.String(s) }

// ConfigHome is Claude Code's general config root.
func ConfigHome() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return nfc(v)
	}
	return nfc(filepath.Join(homeDir(), ".claude"))
}

// CredentialHome is the directory holding .credentials.json and the credential
// locks. CLAUDE_SECURESTORAGE_CONFIG_DIR scopes credentials independently of
// CLAUDE_CONFIG_DIR, which is what makes it a usable mechanism for running one
// account in a single terminal.
//
// Claude Code tests whether the variable is DEFINED, not whether it is
// non-empty: defined-but-empty falls back to ~/.claude, NOT to ConfigHome.
func CredentialHome() string {
	if v, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		if v == "" {
			return nfc(filepath.Join(homeDir(), ".claude"))
		}
		return nfc(v)
	}
	return ConfigHome()
}

// CredentialsPath is the full path to Claude Code's credential store.
func CredentialsPath() string {
	return filepath.Join(CredentialHome(), CredentialsFile)
}

// GlobalConfigPath is Claude Code's global config file. The legacy location
// inside the config home wins when it exists.
func GlobalConfigPath() string {
	legacy := filepath.Join(ConfigHome(), ".config.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		base = homeDir()
	}
	return nfc(filepath.Join(base, ".claude.json"))
}

// StoreHome is ccdad's own state directory. CCDAD_HOME overrides it, which is
// what the test suite uses to stay out of the real store.
func StoreHome() string {
	if v := os.Getenv("CCDAD_HOME"); v != "" {
		return nfc(v)
	}
	return nfc(filepath.Join(homeDir(), ".ccdad"))
}
