package cclink

import (
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

// Load reads the live credentials file. A missing file is an empty Blob and
// no error: that is a machine where Claude Code has never logged in.
func Load() (Blob, error) {
	path, err := ccpath.CredentialsPath()
	if err != nil {
		return nil, err
	}
	return loadFrom(path)
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

	live, err := loadFrom(livePath)
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

	if err := WriteFileAtomic(livePath, data, 0o600); err != nil {
		return err
	}
	return nil
}
