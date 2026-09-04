// Package codexlaunch owns the per-launch secret that lets one codex process
// reach ccdad's local Codex proxy, and the evidence that says the launch is
// still running.
//
// A launch is TWO files named after the same hash of the same secret:
//
//	<root>/codex/launches/<sha256hex>.lock   0 bytes, flocked for the launcher's life
//	<root>/codex/launches/<sha256hex>.json   {"pin":…,"startedAt":…}
//
// The lock is the liveness test, and AGE IS NEVER A CRITERION. A launcher that
// dies without cleaning up leaves both files behind; the next lookup takes the
// lock the dead process was holding, learns from that alone that nobody is
// there, and removes them. A codex session a user left open for a week keeps
// its lock and stays valid, which is exactly what an expiry would have broken.
//
// The two lock modes are not interchangeable and the difference is measured
// rather than stylistic. The LAUNCHER takes an EXCLUSIVE lock and holds it for
// its child's whole life. Every PROBE takes a SHARED one, so that probes never
// contend with each other and a refused try-lock can only mean a launcher.
//
// The secret is treated as public within the session: a same-uid process can
// read it out of the launcher's environment. What it authorises is one route on
// a loopback listener for one launch's pin or the serving account, and nothing
// else — no OAuth token is reachable through it.
package codexlaunch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/winerr"
)

const (
	// secretBytes is the entropy in one launch secret, before hex encoding.
	secretBytes = 32

	// filePerm and dirPerm match the rest of the ccdad store. chmod is a no-op
	// on Windows, so nothing may depend on them there.
	filePerm = 0o600
	dirPerm  = 0o700
)

// Dir is where a store's launch records live.
func Dir(root string) string { return filepath.Join(root, "codex", "launches") }

// Record is the document beside the lock.
type Record struct {
	// Pin is the account uuid this launch is bound to, or "" for a launch that
	// follows the serving pointer.
	Pin string `json:"pin"`
	// StartedAt is when the launcher created the record. It is informational:
	// nothing reads it to decide whether a launch is alive.
	StartedAt time.Time `json:"startedAt"`
}

// Launch is one live launch, held by the process that started codex.
type Launch struct {
	secret   string
	lock     *flock.Flock
	lockPath string
	jsonPath string
}

// Create mints a secret, takes the lock and writes the record.
//
// The returned Launch must be held for the child's whole life and closed after
// it exits: releasing the lock is what tells the proxy the launch is over.
func Create(root, pin string) (*Launch, error) {
	dir := Dir(root)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("creating the codex launch directory: %w", err)
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generating the codex launch secret: %w", err)
	}
	secret := hex.EncodeToString(raw)
	h := hashOf(secret)
	lockPath := filepath.Join(dir, h+".lock")
	jsonPath := filepath.Join(dir, h+".json")

	// The lock file is brought into existence HERE rather than by flock,
	// because every other open in this package deliberately drops O_CREATE so
	// that a missing file answers ENOENT instead of being created by a reader.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("creating the codex launch lock: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("creating the codex launch lock: %w", err)
	}

	// EXCLUSIVE, and it is the only exclusive lock this package takes. It is
	// what every shared probe in lookupHash is asking about: a probe that is
	// refused has been refused by a launcher, because nothing else here holds
	// this file any way but shared.
	fl := flock.New(lockPath, flock.SetFlag(os.O_RDWR))
	locked, err := fl.TryLock()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("locking the codex launch record: %w", err), removeIfPresent(lockPath))
	}
	if !locked {
		// TryLock reports contention as (false, nil), never as an error. The
		// name is 32 fresh random bytes, so a holder here is not a collision:
		// it means something is writing this directory that ccdad cannot
		// account for.
		return nil, errors.Join(errors.New("a freshly generated codex launch name is already locked"), removeIfPresent(lockPath))
	}

	data, err := json.Marshal(Record{Pin: pin, StartedAt: time.Now()})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("encoding the codex launch record: %w", err), fl.Unlock(), removeIfPresent(lockPath))
	}
	if err := atomicfile.WriteFile(jsonPath, data, filePerm); err != nil {
		return nil, errors.Join(fmt.Errorf("writing the codex launch record: %w", err), fl.Unlock(), removeIfPresent(lockPath))
	}
	// The method value keeps the *Flock reachable for as long as the Launch is,
	// which matters: os.File carries a finalizer that closes the descriptor,
	// and flock releases on the last close of the open file description.
	return &Launch{secret: secret, lock: fl, lockPath: lockPath, jsonPath: jsonPath}, nil
}

// Secret is the value the launcher puts in the child's environment.
func (l *Launch) Secret() string { return l.secret }

// Close releases the lock and removes both files.
//
// The unlock comes FIRST: Windows refuses to delete a file another handle holds
// a lock on, so deleting first would strand the lock file there and make every
// later lookup pay to reap it.
func (l *Launch) Close() error {
	if l == nil {
		return nil
	}
	return errors.Join(l.lock.Unlock(), removeIfPresent(l.jsonPath), removeIfPresent(l.lockPath))
}

// LookupResult is what a bearer turned out to be.
type LookupResult int

const (
	// Valid: the record exists and its launcher still holds the lock.
	Valid LookupResult = iota
	// Unknown: no record under this bearer's hash.
	Unknown
	// Dead: the record exists and nothing holds the lock, so the launcher is
	// gone. Lookup removes both files before returning it.
	Dead
)

// String names a result, so a failing test reads as a sentence rather than as
// an integer.
func (r LookupResult) String() string {
	switch r {
	case Valid:
		return "valid"
	case Unknown:
		return "unknown"
	case Dead:
		return "dead"
	}
	return "unrecognised"
}

// Lookup answers for one bearer.
//
// It is safe to call from any number of goroutines at once, including about the
// same bearer, which is not a courtesy: the proxy calls it per request and the
// daemon's sweep calls it for every record on every tick.
func Lookup(root, bearer string) (Record, LookupResult, error) {
	if bearer == "" {
		return Record{}, Unknown, nil
	}
	return lookupHash(Dir(root), hashOf(bearer))
}

// Reap sweeps the whole directory and reports how many dead records it removed.
//
// It exists because a Lookup only ever reaches the record a live codex quotes:
// a machine that ran ten codex sessions and lost all ten to a reboot would
// otherwise keep ten pairs of files forever.
func Reap(root string) (int, error) {
	dir := Dir(root)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the codex launch directory: %w", err)
	}
	reaped := 0
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		_, res, err := lookupHash(dir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			errs = append(errs, err)
		}
		if res == Dead {
			reaped++
		}
	}
	return reaped, errors.Join(errs...)
}

// lookupHash is the whole liveness rule, in one place, so Lookup and Reap
// cannot answer differently about the same pair of files.
func lookupHash(dir, h string) (Record, LookupResult, error) {
	jsonPath := filepath.Join(dir, h+".json")
	lockPath := filepath.Join(dir, h+".lock")

	data, err := os.ReadFile(jsonPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, Unknown, nil
	}
	if err != nil {
		return Record{}, Unknown, fmt.Errorf("reading a codex launch record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		// A record nothing can parse authorises nothing, and leaving it would
		// make every request pay to fail on it. It is treated as dead.
		return Record{}, Dead, errors.Join(removeIfPresent(jsonPath), removeIfPresent(lockPath))
	}

	// The probe takes a SHARED lock, and that is the correctness of this
	// function rather than an optimisation. An exclusive probe contends with
	// every OTHER PROBE as well as with the launcher, and a contended try-lock
	// is indistinguishable from a held one — so two requests arriving together
	// under the same DEAD bearer would have one of them answered Valid, which
	// authorises a launch that no longer exists and hands back the account its
	// record pinned. Measured here, 200 rounds of two concurrent lookups of one
	// planted dead record: an exclusive probe answered Valid on every run and
	// never fewer than 13 times out of 400, a shared probe none. The count
	// itself moves with what else the machine is doing -- 13 to 44 across nine
	// runs here, higher again under -race -- so it is the ZERO that is the
	// property, and the test asserts that rather than a number.
	// internal/daemon/singleton.go
	// measured the same thing about its own probe with this same library —
	// sixteen concurrent exclusive probers against a free lock produced 2059
	// false "running" answers out of 8000, and the same run with shared locks
	// produced none.
	//
	// The reverse direction is what makes a shared probe an answer at all: it
	// still fails against the EXCLUSIVE lock Create holds for the launcher's
	// whole life, which is the only question being asked here.
	fl := flock.New(lockPath, flock.SetFlag(os.O_RDWR))
	locked, err := fl.TryRLock()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A record with no lock beside it: nothing is holding this launch open.
		return Record{}, Dead, errors.Join(removeIfPresent(jsonPath), removeIfPresent(lockPath))
	case err != nil:
		return Record{}, Unknown, fmt.Errorf("locking a codex launch record: %w", err)
	case !locked:
		// The try-lock FAILED, and that failure is the evidence. A SHARED
		// request can only be refused by an EXCLUSIVE holder, other probes take
		// shared locks too and so never block one another, and the only
		// exclusive holder this package ever creates is the launcher.
		return rec, Valid, nil
	}
	// It was free, so the launcher is gone.
	//
	// Two probes of the same dead record can reach this line together and both
	// remove the same two files. That was decided to be harmless rather than
	// guarded, and the reason is that the ANSWER is decided by the lock and
	// never by whether the unlink succeeded: removeIfPresent reports nothing
	// for a removal that lost the race, and a probe that reads the record after
	// the winner deleted it answers Unknown, which refuses exactly as Dead
	// does. A guard would have to be a second lock over the pair of files,
	// bought to make two harmless answers identical.
	return Record{}, Dead, errors.Join(fl.Unlock(), removeIfPresent(jsonPath), removeIfPresent(lockPath))
}

// hashOf is the record name for a secret.
func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// removeIfPresent deletes path. A removal that lost a race to another probe of
// the same dead launch is not a failure, and it arrives in two shapes, one per
// operating system.
//
// ENOENT is the Unix shape: the other probe removed the file first. The Windows
// shape is the opposite handshake — Go's open passes
// FILE_SHARE_READ|FILE_SHARE_WRITE and NOT FILE_SHARE_DELETE, so the other
// probe's still-open handle on the lock file blocks this delete outright and it
// fails with a sharing violation instead. winerr.Retryable names that errno set
// and is false on every other platform, so nothing off Windows changes shape
// here.
//
// Neither is retried, and neither is reported. The removal is garbage
// collection, not the answer: a record left behind authorises nothing, because
// every probe that reads it finds the lock free and refuses, and Reap collects
// whatever survives the next time the directory is swept. Turning a lost race
// into an error would instead put a spurious failure on the request path, where
// the proxy logs it and refuses a launch it was already going to refuse.
//
// That is deliberately NOT what atomicfile does with the same predicate, and
// the difference is what is at stake rather than a preference. A rename that
// gives up LOSES the write, so atomicfile spends ten attempts and up to 250ms
// of backoff to keep it. An unlink that gives up loses nothing, and this one
// runs while a codex request waits.
func removeIfPresent(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) || winerr.Retryable(err) {
		return nil
	}
	return fmt.Errorf("removing a codex launch file: %w", err)
}
