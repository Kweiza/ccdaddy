package credhome

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// OwnerSchemaVersion is the version stamped into every document this binary
// writes.
//
// The contract is ADDITIVE, as status.json's is: fields are added, never
// repurposed or removed, so a reader of any vintage can read a document of any
// vintage by ignoring what it does not recognise. A version NEWER than this one
// is therefore accepted rather than refused — the fields this binary knows
// still mean what they meant. A version of zero is not: that is a document
// nothing here wrote, and reading a store path out of it would name an engine
// that does not exist.
//
// It is a constant of its own rather than daemon.StatusSchemaVersion, which
// describes an unrelated document. Sharing one number would make either
// document's evolution a lie about the other's.
const OwnerSchemaVersion = 1

// Owner is the engine holding the claim.
//
// It is never liveness evidence. The lock is, and this document is read only
// once the lock has been observed held — so a document left behind by a crashed
// engine is never consulted. Same doctrine as the pidfile's recorded pid, and
// for the same reason: no staleness heuristic can be right about a process, and
// the kernel already knows.
type Owner struct {
	SchemaVersion int `json:"schemaVersion"`
	// Store is the CCDAD_HOME the holding engine runs against, absolute. It is
	// what makes the "is this me" question answerable at all: two engines are
	// only in conflict when their STORES differ.
	Store string `json:"store"`
	// PID is informational, and is never used to decide anything. The process
	// may have died and the number may have been recycled onto something
	// unrelated; it is here because it is the first thing an operator looks for.
	PID int `json:"pid"`
	// ClaimedAt is when the claim was taken.
	ClaimedAt time.Time `json:"claimedAt,omitzero"`
}

// commitMarker is the trailing byte that makes an owner document complete.
//
// It earns its keep because the write is truncate-then-write rather than a
// temp file and a rename: with a rename there is no torn state for a reader to
// catch and the marker would be dead code. Both halves stay together, exactly
// as pidfile.go says of its own.
//
// A rename would also be wrong here for a reason the pidfile does not have. The
// window this document is read in is the one where a NEW holder has the lock
// and has not written yet, and truncate-then-write makes that window read as
// "held, not yet named" — which is a state the callers handle — where a rename
// would leave the PREVIOUS holder's document intact and readable, naming an
// engine that is no longer driving anything.
const commitMarker = '\n'

// writeOwner records the holder, in place, as truncate-then-write with a
// trailing newline.
//
// The truncation is O_TRUNC at open, which is the same instant the write
// begins. Truncating in a separate call first would not shorten the window in
// which no holder can be named — it would double it.
func writeOwner(path string, o Owner) error {
	body, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("encoding the credential-home owner: %w", err)
	}
	body = append(body, commitMarker)
	if err := os.WriteFile(path, body, filePerm); err != nil {
		return fmt.Errorf("writing %s: %w", OwnerFileName, err)
	}
	return nil
}

// clearOwner empties the owner document without removing it.
//
// Removal is never correct, for the same reason ClearPID does not unlink the
// pidfile: an absent document beside a lock file that exists is a state nothing
// else produces, and forging it would tell the next reader something untrue. A
// zero-byte document says "the claim is free, and an engine was here", which is
// what actually happened.
func clearOwner(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("clearing %s: %w", OwnerFileName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("clearing %s: %w", OwnerFileName, err)
	}
	return nil
}

// readOwner reports who holds the claim.
//
// named is false, with no error, for every state that is a legitimate "there is
// nothing to read here":
//
//   - the file is absent — no engine has written one, which on a lock that IS
//     held means one is starting right now;
//   - the file is zero bytes — the truncate half of a write in flight, or the
//     document a departing engine cleared;
//   - the body has no trailing newline — the half-written prefix of a write.
//
// A body that IS committed and does not make sense returns an error AND
// named=false, and both halves matter. Unlike the pidfile, corruption here
// cannot hide liveness — the lock already answered that — so it must not become
// a refusal; it costs the caller a NAME and nothing else. But `ccdad doctor` is
// the reader that has to see it, so it is not swallowed either.
func readOwner(path string) (o Owner, named bool, err error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Owner{}, false, nil
		}
		return Owner{}, false, fmt.Errorf("reading %s: %w", OwnerFileName, err)
	}
	// The length check is not defensive tidiness: it is the zero-byte case,
	// which indexing the last byte would panic on.
	if len(body) == 0 || body[len(body)-1] != commitMarker {
		return Owner{}, false, nil
	}
	var got Owner
	if err := json.Unmarshal(body[:len(body)-1], &got); err != nil {
		return Owner{}, false, fmt.Errorf("%s is committed but does not parse: %w", OwnerFileName, err)
	}
	// A missing or zero version means this document was not written by any
	// ccdad. A HIGHER one is accepted: the additive contract says the fields
	// below still mean what they mean.
	if got.SchemaVersion < OwnerSchemaVersion {
		return Owner{}, false, fmt.Errorf(
			"%s carries schema version %d, which no ccdad wrote", OwnerFileName, got.SchemaVersion)
	}
	if got.Store == "" {
		return Owner{}, false, fmt.Errorf("%s names no store, so the engine holding the claim cannot be identified", OwnerFileName)
	}
	return got, true, nil
}
