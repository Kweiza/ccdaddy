package ccpath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")

	got, err := ConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".claude"); got != want {
		t.Fatalf("ConfigHome() = %q, want %q", got, want)
	}
}

func TestConfigHomeRespectsEnv(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/cc")

	got, err := ConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/custom/cc"; got != want {
		t.Fatalf("ConfigHome() = %q, want %q", got, want)
	}
}

// CLAUDE_SECURESTORAGE_CONFIG_DIR scopes credentials only. Claude Code checks
// whether it is DEFINED, not whether it is non-empty: a defined-but-empty value
// falls back to ~/.claude rather than to CLAUDE_CONFIG_DIR.
func TestCredentialHomeScopesIndependently(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/cc")

	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/custom/creds")
	got, err := CredentialHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/custom/creds"; got != want {
		t.Fatalf("CredentialHome() with value = %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	got, err = CredentialHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".claude"); got != want {
		t.Fatalf("CredentialHome() with empty value = %q, want %q", got, want)
	}

	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	got, err = CredentialHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/custom/cc"; got != want {
		t.Fatalf("CredentialHome() unset = %q, want %q", got, want)
	}
}

func TestCredentialsPath(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")

	want := filepath.Join(home, ".claude", ".credentials.json")
	got, err := CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("CredentialsPath() = %q, want %q", got, want)
	}
}

// The global config sits at the HOME root by default, not inside .claude/.
func TestGlobalConfigPathDefault(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	want := filepath.Join(home, ".claude.json")
	got, err := GlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("GlobalConfigPath() = %q, want %q", got, want)
	}
}

func TestGlobalConfigPathPrefersLegacyWhenPresent(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	legacyDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, ".config.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := GlobalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy {
		t.Fatalf("GlobalConfigPath() = %q, want %q", got, legacy)
	}
}

func TestStoreHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CCDAD_HOME", "")

	got, err := StoreHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".ccdad"); got != want {
		t.Fatalf("StoreHome() = %q, want %q", got, want)
	}

	t.Setenv("CCDAD_HOME", "/opt/ccdad")
	got, err = StoreHome()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/opt/ccdad"; got != want {
		t.Fatalf("StoreHome() override = %q, want %q", got, want)
	}
}

// Claude Code NFC-normalizes every derived path. macOS hands back decomposed
// Unicode for accented directory names; without normalization, ccpath would
// resolve to a different path than Claude Code did when it created the file.
func TestConfigHomeNormalizesToNFC(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/"+decomposedCafe)

	want := "/custom/" + composedCafe
	got, err := ConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ConfigHome() = %q, want %q (NFC-normalized)", got, want)
	}
}

// CredentialHome's explicit-value branch NFC-normalizes independently of
// ConfigHome's -- it is a separate call site that later tasks derive the
// macOS Keychain service-name hash and legacy lock path from, so it needs its
// own coverage, not just ConfigHome's.
func TestCredentialHomeNormalizesToNFC(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "/should-not-be-used")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/creds/"+decomposedCafe)

	want := "/creds/" + composedCafe
	got, err := CredentialHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
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

// setHome points os.UserHomeDir at dir on every platform. It reads $HOME on
// Unix and %USERPROFILE% on Windows, so a test that sets only HOME sandboxes
// nothing on the Windows CI leg: every resolver below falls through to the
// runner's real profile and the comparison against the temp directory fails.
// Setting the variable that does not apply is harmless; not setting it is not.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// clearHome unsets the variable os.UserHomeDir consults on this platform, which
// is the only way to make the lookup fail. It is written per-platform rather
// than clearing both, so a future change to os.UserHomeDir's variable is a
// compile-visible edit here rather than a test that silently stops failing.
func clearHome(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
		return
	}
	t.Setenv("HOME", "")
}

// isolateEnv clears every variable the resolvers consult, so a test starts from
// "nothing is set" rather than from whatever the developer's shell exports.
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CCDAD_HOME", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
}

// resolvers is every exported path function, named, so the no-home test covers
// the whole surface instead of a sample of it -- a new resolver that forgets to
// propagate the error is the regression this is here to catch, and it is only
// caught if adding one to the package means adding it here too.
func resolvers() []struct {
	name string
	fn   func() (string, error)
} {
	return []struct {
		name string
		fn   func() (string, error)
	}{
		{"ConfigHome", ConfigHome},
		{"CredentialHome", CredentialHome},
		{"CredentialsPath", CredentialsPath},
		{"GlobalConfigPath", GlobalConfigPath},
		{"StoreHome", StoreHome},
	}
}

// With no home and no overrides, every resolver must fail -- and must return
// "" alongside the error.
//
// The empty string is half the point and is asserted separately. The bug this
// replaced was not that the old code returned a wrong error; it returned no
// error at all and a RELATIVE path (".claude", ".ccdad"), so ccdad read and
// wrote credentials under the process's working directory. A resolver that
// returned an error but still handed back that relative path would leave a
// caller who ignores the error in exactly the old situation.
func TestEveryResolverFailsWhenTheHomeIsUnknown(t *testing.T) {
	isolateEnv(t)
	clearHome(t)

	for _, r := range resolvers() {
		got, err := r.fn()
		if err == nil {
			t.Errorf("%s() = %q, nil; want an error when the home directory cannot be resolved", r.name, got)
			continue
		}
		if got != "" {
			t.Errorf("%s() returned %q alongside its error; a caller that drops the error would use a relative path", r.name, got)
		}
	}
}

// The error has to name what to set, because the operator's only fix is an
// environment variable and nothing else in the process knows which one.
func TestNoHomeErrorNamesTheVariableToSet(t *testing.T) {
	isolateEnv(t)
	clearHome(t)

	_, err := StoreHome()
	if err == nil {
		t.Fatal("StoreHome() succeeded with no home directory")
	}
	for _, want := range []string{"HOME", "CCDAD_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("StoreHome() error = %q, want it to name %s", err, want)
		}
	}
}

// The home lookup is consulted lazily: a fully overridden environment resolves
// every path without one. This is what makes the error above a genuine
// last-resort rather than a new hard requirement, and it is the configuration
// the test suite itself runs under.
func TestResolversDoNotNeedAHomeWhenTheEnvironmentSuppliesOne(t *testing.T) {
	clearHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join("/custom", "cc"))
	t.Setenv("CCDAD_HOME", filepath.Join("/custom", "ccdad"))
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")

	for _, r := range resolvers() {
		if _, err := r.fn(); err != nil {
			t.Errorf("%s() = %v; want it to resolve from the environment without a home directory", r.name, err)
		}
	}
}
