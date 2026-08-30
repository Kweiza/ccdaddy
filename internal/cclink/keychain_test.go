package cclink

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Every expected service name below is a LITERAL, hash included. Recomputing
// the digest inside the test would make it pass against any implementation that
// hashed the same string it was given, which is precisely the mistake this
// derivation exists to avoid: clauth hashes a canonicalized path and its own
// tests agree with it perfectly.
//
// The digests were produced independently of this package:
//
//	sha256("/home/tester/.claude")[0:8]     = b2e2cf9d
//	sha256("/home/tester/.claude/")[0:8]    = 6211cf53
//	sha256("/home/tester/./.claude")[0:8]   = f1df102a
//	sha256("/tmp/cc")[0:8]                  = aa3d8c96
//	sha256("/tmp/secure")[0:8]              = 7f494bd1   <- must never appear
//	sha256(NFC("/tmp/café"))[0:8]           = 0873cca0
//	sha256(NFD("/tmp/café"))[0:8]           = 16eb4464   <- 1.0.30-2.1.37's spelling

// nfdCafe is "/tmp/café" with the accent as a combining mark: LATIN SMALL
// LETTER E (U+0065) then COMBINING ACUTE ACCENT (U+0301). Spelled with explicit
// escapes, not a literal accented character, so no editor or VCS filter can
// silently re-normalize the fixture into the form it exists to be different
// from.
const nfdCafe = "/tmp/caf\u0065\u0301"

// nfcCafe is the same path with U+00E9, the single composed code point.
const nfcCafe = "/tmp/caf\u00e9"

func TestKeychainServiceName(t *testing.T) {
	tests := []struct {
		name string
		env  keychainEnv
		want string
	}{
		{
			// The machine almost everyone is on: nothing set, no suffix.
			name: "nothing set",
			env:  keychainEnv{},
			want: "Claude Code-credentials",
		},
		{
			// This value IS the default config home, and it still produces a
			// suffixed item, because the value is hashed rather than compared
			// against anything.
			name: "CLAUDE_CONFIG_DIR set to the literal default still suffixes",
			env:  keychainEnv{configDir: "/home/tester/.claude"},
			want: "Claude Code-credentials-b2e2cf9d",
		},
		{
			// The hashed string is the raw value, in the two shapes a path
			// resolver would erase: a trailing slash and a "./" segment name
			// the same directory and hash differently, so anything that cleans,
			// resolves or realpaths the value first lands on the wrong item.
			name: "a trailing slash is part of the hashed string",
			env:  keychainEnv{configDir: "/home/tester/.claude/"},
			want: "Claude Code-credentials-6211cf53",
		},
		{
			name: "a dot segment is part of the hashed string",
			env:  keychainEnv{configDir: "/home/tester/./.claude"},
			want: "Claude Code-credentials-f1df102a",
		},
		{
			// Raw, but NFC-normalized: the one transformation Claude Code does
			// apply. A macOS filesystem hands back decomposed names, so this is
			// not hypothetical there.
			name: "a decomposed value hashes as its composed form",
			env:  keychainEnv{configDir: nfdCafe},
			want: "Claude Code-credentials-0873cca0",
		},
		{
			name: "the composed form hashes to the same digest",
			env:  keychainEnv{configDir: nfcCafe},
			want: "Claude Code-credentials-0873cca0",
		},
		{
			// The test is `!process.env.CLAUDE_CONFIG_DIR`, so a variable set
			// to the empty string is indistinguishable from an unset one. This
			// is the case where "definedness" and "truthiness" part company,
			// and the value a test isolate sets.
			name: "CLAUDE_CONFIG_DIR set to empty behaves as unset",
			env:  keychainEnv{configDir: ""},
			want: "Claude Code-credentials",
		},
		{
			// CLAUDE_CONFIG_DIR is the only directory this derivation reads.
			// The struct has no securestorage field to set, which is the point:
			// no release that ever wrote one of these items read that variable,
			// so there is nothing here for it to outrank.
			name: "CLAUDE_CONFIG_DIR decides the hash",
			env:  keychainEnv{configDir: "/tmp/cc"},
			want: "Claude Code-credentials-aa3d8c96",
		},
		{
			// OAUTH_FILE_SUFFIX is not always empty, and it lands BEFORE the
			// item name rather than at the end.
			name: "a custom OAuth URL stamps its suffix ahead of the item name",
			env:  keychainEnv{customOAuthURL: "https://example.invalid"},
			want: "Claude Code-custom-oauth-credentials",
		},
		{
			// All three parts at once, which is the only case that pins their
			// ORDER: base, oauth suffix, item, hash.
			name: "the oauth suffix, the item and the hash keep their order",
			env: keychainEnv{
				configDir:      "/tmp/cc",
				customOAuthURL: "https://example.invalid",
			},
			want: "Claude Code-custom-oauth-credentials-aa3d8c96",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keychainServiceName(tc.env, keychainCredentialsItem); got != tc.want {
				t.Fatalf("keychainServiceName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The PRIMARY name must be the composed one. Spelled as its own test because
// the table above asserts what the answer IS, and this asserts which of the two
// real spellings comes first -- 2.1.38 through 2.1.112 is the later and likelier
// half of the era, so its name is the one a single-probe caller gets.
func TestKeychainServiceNamePrefersTheComposedSpelling(t *testing.T) {
	got := keychainServiceName(keychainEnv{configDir: nfdCafe}, keychainCredentialsItem)
	if got == "Claude Code-credentials-16eb4464" {
		t.Fatalf("the primary name is the raw-bytes one: %q", got)
	}
	if want := keychainServiceName(keychainEnv{configDir: nfcCafe}, keychainCredentialsItem); got != want {
		t.Fatalf("the decomposed and composed forms disagree: %q vs %q", got, want)
	}
}

// Both spellings, because Claude Code changed its own mind mid-era: 1.0.30
// through 2.1.37 hashed CLAUDE_CONFIG_DIR as it came and 2.1.38 started
// normalizing to NFC first. A decomposed value has an item under EACH digest
// depending on which build wrote it, so naming only one is a diagnostic that
// answers "nothing there" about a name it never looked for -- the same class of
// defect as deriving from CLAUDE_SECURESTORAGE_CONFIG_DIR.
//
// Both digests are literals produced outside this package (see the header), so
// a derivation that hashed whatever it was handed cannot satisfy this.
func TestKeychainServiceNamesCoversBothNormalizations(t *testing.T) {
	tests := []struct {
		name string
		env  keychainEnv
		want []string
	}{
		{
			// The ordinary machine: one name, so one spawn.
			name: "nothing set is a single candidate",
			env:  keychainEnv{},
			want: []string{"Claude Code-credentials"},
		},
		{
			// An ASCII path is already composed, so normalizing changes
			// nothing and the second lookup would be the same lookup.
			name: "an already-composed value is a single candidate",
			env:  keychainEnv{configDir: "/tmp/cc"},
			want: []string{"Claude Code-credentials-aa3d8c96"},
		},
		{
			name: "a decomposed value yields the composed name first, then the raw one",
			env:  keychainEnv{configDir: nfdCafe},
			want: []string{"Claude Code-credentials-0873cca0", "Claude Code-credentials-16eb4464"},
		},
		{
			// Handed the composed form there is nothing to split: 1.0.30-2.1.37
			// and 2.1.38+ hashed the same bytes.
			name: "the composed value is a single candidate",
			env:  keychainEnv{configDir: nfcCafe},
			want: []string{"Claude Code-credentials-0873cca0"},
		},
		{
			// The split survives the other two parts of the name, which is what
			// pins that the eras differ ONLY in what gets hashed.
			name: "the oauth suffix is carried onto both spellings",
			env:  keychainEnv{configDir: nfdCafe, customOAuthURL: "https://example.invalid"},
			want: []string{
				"Claude Code-custom-oauth-credentials-0873cca0",
				"Claude Code-custom-oauth-credentials-16eb4464",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keychainServiceNames(tc.env, keychainCredentialsItem)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keychainServiceNames() = %q, want %q", got, tc.want)
			}
		})
	}
}

// qpt with no item argument names the managed-API-key item. ccdad does not
// probe for it, but the derivation has to be able to spell it, and this pins
// that the item name is a parameter rather than baked into the format string.
func TestKeychainServiceNameWithoutAnItemIsTheBareService(t *testing.T) {
	if got := keychainServiceName(keychainEnv{}, ""); got != "Claude Code" {
		t.Fatalf("keychainServiceName(env, \"\") = %q, want %q", got, "Claude Code")
	}
}

func TestKeychainAccountName(t *testing.T) {
	failing := func() (string, error) { return "", errors.New("no such user") }
	answers := func(name string) func() (string, error) {
		return func() (string, error) { return name, nil }
	}

	tests := []struct {
		name  string
		user  string
		osUsr func() (string, error)
		want  string
	}{
		{
			name: "$USER wins", user: "alice", osUsr: answers("bob"), want: "alice",
		},
		{
			// The OS is consulted only when $USER is empty -- and it has to be
			// consulted, because a launchd or cron session has no $USER and
			// Claude Code would still find the real name there.
			name: "the operating system answers when $USER is empty",
			user: "", osUsr: answers("bob"), want: "bob",
		},
		{
			name: "an OS lookup that fails falls back",
			user: "", osUsr: failing, want: "claude-code-user",
		},
		{
			// An OS that answers with nothing is the catch too: `security -a ""`
			// is not a lookup, and this is the branch a pattern check used to
			// cover by accident.
			name: "an OS name that is empty falls back",
			user: "", osUsr: answers(""), want: "claude-code-user",
		},
		{
			// NOT rewritten to the constant, and this is the correction. The
			// releases that wrote these items ran `process.env.USER ||
			// os.userInfo().username` through no pattern at all, so a name with
			// a space in it is the name on the item. Only the dead builder in
			// today's binary validates, and it has never written one.
			name: "a $USER with a space is the account, not the fallback",
			user: "al ice", osUsr: answers("bob"), want: "al ice",
		},
		{
			// The same for a name the OS supplied, which is the case that proves
			// the rule is about the SOURCE order and not about the characters.
			name: "an OS name with a space is used as it is",
			user: "", osUsr: answers("Bad Name"), want: "Bad Name",
		},
		{
			name: "a non-ASCII name is used as it is",
			user: "유저", osUsr: answers("bob"), want: "유저",
		},
		{
			name: "dots, underscores and hyphens are ordinary",
			user: "a.b_c-d1", osUsr: failing, want: "a.b_c-d1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := keychainAccountName(keychainEnv{user: tc.user}, tc.osUsr)
			if got != tc.want {
				t.Fatalf("keychainAccountName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The end-to-end derivation, through the real environment rather than a
// hand-built struct, because reading the wrong variable is a defect the pure
// table cannot see.
func TestCredentialKeychainItemReadsTheEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/cc")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/tmp/secure")
	t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
	t.Setenv("USER", "tester")

	got := CredentialKeychainItem()
	if want := "Claude Code-credentials-aa3d8c96"; got.Service != want {
		t.Fatalf("Service = %q, want %q", got.Service, want)
	}
	// The named negative. /tmp/secure's digest is what the dead 2.1.113+ builder
	// would produce here, and it names an item no Claude Code has ever written:
	// CLAUDE_SECURESTORAGE_CONFIG_DIR does not occur once in 2.1.112, the last
	// release that read the keychain at all. `ccdad run` sets this variable, so
	// this is the exact shape of the false "no legacy item" it used to cause.
	if strings.Contains(got.Service, "7f494bd1") {
		t.Fatalf("Service = %q: derived from CLAUDE_SECURESTORAGE_CONFIG_DIR", got.Service)
	}
	if got.Account != "tester" {
		t.Fatalf("Account = %q, want %q", got.Account, "tester")
	}
}

// The securestorage variable set ALONE must not suffix anything: with
// CLAUDE_CONFIG_DIR absent the item is the unsuffixed one, which is what a
// <=2.1.112 Claude Code launched inside a default `ccdad run` session reads.
func TestCredentialKeychainItemIgnoresTheSecureStorageVariable(t *testing.T) {
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/tmp/secure")
	t.Setenv("USER", "tester")
	unsetEnv(t, "CLAUDE_CONFIG_DIR")
	unsetEnv(t, "CLAUDE_CODE_CUSTOM_OAUTH_URL")

	if got := CredentialKeychainItem().Service; got != "Claude Code-credentials" {
		t.Fatalf("Service = %q, want %q", got, "Claude Code-credentials")
	}
}

// The same trap as the table's second case, but reached the way a user reaches
// it: CCDAD's own isolate helpers set CLAUDE_CONFIG_DIR to a real directory, and
// pointing it at the default one is the mistake -- the item is suffixed
// anyway.
func TestCredentialKeychainItemSuffixesEvenAtTheDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("USER", "tester")
	unsetEnv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")

	got := CredentialKeychainItem().Service
	if got == "Claude Code-credentials" {
		t.Fatalf("Service = %q, want a suffixed item", got)
	}
}

// unsetEnv removes a variable for the duration of the test. t.Setenv cannot
// unset, and the difference is load-bearing here: this variable set to the
// empty string is a DIFFERENT case from absent, and a test that could only
// reach the empty one would never exercise the branch it is about.
//
// The t.Setenv first is what registers the restore, so the original value comes
// back at cleanup whether it was set, empty or absent.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetting %s: %v", name, err)
	}
}

func TestClassifyKeychainError(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   keychainFailure
	}{
		{"nothing said", "", failSaidNothing},
		{"unrecognised", "security: something new happened", failOther},
		{
			"item not found",
			"security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.",
			failItemNotFound,
		},
		{"duplicate", "security: SecKeychainItemCreateFromContent: errSecDuplicateItem", failDuplicateItem},
		{"unopenable", "security: Unable to open /Users/t/Library/Keychains/login.keychain-db", failUnavailable},
		{"no default keychain", "security: SecKeychainCopyDefault: A default keychain could not be found.", failNoKeychain},
		{
			"interaction refused",
			"security: SecKeychainItemCopyContent: User interaction is not allowed.",
			failNoInteraction,
		},
		{"cancelled", "security: SecKeychainItemCopyContent: User canceled the operation.", failUserCanceled},
		{
			"bad passphrase",
			"security: SecKeychainUnlock: The user name or passphrase you entered is not correct.",
			failAuthFailed,
		},
		{"locked", "security: SecKeychainItemCopyContent: The specified keychain is locked.", failLocked},

		// The order cases. Every one of these matches two branches, and the
		// answer is decided by which branch Claude Code asks about first. A
		// classifier written with the same set of tests in a different order
		// passes every case above and fails all three of these.
		{
			"cancelled beats locked",
			"security: User canceled the unlock of the locked keychain.",
			failUserCanceled,
		},
		{
			"authorization beats locked",
			"security: authorization denied for the locked keychain.",
			failAuthFailed,
		},
		{
			"duplicate beats everything",
			"security: the item already exists in the locked keychain.",
			failDuplicateItem,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyKeychainError(tc.stderr); got != tc.want {
				t.Fatalf("classifyKeychainError(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

// Two failures reported with the same sentence are two failures the classifier
// never had to tell apart: the whole point of classifying is that a locked
// keychain and a missing one want opposite responses from the user. This is the
// assertion that stops a branch from being collapsed into its neighbour.
func TestKeychainFailureDetailsAreDistinct(t *testing.T) {
	all := []keychainFailure{
		failDuplicateItem, failUnavailable, failNoKeychain, failItemNotFound,
		failNoInteraction, failUserCanceled, failAuthFailed, failLocked,
		failSaidNothing, failOther, failTimedOut, failLingering, failSecurityMissing,
	}
	seen := make(map[string]keychainFailure, len(all))
	for _, f := range all {
		detail := keychainFailureDetail(f)
		if detail == "" {
			t.Errorf("%q has no detail", f)
			continue
		}
		if prior, dup := seen[detail]; dup {
			t.Errorf("%q and %q share a detail: %q", prior, f, detail)
			continue
		}
		seen[detail] = f
	}
}

func TestTrimKeychainSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"{\"a\":1}\n", "{\"a\":1}"},
		{"{\"a\":1}", "{\"a\":1}"},
		// Exactly one newline, not all of them: a second one belonged to the
		// value, and this function is not entitled to reshape a document
		// somebody else wrote.
		{"{\"a\":1}\n\n", "{\"a\":1}\n"},
		{"  padded  ", "  padded  "},
		{"", ""},
	}
	for _, tc := range tests {
		if got := trimKeychainSecret(tc.in); got != tc.want {
			t.Fatalf("trimKeychainSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
