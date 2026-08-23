package cclink

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Legacy macOS Keychain support, and it is legacy in the strongest sense: no
// Claude Code this project has ever been able to run reads or writes these
// items.
//
// 2.1.222, 2.1.238 and 2.1.240 all name their secure-storage backend
// "plaintext" and read .credentials.json. The service-name builder below is
// still compiled into 2.1.238 and 2.1.240, byte-for-byte the same logic under
// different minified names -- but in BOTH of them its only call site is inside
// saveApiKey's `let r = !1; if (r) {...}`, which no build can enter, and that
// call passes no item argument, so it would name the API-key item rather than
// this one. The "-credentials" constant ships in both and is referenced by
// nothing at all. ccdad keeps this path for exactly two situations that
// outlive that:
//
//   - a user still on a Keychain-era Claude Code, whose login lives in the
//     Keychain and not in any file ccdad can read;
//   - a stale item left behind on a machine that has since moved to the file.
//     Nothing reads it today, but a DOWNGRADED Claude Code would, and it would
//     shadow whatever ccdad had written -- a switch that appears to work and
//     changes nothing.
//
// WHERE TO RE-CHECK THIS. Do NOT search for the "-credentials" constant: it is
// unreferenced, so its minified name has one hit in a 229 MB binary and the
// obvious conclusion -- that the derivation is gone -- is wrong. Search for the
// FUNCTION instead, by its body rather than its name, which changes every
// release (`qpt` in 2.1.238, `xht` in 2.1.240):
//
//	grep -aoP '.{0,300}CLAUDE_SECURESTORAGE_CONFIG_DIR,r=t.{0,400}'
//
// against the extracted bundle (see claude-code-oauth-ground-truth for the
// dd/tr recipe). What it finds, in both versions:
//
//	function qpt(e=""){
//	  let t=process.env.CLAUDE_SECURESTORAGE_CONFIG_DIR,
//	      r=t!==void 0?!t:!process.env.CLAUDE_CONFIG_DIR,
//	      n=t!==void 0?t.normalize("NFC"):An(),
//	      o=r?"":`-${sha256(n).hex.substring(0,8)}`;
//	  return `Claude Code${al().OAUTH_FILE_SUFFIX}${e}${o}`}
//
// The pure derivation lives in this file and the spawns live in
// keychain_security.go, so everything with a decision in it is reachable from a
// test on a Linux development machine.

const (
	// keychainBaseService is the literal Claude Code prepends to every item
	// name. It is not derived from anything.
	keychainBaseService = "Claude Code"

	// keychainCredentialsItem is qpt's argument for the OAuth credential item
	// -- Claude Code's `$Or`/`x2r` constant. Calling qpt with NO argument
	// yields the managed-API-key item, whose service is therefore the bare
	// "Claude Code"; ccdad does not look for that one, because the live API-key
	// axis is ~/.claude.json's primaryApiKey and globalconfig.go already owns
	// it.
	keychainCredentialsItem = "-credentials"

	// keychainCustomOAuthSuffix is the only value of OAUTH_FILE_SUFFIX a
	// shipped Claude Code can produce besides "". See oauthFileSuffix.
	keychainCustomOAuthSuffix = "-custom-oauth"

	// keychainFallbackAccount is Claude Code's own last resort for the item's
	// account attribute. Matching it exactly matters: on a launchd or cron host
	// with no $USER, an item written under a different fallback is an item
	// ccdad cannot find and cannot delete.
	keychainFallbackAccount = "claude-code-user"
)

// keychainAccountPattern is Claude Code's CVb/qwb, and it is applied to the
// resolved username rather than only to $USER: a name that fails it becomes the
// fallback even when the operating system is the one that supplied it.
var keychainAccountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// KeychainItem names one generic-password item: the two attributes that
// identify it to `security`, and the only two ccdad ever needs to spell.
type KeychainItem struct {
	Service string
	Account string
}

// keychainEnv is the environment the derivation reads, captured as raw values.
//
// RAW is the point. Claude Code hashes the string that was exported -- not a
// cleaned, resolved or symlink-followed form of it -- so "/tmp/cc", "/tmp/cc/"
// and "/tmp/./cc" are three different Keychain items even where they are one
// directory. clauth calls canonicalize() before hashing and is wrong about this;
// cswap hashes the raw value and is right.
type keychainEnv struct {
	// secureStorageDir is CLAUDE_SECURESTORAGE_CONFIG_DIR's value, and
	// secureStorageSet is whether it is present in the environment at all.
	// The two are separate because Claude Code's own test is neither purely
	// definedness nor purely emptiness -- see keychainServiceName.
	secureStorageDir string
	secureStorageSet bool

	// configDir is CLAUDE_CONFIG_DIR's value. Its definedness is never asked.
	configDir string

	// customOAuthURL is CLAUDE_CODE_CUSTOM_OAUTH_URL's value, which is the one
	// input that changes OAUTH_FILE_SUFFIX in a released binary.
	customOAuthURL string

	// user is $USER, consulted before the operating system.
	user string
}

// readKeychainEnv captures the environment once, so a service name and an
// account name derived in the same call cannot straddle a change to it.
func readKeychainEnv() keychainEnv {
	secure, secureSet := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	return keychainEnv{
		secureStorageDir: secure,
		secureStorageSet: secureSet,
		configDir:        os.Getenv("CLAUDE_CONFIG_DIR"),
		customOAuthURL:   os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL"),
		user:             os.Getenv("USER"),
	}
}

// CredentialKeychainItem is the item a Keychain-era Claude Code would keep this
// machine's OAuth login in, for the environment ccdad is running under.
//
// It is a derivation, not a probe: it says what the item WOULD be called, and
// says nothing about whether one exists. Nothing here touches the filesystem or
// spawns anything, so it is safe to call on any platform and in any state.
func CredentialKeychainItem() KeychainItem {
	env := readKeychainEnv()
	return KeychainItem{
		Service: keychainServiceName(env, keychainCredentialsItem),
		Account: keychainAccountName(env, osUsername),
	}
}

// keychainServiceName is qpt(), and three things about it are easy to get
// wrong.
//
// CLAUDE_SECURESTORAGE_CONFIG_DIR OUTRANKS CLAUDE_CONFIG_DIR, and completely.
// When the securestorage variable is DEFINED it decides both halves on its own
// and CLAUDE_CONFIG_DIR is not consulted at all -- which is the same asymmetry
// ccpath.CredentialHome already has for the credential PATH, for the same
// reason: that variable scopes credentials independently of everything else.
//
// THE SUFFIX TEST IS TRUTHINESS, NOT DEFINEDNESS. Claude Code writes
// `!process.env.CLAUDE_CONFIG_DIR`, so a variable that is set to the empty
// string behaves exactly like an unset one and yields the UNSUFFIXED item.
// Definedness and truthiness differ on precisely that value, and it is the one
// a test isolate sets. It is not an EQUALITY test either: setting
// CLAUDE_CONFIG_DIR to the literal default path still produces a suffixed item,
// because the value is hashed rather than compared against anything.
//
// THE HASHED STRING IS THE RAW VALUE, NFC-normalized. See keychainEnv.
func keychainServiceName(env keychainEnv, item string) string {
	var hashed string
	var suffixed bool
	if env.secureStorageSet {
		hashed, suffixed = env.secureStorageDir, env.secureStorageDir != ""
	} else {
		hashed, suffixed = env.configDir, env.configDir != ""
	}

	tail := ""
	if suffixed {
		sum := sha256.Sum256([]byte(norm.NFC.String(hashed)))
		tail = "-" + hex.EncodeToString(sum[:])[:8]
	}
	return keychainBaseService + oauthFileSuffix(env.customOAuthURL) + item + tail
}

// oauthFileSuffix is al().OAUTH_FILE_SUFFIX, which reads like an opaque
// constant and is in fact decided by one environment variable.
//
// Claude Code's environment selector is a function that statically returns
// "prod", so the "-staging-oauth" and "-local-oauth" suffixes it also knows are
// unreachable in a released binary. What is reachable is
// CLAUDE_CODE_CUSTOM_OAUTH_URL: setting it rewrites the whole OAuth config and
// stamps "-custom-oauth" onto every derived name. getOauthConfig also refuses
// any URL outside its own allow-list by throwing, so a Claude Code that runs at
// all with the variable set is a Claude Code using this suffix -- which is why
// the test here is the value's truthiness and not its contents.
func oauthFileSuffix(customOAuthURL string) string {
	if customOAuthURL != "" {
		return keychainCustomOAuthSuffix
	}
	return ""
}

// osUsername is the operating system's answer for the current user, which on a
// CGO-free darwin build is still the real directory-service answer: Go's
// os/user takes its getpwuid path on darwin whether or not cgo is enabled
// (`//go:build (cgo || darwin) && !osusergo && unix && !android`), because
// libSystem is always linked there. Without that, a static build would fall
// back to $USER alone and disagree with Claude Code on exactly the headless
// hosts where $USER is unset.
//
// It is a var so a test can make the operating system fail without needing a
// host that has no user.
var osUsername = func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// keychainAccountName is Claude Code's getUsername(): $USER, then the operating
// system, then a constant -- and the pattern is applied to whichever of those
// answered, so an unusable name from the OS falls back just as an unusable
// $USER does. An OS lookup that fails is treated as an empty answer, which the
// pattern then rejects, which is how Claude Code's try/catch behaves too.
func keychainAccountName(env keychainEnv, osUser func() (string, error)) string {
	name := env.user
	if name == "" {
		if fromOS, err := osUser(); err == nil {
			name = fromOS
		}
	}
	if !keychainAccountPattern.MatchString(name) {
		return keychainFallbackAccount
	}
	return name
}

// keychainFailure is why a `security` invocation did not answer. It exists so
// doctor can say something the user can act on: "the keychain is locked" and
// "there is no keychain here" both mean "ccdad cannot tell", and they want
// opposite responses.
type keychainFailure string

const (
	failDuplicateItem   keychainFailure = "duplicate-item"
	failUnavailable     keychainFailure = "keychain-unavailable"
	failNoKeychain      keychainFailure = "no-keychain"
	failItemNotFound    keychainFailure = "item-not-found"
	failNoInteraction   keychainFailure = "interaction-not-allowed"
	failUserCanceled    keychainFailure = "user-canceled"
	failAuthFailed      keychainFailure = "auth-failed"
	failLocked          keychainFailure = "keychain-locked"
	failEmpty           keychainFailure = "empty"
	failOther           keychainFailure = "other"
	failTimedOut        keychainFailure = "timed-out"
	failLingering       keychainFailure = "not-reaped"
	failSecurityMissing keychainFailure = "security-missing"
)

// classifyKeychainError mirrors Claude Code's own stderr classifier, IN ITS
// ORDER, because the order is load-bearing: the tests are substring tests on
// one lowercased string and several of them can match at once. A keychain that
// is locked and reports "User canceled the operation" classifies as
// user-canceled and not as locked, because Claude Code asks about cancellation
// first, and a diagnostic that disagrees with Claude Code about why the same
// spawn failed is worse than no diagnostic.
func classifyKeychainError(stderr string) keychainFailure {
	if stderr == "" {
		return failEmpty
	}
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "errsecduplicateitem"), strings.Contains(s, "already exists"):
		return failDuplicateItem
	case strings.Contains(s, "unable to open"), strings.Contains(s, "could not open"):
		return failUnavailable
	case strings.Contains(s, "errsecnodefaultkeychain"),
		strings.Contains(s, "default keychain"),
		strings.Contains(s, "no keychain"):
		return failNoKeychain
	case strings.Contains(s, "errsecitemnotfound"), strings.Contains(s, "item could not be found"):
		return failItemNotFound
	case strings.Contains(s, "errsecinteractionnotallowed"),
		strings.Contains(s, "interaction is not allowed"),
		strings.Contains(s, "no user interaction"):
		return failNoInteraction
	case strings.Contains(s, "errsecusercanceled"), strings.Contains(s, "cancel"):
		return failUserCanceled
	case strings.Contains(s, "errsecauthfailed"),
		strings.Contains(s, "authorization"),
		strings.Contains(s, "authentication"),
		strings.Contains(s, "name or passphrase"):
		return failAuthFailed
	case strings.Contains(s, "locked"), strings.Contains(s, "unlock"):
		return failLocked
	}
	return failOther
}

// keychainFailureDetail is the sentence doctor prints for a probe that could not
// answer. Every branch says something only that branch says: a message that
// merely repeated the level would let the whole classifier be deleted without a
// test noticing.
func keychainFailureDetail(f keychainFailure) string {
	switch f {
	case failLocked:
		return "the login keychain is locked, so ccdad cannot tell whether a stale item is there; unlock it and run doctor again"
	case failNoInteraction:
		return "the keychain refused a non-interactive lookup, which is what a headless or SSH session gets; run doctor from a logged-in desktop session"
	case failUserCanceled:
		return "the keychain prompt was dismissed, so the lookup never completed"
	case failAuthFailed:
		return "the keychain rejected the credentials offered for it"
	case failNoKeychain:
		return "this machine has no default keychain, so there is nothing for a downgraded Claude Code to read"
	case failUnavailable:
		return "the keychain could not be opened"
	case failDuplicateItem:
		return "the keychain reported a duplicate item, which a lookup should never produce"
	case failTimedOut:
		return "the lookup did not finish in time and was killed; a keychain waiting on an unlock dialog nobody will answer does this"
	case failLingering:
		return "the lookup exited but something it started held its output open, so ccdad stopped waiting and cannot trust what it read"
	case failSecurityMissing:
		return "/usr/bin/security is not there, so ccdad cannot look"
	case failItemNotFound:
		// Reachable only from a `security` that said "not found" on some exit
		// code other than 44, which is not a machine ccdad can reason about --
		// the exit code is the contract, and this one broke it.
		return "the keychain said the item is absent but exited with an unexpected status, so ccdad is not treating that as an answer"
	case failEmpty:
		return "the lookup failed without saying why"
	}
	return "the lookup failed for a reason ccdad does not recognise"
}

// trimKeychainSecret strips the ONE newline `security -w` writes after the
// value, and no more. A blanket TrimSpace would silently alter a stored secret
// that legitimately ends in whitespace, and this value is parsed as JSON by
// whoever wrote it -- ccdad is not entitled to reshape it.
func trimKeychainSecret(out string) string {
	return strings.TrimSuffix(out, "\n")
}
