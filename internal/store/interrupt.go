package store

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

// The store transaction's signal guard.
//
// WHAT IT IS FOR. add writes a credential file before the document that names
// it is saved, and rollback pays that back — but only from a return. A process
// that never returns leaves the file with no account naming it, holding a live
// refresh token at 0600, invisible to `ccdad list`, `ccdad remove` and every
// account row `ccdad doctor` prints, because all of them read accounts.toml.
// The ordinary way to reach that state is Ctrl-C partway through a
// multi-account `ccdad import` or `ccdad bootstrap`.
//
// This does not FIX that. Nothing can: SIGKILL and a power cut take the process
// out between any two instructions, and doctor's credential-files row exists to
// report the residue. What this does is narrow it, and the size of the
// narrowing is the decision the item this closes asked to be made first. So:
//
// TRAPPED: SIGINT, for the span of one transaction's write and no longer.
//
// NOT TRAPPED: SIGTERM and SIGHUP, deliberately, on two grounds rather than by
// oversight. The exit contract in internal/cli/exit.go has exactly one code for
// this class — 130, which means SIGINT — so a run that died on SIGTERM and
// reported 130 would be lying about which signal it got, and adding a code is a
// change to a contract that says every command uses these and only these. And
// the process that SIGTERM is actually aimed at, the daemon, already holds all
// three for its whole life (internal/daemon/daemon.go) and therefore already
// cannot be killed abruptly mid-transaction by any of them. What is left
// uncovered is `docker stop` landing inside a `ccdad bootstrap` — named here
// rather than left for someone to discover, and closable later by the same
// mechanism once the exit contract has an answer for it.
//
// WHY THIS IS NOT THE PROCESS-WIDE TRAP root.go REFUSES. root.go's reason is
// duration and reach: a trap installed for the life of the process strips
// SIGINT's default disposition from every command while almost none of them
// watch a context, which is how Ctrl-C became a no-op on `switch` waiting for a
// credential lock and on `add-token` blocked on stdin. This trap is installed
// AFTER the store lock is granted and removed before mutate returns, and every
// WithStore callback in the tree is disk-only — it has to be, the cross-process
// lock is held across it — so the span is a handful of file writes. Inside it
// SIGINT does not become a no-op either: it becomes a reversal and exit 130.
//
// THE ONE RULE, and it is the whole reason the shape below is not simpler: a
// signal whose default disposition this code removed is ALWAYS acted on, never
// dropped. Removing the default and then discarding what arrived is exactly the
// no-op Ctrl-C root.go is written against, one window smaller. So the watch is
// drained after signal.Stop rather than before — Stop guarantees no further
// sends once it returns, which makes that final drain complete — and a signal
// that arrives after the transaction has already committed still surfaces,
// as an error that says the write landed. See mutate.

// ErrInterrupted means a signal arrived while a store transaction was writing.
//
// It is a sentinel for two readers. internal/cli maps it to ExitInterrupted, so
// an interrupted `ccdad import` exits 130 like every other Ctrl-C in this
// binary rather than 1; and a caller that wants to distinguish "the user
// stopped this" from "the disk failed" has something to match on. Both messages
// wrapping it say which side of the commit the signal landed on, because the
// answer to "is my change saved" is opposite on the two sides.
var ErrInterrupted = errors.New("interrupted")

// interruptedErr is the sentence for a signal, on whichever side of the commit
// it landed.
//
// The two are never collapsed into one message. They are opposite answers to
// the only question the user actually has — is my change saved — and a single
// hedged sentence covering both would leave a person who pressed Ctrl-C during
// `ccdad import` with no way to know whether to run it again.
func interruptedErr(committed bool) error {
	if committed {
		return fmt.Errorf(
			"%w: this run was stopped just as the store write finished, so the change IS saved", ErrInterrupted)
	}
	return fmt.Errorf(
		"%w: this run was stopped while it was writing the store, so the change was reversed and nothing was saved",
		ErrInterrupted)
}

// interruptWatch is one transaction's registration on SIGINT.
//
// It is a value rather than a package-level flag because two transactions can
// exist in one process — the daemon's tick loop and a foreground command share
// nothing but the cross-process lock — and a shared flag would let one
// transaction's Ctrl-C reverse another's write.
type interruptWatch struct {
	ch      <-chan os.Signal
	unhook  func()
	stopped bool
	got     bool
}

// seen reports whether a signal has arrived so far.
//
// It latches: the channel is drained into got, so a second call answers the
// same as the first even though the channel is now empty. mutate asks twice —
// once before the save and once after the commit — and a poll that consumed the
// only evidence would make the second question unanswerable.
func (w *interruptWatch) seen() bool {
	select {
	case <-w.ch:
		w.got = true
	default:
	}
	return w.got
}

// close unregisters the watch and reports whether a signal arrived at any point
// during it.
//
// The drain happens AFTER unhooking, and that order is the whole correctness of
// this function: signal.Stop guarantees no further sends once it returns, so
// what is in the channel at that moment is everything that was ever delivered.
// Draining first would leave a window in which a signal arrives, is queued, and
// is then discarded by Stop with nobody having looked at it.
//
// It is idempotent so that mutate can both defer it — which is what keeps the
// watch in place across rollback, where being killed halfway is the leak this
// file exists to make rarer — and call it explicitly at the commit point, where
// its answer decides the return value.
func (w *interruptWatch) close() bool {
	if !w.stopped {
		w.unhook()
		w.stopped = true
	}
	return w.seen()
}

// defaultWatchInterrupt takes SIGINT's default disposition away for the span of
// the returned watch.
//
// The channel is buffered by one for the reason signal.Notify's own
// documentation gives: it does not block sending, so an unbuffered channel with
// nobody selecting on it drops the signal outright, which is this file's one
// rule broken at the first line.
func defaultWatchInterrupt() *interruptWatch {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return &interruptWatch{ch: ch, unhook: func() { signal.Stop(ch) }}
}

// watchInterruptMu guards watchInterruptImpl, for the reason tryLockMu guards
// tryLockImpl: the daemon reaches store mutators from a background goroutine,
// and swapping a package-level function value out from under a concurrent
// reader is a data race under the Go memory model however much time separates
// the two.
var (
	watchInterruptMu   sync.RWMutex
	watchInterruptImpl = defaultWatchInterrupt
)

func watchInterrupt() *interruptWatch {
	watchInterruptMu.RLock()
	impl := watchInterruptImpl
	watchInterruptMu.RUnlock()
	return impl()
}

// setWatchInterruptForTest replaces the watch constructor and returns a
// function restoring the previous one. It exists so a test can describe a
// signal landing at a chosen point inside a transaction — before the save,
// during it, after the commit — which a test that raised a real SIGINT could
// only do by racing the scheduler. Production code must never call it.
//
// newWatch is invoked once per transaction, exactly as the real constructor is.
func setWatchInterruptForTest(newWatch func() *interruptWatch) (restore func()) {
	watchInterruptMu.Lock()
	prev := watchInterruptImpl
	watchInterruptImpl = newWatch
	watchInterruptMu.Unlock()
	return func() {
		watchInterruptMu.Lock()
		watchInterruptImpl = prev
		watchInterruptMu.Unlock()
	}
}
