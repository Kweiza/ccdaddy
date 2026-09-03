// Package codexswitch repoints the account ccdad's codex proxy serves.
//
// It is a package of its own, and a very small one, so that a machine-checkable
// statement can be made about it: its dependency closure contains neither
// internal/switcher nor internal/cclink nor internal/store, so nothing reachable
// from here can install a Claude Code credential, rewrite the credentials file,
// or read one. That is why Execute takes a uuid STRING rather than a
// store.Account -- taking the account would put the store in the closure and
// cost the guarantee for a convenience.
//
// A Codex "switch" is not a credential swap at all. Codex holds no token on
// this machine; the daemon's proxy holds them and rewrites the bearer per
// request. So all a switch does is move a pointer, and the pointer is read on
// every request rather than cached, which is what makes a repoint apply to new
// threads without disturbing threads already in flight.
package codexswitch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// ErrPointerMovedUnstamped marks an Execute failure that happened AFTER the
// pointer already moved: the proxy is serving the new account, but the
// anti-flap cooldown for that switch was not recorded, so a poll tick shortly
// after can re-switch immediately with nothing holding it back. A caller that
// sees an error satisfying errors.Is(err, ErrPointerMovedUnstamped) cannot
// conclude the repoint did not happen -- it must re-read ReadServing to learn
// what is actually being served.
var ErrPointerMovedUnstamped = errors.New("codexswitch: the pointer moved but the switch cooldown was not recorded")

// dirName and fileName are the two path components, spelled once.
const (
	dirName  = "codex"
	fileName = "serving"
)

// ServingPath is where the pointer lives under a ccdad store root.
func ServingPath(root string) string {
	return filepath.Join(root, dirName, fileName)
}

// ReadServing is the account codex is served from, and whether there is one.
//
// Every failure answers "no pointer" rather than an error, and that is the
// right degradation for this file: the reader is a request handler, and a
// pointer that cannot be read means the same thing to it as a pointer that was
// never written -- fall through to the top-ranked eligible account. An account
// named by a pointer that no longer exists is the caller's to notice; this
// function only reports what the file says.
func ReadServing(root string) (string, bool) {
	raw, err := os.ReadFile(ServingPath(root))
	if err != nil {
		return "", false
	}
	uuid := strings.TrimSpace(string(raw))
	if uuid == "" {
		return "", false
	}
	return uuid, true
}

// Execute repoints codex at one account.
//
// TWO CRITICAL SECTIONS, IN THIS ORDER, and the order is the whole of the
// correctness here. The pointer is written first, because it is the thing that
// changes what the machine spends; the cooldown stamp is written only after
// that succeeded, because a cooldown earned by a repoint that did not happen
// would hold the lane off the retry that would have made it happen.
//
// The pointer write is atomic -- a sibling temp file and a rename -- so the
// proxy, which reads this file on every request, sees one whole uuid or the
// previous one, never a torn line.
//
// A non-nil error does NOT mean the repoint did not happen. The pointer is
// written first and the stamp only after, on purpose (see above), so a
// failure in the stamp step returns an error while the pointer has already
// moved: the proxy is already serving uuid, and the cooldown that would hold
// the lane off an immediate re-switch was never recorded. That case wraps
// ErrPointerMovedUnstamped. A caller that needs to know what is actually
// being served on any error must re-read ReadServing rather than infer it
// from Execute's return value.
//
// What it deliberately does NOT do: store.SetActive, switcher.RecordSwitch,
// releaseManagedAPIKey, SyncGlobalConfigIdentity. None of them is reachable
// from this package, which is asserted rather than remembered.
func Execute(root, uuid string) error {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return fmt.Errorf("codexswitch: no account named")
	}
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := atomicfile.WriteFile(ServingPath(root), []byte(uuid+"\n"), 0o600); err != nil {
		return fmt.Errorf("repointing codex at %s: %w", uuid, err)
	}
	if err := strategy.RecordCodexSwitch(uuid); err != nil {
		return fmt.Errorf("%w: %w", ErrPointerMovedUnstamped, err)
	}
	return nil
}

// Clear removes the pointer. `ccdad remove` calls it for the account that was
// serving, and calling it on a machine with no pointer is not a failure --
// nothing asks first.
func Clear(root string) error {
	if err := os.Remove(ServingPath(root)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing the codex serving pointer: %w", err)
	}
	return nil
}
