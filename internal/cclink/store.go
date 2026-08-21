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
)

// Load reads the live credentials file. A missing file is an empty Blob and
// no error: that is a machine where Claude Code has never logged in.
func Load() (Blob, error) {
	return loadFrom(ccpath.CredentialsPath())
}

// loadFrom reads and decodes the credentials file at an already-resolved
// path. It exists so Activate can re-read from the SAME directory its locks
// cover (held.CredentialHome()) rather than a fresh call to
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

// Capture returns the account-scoped subset of the live file -- what ccdad
// stores when adopting the account Claude Code is currently logged in as.
//
// This read is NOT taken under the credential locks: spec 4.4 numbers it as
// step 1, before locking begins, and it is a separate operation from
// Activate's locked swap. If Claude Code is mid-refresh, Capture can
// snapshot a refresh token that gets rotated away microseconds later,
// producing a stored account that is dead on first use. The window is
// narrow and the failure mode is a refresh error on next use, not silent
// corruption of a live login, so the unlocked read is accepted here rather
// than paying for a lock acquisition on every `ccdad add`.
func Capture() (Blob, error) {
	live, err := Load()
	if err != nil {
		return nil, err
	}
	return Extract(live), nil
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
// The file is re-read INSIDE the lock (loadFrom held.CredentialHome(),
// never Load's fresh ccpath.CredentialsPath()): whatever was loaded before
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
// damage. This is what satisfies spec 4.4 step 7 ("on any failure after
// step 5 begins, restore from the pre-write snapshot") for a compromise: the
// step is vacuous here, because the one case with lingering state to worry
// about is exactly the case a restore would make worse. The caller gets the
// error and should re-read and verify rather than assume either side's
// state.
//
// Activate does not itself surface unrecognized top-level keys (spec 4.3's
// unknown-key probe). That is deferred to the CLI layer: it already holds
// the live Blob via Load and can call UnknownKeys directly, without widening
// this function's signature for every caller that does not need the result.
func Activate(incoming Blob) (err error) {
	// Before taking any lock: an incoming snapshot with no claudeAiOauth
	// would have every account-scoped key deleted by Merge and nothing put
	// back, silently logging the user out. A corrupt or truncated stored
	// snapshot is exactly the kind of input this must refuse up front.
	if _, ok := incoming["claudeAiOauth"]; !ok {
		return fmt.Errorf("refusing to activate a snapshot with no claudeAiOauth: it would log the user out")
	}

	held, aerr := cclock.AcquireCredentials(LockTimeout)
	if aerr != nil {
		return aerr
	}
	defer func() { err = errors.Join(err, held.Release()) }()

	livePath := filepath.Join(held.CredentialHome(), ccpath.CredentialsFile)

	live, err := loadFrom(livePath)
	if err != nil {
		return err
	}

	merged := Merge(live, incoming)
	data, err := json.MarshalIndent(merged, "", "  ")
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
