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

// macOS Keychain support. NOT legacy: this file used to open by dating the
// keychain era's end, and that date was wrong.
//
// WHAT THE OLD HEADER CLAIMED, so the correction has something to point at:
// that 2.1.112 (2026-04-16) is the LAST Claude Code whose credential store
// reads these items, that 2.1.113 removed the backend outright, and that every
// release from 2.1.113 on -- 2.1.222, 2.1.238, 2.1.240, 2.1.241 -- "names its
// secure-storage backend plaintext and reads .credentials.json". ccver carried
// the same boundary as LastPreSecureStorageDir, doctor printed it as "nothing is broken
// right now", and ccdad wrote only .credentials.json on the strength of it.
//
// IT IS FALSE, and the way it failed is worth more than the fact. Three builds
// sitting on the machine that found this -- 2.1.234, 2.1.238 and 2.1.251, one
// of them named in that list -- each contain a whole keychain backend, and it
// SPAWNS:
//
//	name:"keychain",read(){let e=B$(),t=e.cache;
//	  if(Date.now()-t.cachedAt<Aqt)return t.data;
//	  let r=e.lastReadFailure;if(r!==null&&Date.now()-r<OEn)return t.data;
//	  try{let a=a0(s5),o=xC(),
//	    s=nJ(`security find-generic-password -a "${o}" -w -s "${a}"`,{timeout:m});
//	    if(s){let i=V(s);return e.cache={data:i,cachedAt:Date.now()},i}}catch(a){}
//
// THE MEASUREMENT THAT MISSED IT searched a release for `name:"plaintext"`,
// found it, and concluded the backend WAS plaintext. But the store is a
// combinator named `${A.name}-with-${B.name}-fallback` over both members, so
// the fallback's own name is present in every build that has a keychain
// primary. Finding B proves nothing about A. Count them separately instead --
// on an installed build, with no download:
//
//	LC_ALL=C tr -d '\000' < ~/.local/share/claude/versions/<v> > cc.js
//	LC_ALL=C grep -ac 'name:"keychain"' cc.js
//	LC_ALL=C grep -ac 'find-generic-password' cc.js
//
// LC_ALL=C is not decoration: without it `tr` can drop nothing at all and leave
// a zero-byte file, which reads as "the backend is gone" exactly like the real
// thing. And a lookbehind wider than ~500 chars over 166 MB does not finish in
// two minutes -- take the byte offset from `grep -abo` and `dd` a window.
//
// WHAT IT COST, measured on the machine that found it: the item held one
// account and .credentials.json another, ccdad had switched an hour earlier,
// `ccdad which` named the file's account, and every request in that hour was
// metered to the item's. The daemon logged the switch. Nothing errored.
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
// gives the same searchable JS. An INSTALLED copy needs no download at all --
// the same bundle sits readable inside the executable, and stripping the NUL
// padding is the whole extraction:
//
//	tr -d '\000' < ~/.local/share/claude/versions/<version> > cc.js
//
// Written down here rather than pointed at, and re-run rather than copied: on
// 2026-08-24 against the installed 2.1.241 -- 342,636,848 bytes, sha256
// 0771bd86..., the size and digest ccver.go records for that release -- it
// yields the 229 MB of searchable text the paragraph below counts hits in, and
// the grep below finds its one hit there. Budget ~18s per pattern at that size.
//
// FOR MORE THAN ONE QUESTION, NARROW FIRST. The bundle begins at byte
// 307,481,166 in that build, so `dd if=<version> bs=1M skip=290 count=50` puts
// it in a 36 MB window the same greps answer in under a second. The offset is
// per-release and must not be carried forward: the identical marker sits at
// 262,636,955 in the 2.1.222 installed beside it. Ask the build in hand for its
// own with `grep -abo 'name:"plaintext"' <version> | head -1`.
//
// Do NOT search for the "-credentials" constant in a current build: it is
// unreferenced there, so its minified name has one hit in a 229 MB binary and
// the obvious conclusion -- that the derivation is gone -- is wrong. Search for
// the FUNCTION by its body rather than its name, which changes every release
// (`qpt` in 2.1.238, `xht` in 2.1.240 and still `xht` in 2.1.241):
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
	failSaidNothing     keychainFailure = "said-nothing"
	failOther           keychainFailure = "other"
	failTimedOut        keychainFailure = "timed-out"
	failLingering       keychainFailure = "not-reaped"
	failSecurityMissing keychainFailure = "security-missing"
)

// classifyKeychainFailure is the classifier callers use: what `security` SAID if
// it said anything, and what it EXITED with if it did not.
//
// The order is not a preference. classifyKeychainError below mirrors Claude
// Code's own classifier so that ccdad and Claude Code never disagree about why
// one spawn failed, and an exit code consulted first would break that for every
// failure that has stderr. The code is the fallback for the path that produces
// none, never a second opinion about one that did.
func classifyKeychainFailure(stderr string, code int) keychainFailure {
	if stderr != "" {
		return classifyKeychainError(stderr)
	}
	return classifyKeychainExit(code)
}

// classifyKeychainExit names a silent failure from its exit status.
//
// `security` exits with the OSStatus truncated to its low byte -- which is not
// a guess: securityNotFoundCode is 44 because errSecItemNotFound is -25300, and
// this file has relied on that arithmetic since the keychain backend landed.
// The values below were read out of the SDK's own SecBase.h rather than
// remembered.
//
// ONLY the keychain band is read, and only where ccdad already has a sentence
// that says the right thing. A low byte is ambiguous across the whole OSStatus
// space, so a code that is not listed keeps failSaidNothing and its number: a
// bare number is a fact the reader can look up, and a wrong name is a
// confident wrong answer of exactly the kind the silent failure taught this
// file to stop giving. errSecInteractionRequired (29) and errSecNotAvailable
// (53) are real codes left out on that rule -- neither has a sentence here that
// means what it means.
//
// 36 is why this exists. A daemon logged `said-nothing (exit 36)` on every tick
// for two minutes after a restart, and 36 is errSecInteractionNotAllowed: the
// keychain refusing a lookup it cannot ask a human about, which is what a
// process outside the user's login session gets. ccdad had the sentence for it
// and could not reach it, because `security` writes nothing to stderr there.
func classifyKeychainExit(code int) keychainFailure {
	switch code {
	case 36: // errSecInteractionNotAllowed
		return failNoInteraction
	case 37, 50: // errSecNoDefaultKeychain, errSecNoSuchKeychain
		return failNoKeychain
	case 45: // errSecDuplicateItem
		return failDuplicateItem
	case 51: // errSecAuthFailed
		return failAuthFailed
	case 128: // errSecUserCanceled
		return failUserCanceled
	}
	return failSaidNothing
}

// classifyKeychainError mirrors Claude Code's own stderr classifier, IN ITS
// ORDER, because the order is load-bearing: the tests are substring tests on
// one lowercased string and several of them can match at once. A keychain that
// is locked and reports "User canceled the operation" classifies as
// user-canceled and not as locked, because Claude Code asks about cancellation
// first, and a diagnostic that disagrees with Claude Code about why the same
// spawn failed is worse than no diagnostic.
func classifyKeychainError(stderr string) keychainFailure {
	if stderr == "" {
		return failSaidNothing
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
		// Two readers, and the second needs the second half. A person running
		// doctor can move to another shell. A DAEMON cannot: it inherited its
		// session at startup and every successor it spawns inherits the same
		// one, so the automatic replacement cannot fix this and the restart has
		// to come from somewhere else. That is why one wedged daemon survived a
		// restart and another did not.
		return "the keychain refused a non-interactive lookup, which is what a headless or SSH session gets; " +
			"run doctor from a logged-in desktop session, and restart the daemon from one too if it is the daemon " +
			"reporting this — it inherited the refusing session and its replacements inherit it as well"
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
	case failSaidNothing:
		// Reached without an exit code only when nothing set one. The
		// code-carrying sentence is on *KeychainError.Detail, because the
		// number is the whole content of this failure.
		return "the lookup exited non-zero and printed nothing, so there is no reason to report"
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

// liveAccountPattern is xC()'s `s`, the character set 2.1.251 accepts in an
// account name before it gives up and uses the constant.
var liveAccountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// LiveKeychainItem is the credential item a CURRENT Claude Code, launched with
// env, reads and writes.
//
// IT IS A SECOND DERIVATION AND THAT IS THE POINT. CredentialKeychainItems
// names an item a <=2.1.112 build could have LEFT BEHIND, and no such build had
// heard of CLAUDE_SECURESTORAGE_CONFIG_DIR, so reading that variable there names
// an item which cannot exist -- keychainServiceName's header says so. This one
// answers the opposite question: which item is the INSTALLED build using right
// now, in THIS environment. The two agree on a machine that exports neither
// variable, which is why the difference stayed invisible until a scoped session
// needed it.
//
// Measured verbatim in 2.1.251, in the credential-store chunk:
//
//	var s5="-credentials";
//	function a0(n=""){let e=process.env.CLAUDE_SECURESTORAGE_CONFIG_DIR,
//	  t=e!==void 0?!e:!process.env.CLAUDE_CONFIG_DIR,
//	  r=e!==void 0?e.normalize("NFC"):be(),
//	  c=t?"":`-${a("sha256").update(r).digest("hex").substring(0,8)}`;
//	  return `Claude Code${zt().OAUTH_FILE_SUFFIX}${n}${c}`}
//
// So CLAUDE_SECURESTORAGE_CONFIG_DIR OUTRANKS CLAUDE_CONFIG_DIR here, and the
// test on the winner is DEFINEDNESS, not truthiness: a defined-but-empty value
// selects the unsuffixed item, exactly as an unset CLAUDE_CONFIG_DIR does. The
// hashed string is the variable's own NFC-normalized value, not a resolved or
// symlink-followed form of it.
//
// Every `ccdad run` session and every probe exports a real directory into that
// variable, so their item always carries a suffix and can never collide with
// the machine's own login. That is the property adoptBack depends on.
func LiveKeychainItem(env []string) KeychainItem {
	secure, secureSet := envLookup(env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	config, _ := envLookup(env, "CLAUDE_CONFIG_DIR")
	custom, _ := envLookup(env, "CLAUDE_CODE_CUSTOM_OAUTH_URL")
	user, _ := envLookup(env, "USER")

	unsuffixed, hashed := config == "", config
	if secureSet {
		unsuffixed, hashed = secure == "", secure
	}
	service := keychainBaseService + oauthFileSuffix(custom) + keychainCredentialsItem
	if !unsuffixed {
		service += keychainHashSuffix(norm.NFC.String(hashed))
	}
	return KeychainItem{Service: service, Account: liveKeychainAccount(user)}
}

// liveKeychainAccount is xC(), the account attribute the INSTALLED build puts
// on an item it writes:
//
//	function xC(){let n;try{n=process.env.USER||u().username}catch{n="claude-code-user"}
//	  if(!s.test(n))return"claude-code-user";return n}
//
// It differs from keychainAccountName by exactly one rule, and both exist for
// that difference. This build rewrites a name outside liveAccountPattern to the
// constant; NO build that could have left a legacy item behind ever did.
// Applying the pattern to the legacy hunt would report "no item" on the very
// machines whose item sits under a real name with a space in it; omitting it
// here would name an item this build never writes.
func liveKeychainAccount(user string) string {
	name := user
	if name == "" {
		if fromOS, err := osUsername(); err == nil {
			name = fromOS
		}
	}
	if !liveAccountPattern.MatchString(name) {
		return keychainFallbackAccount
	}
	return name
}

// envLookup reads a variable out of an explicit environment slice, reporting
// DEFINEDNESS separately from the value because a0 tests the two differently.
// The last assignment wins, which is what exec does with a duplicated name.
func envLookup(env []string, name string) (string, bool) {
	prefix := name + "="
	value, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value, found = kv[len(prefix):], true
		}
	}
	return value, found
}
