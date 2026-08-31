package cclink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// maxCredentialsSize matches Claude Code's own 1 MiB cap. A file over it is
// treated as corrupt rather than parsed.
const maxCredentialsSize = 1 << 20

// LockTimeout bounds the wait for each Claude Code lock. Claude Code holds
// the credential locks for one token-endpoint round trip, so this outlasts a
// normal hold without stalling the CLI indefinitely. It is per lock, so the
// worst case is roughly three times this.
//
// It is a var, not a const, so a test can shrink it to exercise the timeout
// path -- the one that produces the error the CLI maps to a non-zero exit
// code -- without an unbounded contention test.
var LockTimeout = 9 * time.Second

var (
	// ErrTooLarge means the credentials file exceeded the 1 MiB cap.
	ErrTooLarge = errors.New("credentials file is too large to be valid")
	// ErrSymlink means the credentials path is a symlink. Claude Code opens
	// it O_NOFOLLOW and refuses; ccdad refuses for the same reason, via the
	// platform-specific openCredentialsFile in store_unix.go / store_windows.go,
	// so neither process can be redirected into reading or writing tokens
	// somewhere unexpected.
	ErrSymlink = errors.New("credentials path is a symlink")
	// ErrNoChange is what an ActivateWith decision returns to stand down: the
	// locks were taken, the file was read, and the answer is that nothing should
	// be written. It is a sentinel rather than a (Blob, bool) return so the one
	// path that must never be confused with it -- a decision that FAILED -- keeps
	// its own error, and so a caller can tell the two apart with errors.Is.
	ErrNoChange = errors.New("the credentials file is to be left as it is")
)

// Load reads the login Claude Code is actually authenticating with. A missing
// one is an empty Blob and no error: that is a machine where Claude Code has
// never logged in.
//
// ON macOS THAT IS THE KEYCHAIN ITEM WHEN ONE IS THERE. Claude Code assembles
// its credential store as a keychain-with-plaintext-fallback combinator whose
// read is
//
//	read(a){let o=e.read(a);if(o!==null&&o!==void 0)return o;return t.read(a)||{}}
//
// so an item that exists is the login and the file is a store nothing consults.
// Reading the file past such an item is a CONFIDENT WRONG ANSWER -- it is what
// had `ccdad which` naming one account while an hour of work went to another --
// and every command that asks who is live asks through here.
//
// LoadKeychainCredentials's own header used to rule the opposite way, that Load
// must not consult the keychain because 2.1.113 dropped the backend. That
// premise was measured again and is false; keychain_security.go's Write carries
// the measurement.
//
// A keychain that cannot be READ is an error rather than a fallback. The
// fallback is for "there is no item", which is a fact; a locked keychain is an
// unknown, and answering an unknown with the file is the same confident wrong
// answer in a quieter form.
func Load() (Blob, error) {
	live, _, err := LoadWithSource()
	return live, err
}

// CredentialSource is WHICH STORE answered. It exists because on macOS "who is
// logged in" and "where does that come from" are two different questions, and
// every command that reported the first was naming the second from a constant:
// `which` said "the Claude Code credentials file" while the keychain item was
// what it had read, on exactly the machine where the difference was the whole
// point.
type CredentialSource int

const (
	// SourceNone is no login anywhere.
	SourceNone CredentialSource = iota
	// SourceCredentialsFile is ~/.claude/.credentials.json.
	SourceCredentialsFile
	// SourceKeychainItem is the macOS Keychain item, which Claude Code's
	// credential store reads BEFORE the file.
	SourceKeychainItem
)

// String names the store for a person.
//
// It is a NOUN PHRASE and deliberately short, because both readers put it
// inside a sentence of their own: `which` prints "via <this>" and doctor prints
// "the login in <this>". Spelling it "the Claude Code credentials file" made
// the second read "the login in the Claude Code credentials file", which says
// Claude Code twice and reads like a different file from the one meant.
func (s CredentialSource) String() string {
	switch s {
	case SourceKeychainItem:
		return "the macOS Keychain item, which Claude Code reads first"
	case SourceCredentialsFile:
		return "the credentials file"
	}
	return "no credential store"
}

// LoadWithSource is Load, and also says which store answered.
//
// A missing login is (Blob{}, SourceNone, nil): the file half returns an empty
// Blob for a machine where Claude Code has never logged in, and naming a store
// for a login that is not there would be a third wrong answer of the same kind.
func LoadWithSource() (Blob, CredentialSource, error) {
	if live, ok, err := loadFromKeychain(); err != nil || ok {
		if err != nil {
			return nil, SourceNone, err
		}
		return live, SourceKeychainItem, nil
	}
	path, err := ccpath.CredentialsPath()
	if err != nil {
		return nil, SourceNone, err
	}
	live, err := loadFrom(path)
	if err != nil {
		return nil, SourceNone, err
	}
	if len(live) == 0 {
		return live, SourceNone, nil
	}
	return live, SourceCredentialsFile, nil
}

// loadLiveUnderLock is the merge base: the store Claude Code reads, with its
// file half taken from the directory the locks cover.
//
// It exists because the base is not only what gets merged onto -- it is the
// blob every caller of ActivateWith DECIDES from. switcher.Execute computes
// AlreadyOn out of it, and taking it from the file made a switch decline on the
// one machine that needed one: file and item naming different accounts is the
// shape this path exists to repair, and it was the shape that made it refuse.
//
// Two reasons point the same way, which is what makes this safe as well as
// necessary. The item is the LOGIN, so a decision about who is live has to read
// it. And the item is the FRESHER blob, because Claude Code's combinator writes
// the primary and skips the fallback on success -- so a machine with an item
// has a credentials file that stops being updated, and merging onto it would
// put back machine-scoped keys the item had already moved past.
//
// livePath rather than ccpath.CredentialsPath(): loadFrom's own comment says
// why, and reaching for Load() here would quietly lose that scope on the file
// half. The keychain half has no such scope to lose -- the item's name comes
// from CLAUDE_CONFIG_DIR, which is how `ccdad run` gets its own item rather
// than the machine's.
// THE SECOND RETURN IS WHETHER THE ITEM ANSWERED, and it is not a convenience.
// installIntoKeychain replaces the item WHOLESALE, so the only item it may
// write is the one this read took its base from; deriving that answer a second
// time, from a separate lookup, is what let the two disagree -- the lookup was
// attributes-only and could not fail, so a switch could commit to writing an
// item it had never read.
//
// THE ITEM IS NOT THE WHOLE BASE EITHER. The freshness argument above holds for
// what the item HAS; it says nothing about what the item never had. Claude
// Code's combinator writes the primary and skips the fallback ON SUCCESS, so
// the moment a keychain write FAILS -- a locked keychain answers
// `{success:!1,transient:...}` -- it takes the plaintext fallback instead, and
// from then on the file holds machine-scoped keys the item has never seen. An
// MCP login made during that window lives only in the file. Merging onto the
// item alone would delete it on the next switch, so the file's machine keys are
// folded back in; see overlayMachineKeys, which lets the item win every
// collision and only refuses to DROP what neither store has superseded.
//
// A file that cannot be READ, behind an item that answered, is an error rather
// than an empty base. The write this base feeds replaces that file wholesale,
// so continuing on the item alone would destroy machine keys at exactly the
// moment ccdad cannot see what it is destroying.
func loadLiveUnderLock(livePath string) (Blob, bool, error) {
	item, ok, err := loadFromKeychain()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		live, ferr := loadFrom(livePath)
		return live, false, ferr
	}
	file, ferr := loadFrom(livePath)
	if ferr != nil {
		return nil, true, fmt.Errorf("the keychain item Claude Code reads first answered, but the "+
			"credentials file it shadows could not be read, and a switch rewrites that file: %w", ferr)
	}
	return overlayMachineKeys(item, file), true, nil
}

// loadFromKeychain is Load's first half: the item's login, and whether there
// was an item at all. Off macOS there is no such store and the answer is
// always "no item", which is not a failure.
func loadFromKeychain() (Blob, bool, error) {
	live, ok, err := LoadKeychainCredentials(context.Background())
	if errors.Is(err, ErrKeychainUnsupported) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return live, ok, nil
}

// loadFrom reads and decodes the credentials file at an already-resolved
// path. It exists so Activate can re-read from the SAME directory its locks
// cover (held.Scope()) rather than a fresh call to
// ccpath.CredentialsPath(), which re-resolves environment variables that
// ccdad itself is the program that mutates.
func loadFrom(path string) (Blob, error) {
	f, err := openCredentialsFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Blob{}, nil
		}
		return nil, err
	}
	defer f.Close()

	// Bound the read itself, not just a size check against a separate Stat:
	// enforcing the cap only against Lstat's result while a subsequent read
	// is unbounded would read an unbounded amount of a file that grew past
	// the cap in the window between the two calls.
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialsSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	if int64(len(data)) > maxCredentialsSize {
		return nil, fmt.Errorf("%s (over %d bytes): %w", path, maxCredentialsSize, ErrTooLarge)
	}
	if len(data) == 0 {
		return Blob{}, nil
	}
	var b Blob
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}
	if b == nil {
		b = Blob{}
	}
	return b, nil
}

// Activate makes incoming the live login.
//
// The whole read-merge-write runs under Claude Code's three credential
// locks, acquired via cclock.AcquireCredentials in Claude Code's own order.
// Without them, a switch landing inside Claude Code's refresh window is
// overwritten by the refreshed OLD account's token, and the snapshot ccdad
// just took keeps a pre-rotation refresh token. Under the locks, Claude
// Code's own double-checked re-read sees the swapped credential and
// abandons its refresh.
//
// The file is re-read INSIDE the lock (loadFrom held.Scope(), never Load's
// fresh ccpath.CredentialsPath()): whatever was loaded before
// the wait may be stale by the time the lock is granted, and a freshly
// re-resolved path could in principle disagree with the directory just
// locked if the environment changed in between -- ccdad is the program that
// mutates those variables for per-account scoping.
//
// held.Compromised() is consulted twice. Once, cheaply, right before the
// write: this is the only check that can PREVENT damage, by refusing to
// write at all once a takeover is already known. And once implicitly after
// the write, via held.Release()'s returned error, joined into Activate's own
// result rather than discarded -- this is what actually catches a takeover
// in practice, because cclock only notices one on its touch tick
// (DefaultTouchInterval, 3s): for a fast Activate the pre-write check will
// rarely have fired, while Release's own synchronous re-stat of each lock
// directory does not wait for that tick.
//
// Neither check triggers a rollback. With a same-directory atomic rename
// there is no partially-written state to restore, and if Claude Code wrote
// after us, a blind restore would clobber ITS write instead of preventing
// damage. This is what satisfies the rollback rule -- on any failure once
// the write has begun, restore from the pre-write snapshot -- for a
// compromise: the rule is vacuous here, because the one case with lingering
// state to worry about is exactly the case a restore would make worse. The
// caller gets the error and should re-read and verify rather than assume
// either side's state.
//
// Activate does not itself surface unrecognized top-level keys (the
// unknown-key probe). That is deferred to the CLI layer: it already holds
// the live Blob via Load and can call UnknownKeys directly, without widening
// this function's signature for every caller that does not need the result.
func Activate(incoming Blob) error {
	// Refused BEFORE any lock is taken, unlike ActivateWith's identical check:
	// the argument is already in hand, so a corrupt or truncated stored snapshot
	// costs nothing to reject, and rejecting it early keeps a doomed switch from
	// standing in front of Claude Code's own refresh.
	if err := requireLogin(incoming); err != nil {
		return err
	}
	return writeMerged(func(Blob) (Blob, error) { return incoming, nil })
}

// ActivateWith is Activate for a caller whose decision is only sound against
// the file as it is at the moment of the write.
//
// Activate re-reads under the lock too, but only to build its merge base: by
// the time it has the locks it has already committed to writing. An UNATTENDED
// executor cannot commit that early. Between the engine reading the live login
// and the swap landing, the user may have run `ccdad switch` by hand or logged
// in through Claude Code, and a daemon that goes ahead overwrites a choice a
// human made seconds ago. decide runs under the three credential locks with the
// live file as it is, and returns ErrNoChange to stand down.
//
// Two rules decide inherits, neither of which this function can enforce:
//
//   - It runs with Claude Code's refresh locks held, so it must not make a
//     network call, refresh a token, or fetch usage. cclock says why: Claude
//     Code's own refresh blocks on these, so anything slow here stalls the
//     user's session.
//   - It must not take ccdad's store lock. store/lock.go documents the store
//     lock as the OUTER one, and a store mutation inside a credential lock is
//     the opposite order -- two callers that pick opposite orders deadlock.
//     Reading a credential blob the caller already holds is fine; writing is
//     what waits until after this returns.
func ActivateWith(decide func(live Blob) (Blob, error)) error {
	return writeMerged(func(live Blob) (Blob, error) {
		incoming, err := decide(live)
		if err != nil {
			return nil, err
		}
		if err := requireLogin(incoming); err != nil {
			return nil, err
		}
		return incoming, nil
	})
}

// requireLogin refuses a snapshot Merge would turn into a logout. Merge deletes
// every account-scoped key and puts back only what the snapshot carries, so a
// snapshot with no claudeAiOauth silently signs the user out.
//
// ClearLogin is the same write with this refusal lifted, and lifting it is only
// correct there because logging the user out is what the caller asked for by
// name.
func requireLogin(incoming Blob) error {
	if _, ok := incoming["claudeAiOauth"]; !ok {
		return fmt.Errorf("refusing to activate a snapshot with no claudeAiOauth: it would log the user out")
	}
	return nil
}

// ClearLogin removes every account-scoped key from the live credentials file,
// leaving the machine-scoped ones -- MCP logins, this machine's device key --
// exactly where they are.
//
// This is Activate with an empty snapshot, and it logs the user out on purpose.
// It exists for the one target that cannot be installed as a credential record:
// an API-key account. Claude Code prefers a claudeAiOauth login over its stored
// primaryApiKey in every configuration (the client binds
// `anthropicAuthEnabled: BE()`, and BE() only turns off for an ENVIRONMENT key
// or an apiKeyHelper), so writing the key while a login is sitting in the
// credentials file activates nothing at all. Removing the login is what makes
// the key the answer.
//
// Nothing is lost by it: the account whose login is being removed keeps its own
// stored snapshot in ccdad's store, and switching back reinstalls it.
func ClearLogin() error {
	return writeMerged(func(Blob) (Blob, error) { return Blob{}, nil })
}

// writeMerged is the locked read-decide-merge-write all of the above perform.
func writeMerged(decide func(live Blob) (Blob, error)) (err error) {
	held, aerr := cclock.AcquireCredentials(LockTimeout)
	if aerr != nil {
		return aerr
	}
	defer func() { err = errors.Join(err, held.Release()) }()

	livePath := filepath.Join(held.Scope(), ccpath.CredentialsFile)

	live, itemIsLive, err := loadLiveUnderLock(livePath)
	if err != nil {
		return err
	}

	incoming, err := decide(live)
	if err != nil {
		// Including ErrNoChange, which is a decision and not a failure. It
		// still travels back through Release's deferred join, so a takeover
		// that happened while the caller was deciding is not lost just because
		// the decision was to stand down.
		return err
	}

	merged := Merge(live, incoming)
	// Indented two spaces and NOT HTML-escaped, which is what Claude Code
	// writes: `JSON.stringify(t,null,2)` followed by fsync and rename.
	// json.MarshalIndent would rewrite '&', '<' and '>' as \u0026, \u003c and
	// \u003e -- inside RawMessage values too, so a machine key holding a URL
	// with a query string comes back byte-different from what Claude Code wrote.
	// The values parse identically either way; matching the bytes is what keeps
	// a diff of the credentials file meaningful.
	data, err := marshalIndentNoEscape(merged)
	if err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}

	select {
	case <-held.Compromised():
		return fmt.Errorf("aborting the switch before writing: %w", cclock.ErrCompromised)
	default:
	}

	// THE ITEM FIRST, THE FILE SECOND, and the order is the whole of what a
	// failure between them costs. The two writes cannot be made atomic -- one is
	// a file and the other is a daemon -- but the order was never forced. This
	// way the first failure has moved NOTHING, and the second leaves a login
	// that really did change with a stale file behind it: the store nothing
	// consults while an item exists, and the one Claude Code's own combinator
	// deletes on its next successful write. The other order left the file moved
	// and the login unchanged, which is the state this whole path exists to stop
	// producing.
	if err := installIntoKeychain(itemIsLive, merged); err != nil {
		return err
	}
	if err := WriteFileAtomic(livePath, data, 0o600); err != nil {
		if itemIsLive {
			return fmt.Errorf("the keychain item Claude Code reads first now holds the new login, "+
				"but the credentials file behind it could not be replaced: %w", err)
		}
		return err
	}
	return nil
}

// installIntoKeychain mirrors what was just written into the macOS Keychain
// item -- WHEN one is already there.
//
// It is not a courtesy. On macOS the combinator Claude Code builds its
// credential store from reads the keychain FIRST and falls back to the file:
//
//	read(a){let o=e.read(a);if(o!==null&&o!==void 0)return o;return t.read(a)||{}}
//
// so where an item exists it IS the login, and the file above is a store
// nothing consults. Writing only the file is what made a switch look like it
// worked while every request kept authenticating as the account the item named.
// keychain_security.go's Write carries the measurement that settles this.
//
// AN ABSENT ITEM IS LEFT ABSENT. With no item the fallback is what gets read,
// so the file is already the login and there is nothing to mirror; creating one
// would introduce a second store where the machine had one, and make it the
// store consulted first from then on. That is a decision about how a machine
// stores its credentials, and a switch is not where it gets made.
//
// THE COST IS PAID INSIDE THE CREDENTIAL LOCKS, which is the objection this
// path was refused on before. Two spawns of `security` -- a lookup that does
// not decrypt, then the install -- sit in a window Claude Code's own refresh
// blocks on. That was worth avoiding while the item was believed inert. It is
// not worth avoiding to preserve a swap that does not swap anything.
//
// A failure is returned rather than warned about, and the caller sees a switch
// that failed. That is the honest report: the file moved and the login did not,
// which is exactly the state this whole path exists to stop producing. The two
// writes cannot be made atomic -- one is a file and the other is a daemon --
// so the choice is only which failure the caller is told about, and silence is
// what let this go unnoticed for as long as it did.
func installIntoKeychain(itemIsLive bool, merged Blob) error {
	// itemIsLive comes from the READ that produced the merge base, not from a
	// second lookup. The old lookup was ProbeCredentialKeychainItem, which
	// spawns `find-generic-password` WITHOUT -w so it can never raise an auth
	// dialog -- and therefore answers "present" for an item whose secret the
	// keychain is refusing. A switch could commit to replacing, wholesale, an
	// item it had never read. Now the item written is the item the base came
	// from by construction rather than by agreement, and a keychain that cannot
	// be read has already failed the read, before anything was written.
	if !itemIsLive {
		return nil
	}
	ctx := context.Background()
	// COMPACT, where the file is indented, and the difference is not cosmetic.
	// `security -w` -- the read Claude Code performs, and the one
	// LoadKeychainCredentials performs -- hands back HEX for any value
	// containing a newline, and both readers parse the output as JSON. An
	// indented payload therefore round-trips as hex and is unreadable to both,
	// silently: the reader's parse fails, its catch swallows it, and the
	// combinator falls through to the file as though no item existed.
	//
	// Measured on a real item: the file's indented bytes came back as 5284 hex
	// characters, where the item Claude Code had written itself came back as
	// plain JSON. marshalNoEscape is the same encoder minus the indent, so the
	// bytes still match what Claude Code writes rather than merely parsing the
	// same.
	compact, err := marshalNoEscape(merged)
	if err != nil {
		return fmt.Errorf("encoding the keychain payload: %w", err)
	}
	// CredentialKeychainItem(), not a probed candidate: it is the same name
	// LoadKeychainCredentials just read from, so the item written is the item
	// the base came from.
	if err := CredentialKeychainItem().Write(ctx, string(compact)); err != nil {
		return fmt.Errorf("the keychain item Claude Code reads first could not be updated, so nothing "+
			"was written and the login did not change: %w", err)
	}
	return nil
}
