package cclink

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Legacy macOS Keychain support. "Legacy" here has a date on it now rather than
// an open end: 2.1.112 (2026-04-16) is the LAST Claude Code whose credential
// store reads these items, and 2.1.113 (2026-04-17) removed the backend
// outright. Every release from 2.1.113 on -- 2.1.222, 2.1.238, 2.1.240, 2.1.241
// -- names its secure-storage backend "plaintext" and reads .credentials.json.
// The boundary was measured, not inferred: every published release from 2.1.112
// to 2.1.129 was fetched and searched for the backend, and it disappears at
// exactly 2.1.113.
//
// THE READ ORDER, which is the fact everything else here turns on. Every release
// sampled from 0.2.125 through 2.1.112 wraps the keychain in the same
// combinator, and the ORDER never varies -- only the spelling of its null test
// does (`Q!=null` up to ~1.0.0, `K!==null&&K!==void 0` from ~1.0.30, which mean
// the same thing):
//
//	{name:`${A.name}-with-${B.name}-fallback`,
//	 read(){let K=A.read();if(K!==null&&K!==void 0)return K;return B.read()||{}}, ...}
//	function GP(){if(process.platform==="darwin")return <combinator>;return <file>}
//
// 2.1.50 adds a readAsync beside it with the same order, spawning through
// execFile rather than a shell.
//
// Keychain FIRST, then the file. Never keychain-only. Two consequences, and they
// are the two this path is kept for:
//
//   - An item that is present SHADOWS whatever ccdad writes to
//     .credentials.json, so on a machine running <=2.1.112 a ccdad switch
//     appears to work and changes nothing.
//   - DELETING the item redirects that Claude Code to the file rather than
//     logging it out. That was the open question -- repair or credential loss --
//     and the fallback settles it in favour of repair.
//
// BUT THE REPAIR DOES NOT HOLD, and this is the half a reading of read() alone
// misses. On a machine still running <=2.1.112 the very next credential write
// -- an ordinary access-token refresh, so hours -- runs the combinator's
// update(), which from 1.0.36 onward is:
//
//	update(K){let z=q.read(),Y=q.update(K);
//	  if(Y.success){if(z===null)K.delete();return Y} ...}
//
// After the delete the keychain read returns null, so `security
// add-generic-password -U` RE-CREATES the item and, because the pre-write read
// was null, the FILE is unlinked. Within hours the shadowing item is back and
// ccdad's .credentials.json is gone. Deleting is therefore cleanup on a machine
// that has already moved to 2.1.113+ (nothing there can recreate it) and is NOT
// a fix on one that has not -- there, the fix is to upgrade Claude Code. doctor
// says exactly that, which is why it hands over the command rather than running
// it.
//
// And on a machine actually running <=2.1.112 the item is not stale at all: it
// is that Claude Code's LIVE login. Destroying a credential during a switch is
// the highest-rated risk this tool carries, and nothing here is worth spending
// that on.
//
// The other half of the same era, recorded because it looks like this file's
// problem and is not: CLAUDE_SECURESTORAGE_CONFIG_DIR does not occur even once
// in 2.1.112, so on such a machine `ccdad run`'s default scoping is ignored too
// and the session reads the machine's own credentials file.
//
// WHERE TO RE-CHECK ALL OF THIS. npm carries every release, and up to 2.1.112
// the tarball still contains a readable `package/cli.js`:
//
//	npm pack @anthropic-ai/claude-code@2.1.112
//	tar -xzOf anthropic-ai-claude-code-2.1.112.tgz package/cli.js > cc.js
//	grep -aoP 'name:"plaintext".{0,1800}' cc.js | head -1
//
// From 2.1.113 the package is a launcher and the code ships as a native binary
// in @anthropic-ai/claude-code-<platform>; `tar -xzO | tr -d '\000'` over that
// gives the same searchable JS (see claude-code-oauth-ground-truth for the
// dd/tr recipe against an installed copy).
//
// Do NOT search for the "-credentials" constant in a current build: it is
// unreferenced there, so its minified name has one hit in a 229 MB binary and
// the obvious conclusion -- that the derivation is gone -- is wrong. Search for
// the FUNCTION by its body rather than its name, which changes every release
// (`qpt` in 2.1.238, `xht` in 2.1.240):
//
//	grep -aoP '.{0,300}CLAUDE_SECURESTORAGE_CONFIG_DIR,r=t.{0,400}'
//
// What that finds is the DEAD builder, and it is not the one this file
// implements -- keychainServiceName says why.
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
	// configDir is CLAUDE_CONFIG_DIR's value, and it is the ONLY directory
	// variable in this derivation. CLAUDE_SECURESTORAGE_CONFIG_DIR is not read
	// here on purpose -- see keychainServiceName.
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
	return keychainEnv{
		configDir:      os.Getenv("CLAUDE_CONFIG_DIR"),
		customOAuthURL: os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL"),
		user:           os.Getenv("USER"),
	}
}

// CredentialKeychainItems is every name this machine's legacy OAuth item could
// carry, most recent spelling first.
//
// There is more than one because Claude Code changed how it hashed
// CLAUDE_CONFIG_DIR mid-era: 2.1.38 and later normalize the value to NFC before
// hashing, 1.0.30 through 2.1.37 hash the bytes as they came. A decomposed
// value therefore names TWO different items depending on which build wrote it,
// and probing only one of them is the same false "no legacy item" this
// derivation was corrected to stop producing. They coincide -- one candidate --
// whenever CLAUDE_CONFIG_DIR is unset or already composed, which is every
// ordinary machine.
//
// The older eras are deliberately NOT candidates, and not merely because each
// is another spawn. Before ~1.0.30 there was no hash at all and before 1.0.128
// no OAUTH_FILE_SUFFIX, so their names are the UNSUFFIXED ones -- which on a
// machine that sets CLAUDE_CONFIG_DIR or CLAUDE_CODE_CUSTOM_OAUTH_URL belong to
// a different profile's login, not to this one. Probing them would report, and
// invite deleting, somebody else's item. The NFC pair is the only split where
// both spellings name the SAME logical item.
//
// It is a derivation, not a probe: it says what the items WOULD be called, and
// says nothing about whether any exists. Nothing here touches the filesystem or
// spawns anything, so it is safe to call on any platform and in any state.
func CredentialKeychainItems() []KeychainItem {
	env := readKeychainEnv()
	account := keychainAccountName(env, osUsername)
	names := keychainServiceNames(env, keychainCredentialsItem)
	items := make([]KeychainItem, 0, len(names))
	for _, name := range names {
		items = append(items, KeychainItem{Service: name, Account: account})
	}
	return items
}

// CredentialKeychainItem is the name a CURRENT Claude Code's rules would give
// the item -- the first candidate. Callers that intend to find an item that
// exists want CredentialKeychainItems instead.
func CredentialKeychainItem() KeychainItem { return CredentialKeychainItems()[0] }

// keychainServiceName is the service attribute of the item, derived the way the
// releases that WROTE one derived it -- 1.0.128 through 2.1.112, read out of
// 2.1.112's bundle. One thing inside that span is NOT constant, and
// keychainServiceNames is where it is handled: the config directory is hashed
// as it came through 2.1.37 and NFC-normalized from 2.1.38 on. (Before 1.0.128
// the account was not computed in JS at all:
// the item name went into a SHELL string as a literal `$USER`, and there was no
// OAUTH_FILE_SUFFIX before ~1.0.128 and no hash suffix before ~1.0.30. Those
// eras name the same item as this one on any machine that sets neither
// CLAUDE_CONFIG_DIR nor CLAUDE_CODE_CUSTOM_OAUTH_URL.)
//
//	function Fh(q=""){let K=A7(),
//	    z=!process.env.CLAUDE_CONFIG_DIR?"":`-${sha256(K).hex.substring(0,8)}`;
//	  return `Claude Code${r7().OAUTH_FILE_SUFFIX}${q}${z}`}
//	A7 = memo(()=>(process.env.CLAUDE_CONFIG_DIR ?? join(homedir(),".claude")).normalize("NFC"))
//
// CLAUDE_SECURESTORAGE_CONFIG_DIR IS NOT PART OF IT, and this is the correction
// that matters. Today's dead builder lets that variable outrank
// CLAUDE_CONFIG_DIR and decide the hash on its own -- but the variable does not
// occur even ONCE in 2.1.112, the last release with a live keychain. No build
// that could have written one of these items had ever heard of it, so a name
// derived from it names an item that cannot exist.
//
// It is not a harmless extra either: `ccdad run` sets that variable by design —
// it is how a session's credentials are scoped — so the old reading made `ccdad
// doctor` inside a run session look for
// a hash of the SESSION's credential directory and report "no legacy item" with
// certainty while the real one sat under the unsuffixed name. Reading it the
// keychain era's way makes both questions agree -- what wrote the item, and what
// a <=2.1.112 Claude Code launched in THIS environment would read.
//
// THE SUFFIX TEST IS TRUTHINESS, NOT DEFINEDNESS. Claude Code writes
// `!process.env.CLAUDE_CONFIG_DIR`, so a variable set to the empty string
// behaves exactly like an unset one and yields the UNSUFFIXED item. Definedness
// and truthiness differ on precisely that value, and it is the one a test
// isolate sets. It is not an EQUALITY test either: setting CLAUDE_CONFIG_DIR to
// the literal default path still produces a suffixed item, because the value is
// hashed rather than compared against anything.
//
// THE HASHED STRING IS THE RAW VALUE -- not resolved, not cleaned. A7() hands
// the variable back untouched apart from normalization, and the suffix only
// exists when the variable is truthy, so "the resolved config dir" and "the
// variable" never part company on a machine that has one. Whether it is
// normalized first is the one thing that DOES vary across the era; see
// keychainServiceNames.
func keychainServiceNames(env keychainEnv, item string) []string {
	base := keychainBaseService + oauthFileSuffix(env.customOAuthURL) + item
	if env.configDir == "" {
		return []string{base}
	}
	// NFC first: it is what 2.1.38..2.1.112 wrote, and the later half of the era
	// is the likelier one to have left an item behind. The raw spelling is
	// 1.0.30..2.1.37's, and it only differs at all for a value that was not
	// already composed.
	composed := norm.NFC.String(env.configDir)
	names := []string{base + keychainHashSuffix(composed)}
	if env.configDir != composed {
		names = append(names, base+keychainHashSuffix(env.configDir))
	}
	return names
}

// keychainServiceName is the first of those, which is the name a current Claude
// Code's rules would produce.
func keychainServiceName(env keychainEnv, item string) string {
	return keychainServiceNames(env, item)[0]
}

// keychainHashSuffix is the eight hex characters Claude Code appends, over
// whatever string its era decided to hash.
func keychainHashSuffix(hashed string) string {
	sum := sha256.Sum256([]byte(hashed))
	return "-" + hex.EncodeToString(sum[:])[:8]
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

// keychainAccountName is the account attribute a Keychain-era Claude Code put
// on the item, which is `process.env.USER || os.userInfo().username` inside a
// try/catch whose only other answer is the constant.
//
// THERE IS NO PATTERN HERE, and there used to be. Today's dead builder validates
// the name against ^[a-zA-Z0-9._-]+$ and rewrites anything else to
// "claude-code-user"; NO release that ever wrote one of these items did. Keeping
// that check would make ccdad look for "claude-code-user" on exactly the machines
// whose item is under a real name with a space or a non-ASCII letter in it -- a
// false "no legacy item" from the one diagnostic that exists to find it.
//
// An OS lookup that fails, or that answers with nothing, takes the constant:
// that is Claude Code's catch, and `security -a ""` is not a lookup anyone
// wants ccdad to spell.
func keychainAccountName(env keychainEnv, osUser func() (string, error)) string {
	if env.user != "" {
		return env.user
	}
	if fromOS, err := osUser(); err == nil && fromOS != "" {
		return fromOS
	}
	return keychainFallbackAccount
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
