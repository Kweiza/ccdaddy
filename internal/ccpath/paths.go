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
//
// # Claude Code has TWO home directories, and this package answers both
//
// Every rule above hangs off one home, and Claude Code's XDG-style LAYOUT --
// where the native launcher and the installed binaries live -- hangs off a
// DIFFERENT one. Measured out of 2.1.241, where the config root is Hn() rather
// than the An() the 2.1.238 rules above name -- minified symbols are renamed
// between builds, and Hn is a `var` assignment rather than a function
// declaration, so it is found with `grep -abFo 'Hn=pp(' cc.js` and NOT with the
// `function <name>(` recipe in internal/ccver's header:
//
//	Hn()   = (CLAUDE_CONFIG_DIR ?? join(os.homedir(), ".claude")).normalize("NFC")
//	lAi()  = { home: env.HOME ?? os.homedir() }
//	fpr()  = env.XDG_DATA_HOME ?? join(lAi().home, ".local", "share")
//	q1e()  = join(lAi().home, ".local", "bin")
//
// os.homedir() on one side, HOME-then-os.homedir() on the other. On Unix the two
// are one value. On WINDOWS they part whenever HOME is set -- which a
// Git-for-Windows shell does by default -- and then .credentials.json is under
// %USERPROFILE% while claude.exe is under $HOME. homeDir answers the first and
// LayoutHome the second, and swapping them points a switch at the wrong
// credentials file in one direction and loses a native install in the other.
//
// NOT that a native install is only ever under the layout home. Claude Code
// INSTALLS under it and SEARCHES under os.homedir(), so both directories can
// hold one; internal/ccver.layoutHomes carries that measurement and searches
// both. This package's job is to spell each home correctly, not to choose.
//
// Every resolver returns an error rather than a best-effort string. The
// home-directory lookup is the only step that can fail, and it fails in exactly
// one situation -- $HOME (or %USERPROFILE%) is not set -- but the consequence
// of papering over it is severe: a "" home makes ConfigHome ".claude" and
// StoreHome ".ccdad", both RELATIVE, so ccdad would read and write credentials
// under whatever directory it happened to be started in, silently, while
// reporting success. An error at the resolver is the only place that fact can
// still be attached to the variable the operator has to set.
package ccpath

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/text/unicode/norm"
)

// CredentialsFile is the basename of the credential store inside CredentialHome.
const CredentialsFile = ".credentials.json"

// GlobalConfigFile is the basename of Claude Code's global config at the home
// root. The legacy in-config-home form is named by legacyGlobalConfigFile.
const GlobalConfigFile = ".claude.json"

const legacyGlobalConfigFile = ".config.json"

// homeDir returns the home directory Claude Code hangs its CONFIG paths off:
// Hn() joins os.homedir(), and os.UserHomeDir consults $HOME on Unix and
// %USERPROFILE% on Windows exactly as Node's os.homedir() does, so the two agree
// on every platform.
//
// It is NOT the home the launcher and versions directories hang off -- that one
// reads HOME first and is LayoutHome. The header says why the difference is not
// a detail.
//
// os.UserHomeDir never returns an empty path with a nil error -- it either
// finds a non-empty value in the environment, answers "/sdcard" on Android, or
// errors -- so there is no separate emptiness check here to write.
func homeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ccdad cannot tell where your home directory is (%w); "+
			"set HOME (or USERPROFILE on Windows), or point CCDAD_HOME and CLAUDE_CONFIG_DIR at real directories", err)
	}
	return h, nil
}

// Home is the user's home directory, for the callers that need the directory
// itself rather than a path derived from it -- `ccdad setup-path` writing a
// shell startup file, `ccdad uninstall` removing one, and internal/ccver
// spelling the config-side half of where a Claude Code install can be. It is the
// same resolution and the same error every other path in this package is built
// on, exported so those callers cannot grow a second, differently-sandboxed
// answer.
func Home() (string, error) { return homeDir() }

// LayoutHome is the home Claude Code's XDG-STYLE LAYOUT hangs off -- the native
// launcher at <home>/.local/bin and, through XDG_DATA_HOME, the installed
// binaries under <home>/.local/share/claude/versions. It is a SECOND resolution
// and not a variant of the first: Claude Code computes it as
//
//	lAi() = { home: env.HOME ?? os.homedir() }
//
// HOME first, on every platform, while Hn() joins a plain os.homedir(). On Unix
// this returns what homeDir returns, because Go's os.UserHomeDir IS $HOME there.
// On Windows it returns $HOME where homeDir returns %USERPROFILE%.
//
// Only internal/ccver calls this, and that is the point rather than an accident:
// resolving the credential paths through it would move which .credentials.json
// `ccdad switch` writes on a Git-for-Windows shell. It does NOT replace homeDir
// for the launcher search -- ccver searches both homes, because Claude Code
// installs under this one and looks for an existing install under the other.
//
// ONE DELIBERATE DIVERGENCE, and it is a divergence from a bug. JavaScript's ??
// falls through on null and undefined but NOT on "", so Claude Code started with
// HOME set to the empty string computes a RELATIVE .local/bin. This package's
// rule is that a home it cannot spell is an error and never a relative path --
// see the header -- so an empty HOME falls through to homeDir here, which either
// finds %USERPROFILE% or says which variable to set.
func LayoutHome() (string, error) {
	if v := os.Getenv("HOME"); v != "" {
		return v, nil
	}
	return homeDir()
}

// nfc normalizes to NFC, as Claude Code does to every path it derives. This
// matters on macOS, where the filesystem hands back decomposed forms.
func nfc(s string) string { return norm.NFC.String(s) }

// ConfigHome is Claude Code's general config root.
func ConfigHome() (string, error) {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return nfc(v), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return nfc(filepath.Join(home, ".claude")), nil
}

// CredentialHome is the directory holding .credentials.json and the credential
// locks. CLAUDE_SECURESTORAGE_CONFIG_DIR scopes credentials independently of
// CLAUDE_CONFIG_DIR, which is what makes it a usable mechanism for running one
// account in a single terminal.
//
// Claude Code tests whether the variable is DEFINED, not whether it is
// non-empty: defined-but-empty falls back to ~/.claude, NOT to ConfigHome.
func CredentialHome() (string, error) {
	if v, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		if v == "" {
			home, err := homeDir()
			if err != nil {
				return "", err
			}
			return nfc(filepath.Join(home, ".claude")), nil
		}
		return nfc(v), nil
	}
	return ConfigHome()
}

// CredentialsPath is the full path to Claude Code's credential store.
func CredentialsPath() (string, error) {
	home, err := CredentialHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, CredentialsFile), nil
}

// GlobalConfigPath is Claude Code's global config file -- the one holding
// primaryApiKey and customApiKeyResponses. The legacy location inside the
// config home wins when it exists.
//
// The legacy probe's Stat error is deliberately not surfaced: "it is not there"
// and "it could not be statted" both mean the same thing to this function,
// which is that the modern path is the answer.
func GlobalConfigPath() (string, error) {
	configHome, err := ConfigHome()
	if err != nil {
		return "", err
	}
	legacy := filepath.Join(configHome, legacyGlobalConfigFile)
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	// This re-reads CLAUDE_CONFIG_DIR instead of reusing configHome, and must
	// keep doing so: ConfigHome falls back to <home>/.claude, while Claude Code
	// computes this file as join(CLAUDE_CONFIG_DIR || homedir(), ".claude.json")
	// -- the bare home, with no .claude segment. Sharing the value would move
	// ~/.claude.json to ~/.claude/.claude.json and stop finding the real one.
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		if base, err = homeDir(); err != nil {
			return "", err
		}
	}
	return nfc(filepath.Join(base, GlobalConfigFile)), nil
}

// GlobalConfigPathIn is GlobalConfigPath for a config home other than the one
// this process resolves -- the `ccdad run --full-profile` profile, which is a
// whole CLAUDE_CONFIG_DIR that only the child will ever have set.
//
// It cannot be folded into GlobalConfigPath, and the reason is the .claude.json
// asymmetry that function works around: with CLAUDE_CONFIG_DIR UNSET the two
// halves of that rule read different directories -- the legacy file at
// <home>/.claude/.config.json and the modern one at <home>/.claude.json -- so
// there is no single "config home" to pass. With the variable SET they collapse
// to one, which is exactly the case this function is for, and
// TestGlobalConfigPathInAgreesWithTheAmbientRule pins that they agree there.
//
// The legacy probe's Stat error is dropped for the same reason as above: "not
// there" and "could not be statted" both mean the modern path is the answer.
func GlobalConfigPathIn(configHome string) string {
	legacy := filepath.Join(configHome, legacyGlobalConfigFile)
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return nfc(filepath.Join(configHome, GlobalConfigFile))
}

// StoreHome is ccdad's own state directory. CCDAD_HOME overrides it, which is
// what the test suite uses to stay out of the real store.
func StoreHome() (string, error) {
	if v := os.Getenv("CCDAD_HOME"); v != "" {
		return nfc(v), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return nfc(filepath.Join(home, ".ccdad")), nil
}
