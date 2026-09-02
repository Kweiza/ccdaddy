package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// deliverAt builds a watch that reports a signal at a chosen point.
//
// The two points are the two sides of the commit, and the fake reaches them the
// way the real one does rather than by setting a flag. before puts the signal
// in the channel up front, so mutate's poll between fn and the save finds it;
// onClose puts it there from unhook, which is exactly what signal.Stop's
// quiesce guarantees for a signal that was raised while the save was running —
// the poll cannot see it and the drain after Stop must.
func deliverAt(before, onClose bool) func() *interruptWatch {
	return func() *interruptWatch {
		ch := make(chan os.Signal, 1)
		if before {
			ch <- os.Interrupt
		}
		return &interruptWatch{
			ch: ch,
			unhook: func() {
				if onClose {
					select {
					case ch <- os.Interrupt:
					default:
					}
				}
			},
		}
	}
}

// TestCloseObservesASignalThatArrivesDuringUnhook pins close()'s own contract
// directly, rather than through mutate. mutate's defer calls close() a second
// time no matter what the explicit call at the commit point returned, and that
// redundancy is real: it means a reordering inside close() alone does not
// change mutate's OUTPUT, because the second call papers over the first one's
// mistake. It would still be a bug in close() as a primitive — signal.Stop's
// own quiesce guarantee is that a signal already being routed to the channel is
// delivered before Stop returns, and a drain taken before that call misses
// exactly the signal the guarantee exists to catch. This test is what makes
// that promise close()'s own, checkable without mutate's second chance
// standing in front of it.
//
// The fake's unhook is where the signal becomes visible, which is what a real
// signal arriving mid-quiesce looks like from close()'s point of view: absent
// when asked, present once the call that is supposed to wait for it returns.
func TestCloseObservesASignalThatArrivesDuringUnhook(t *testing.T) {
	ch := make(chan os.Signal, 1)
	w := &interruptWatch{ch: ch, unhook: func() {
		select {
		case ch <- os.Interrupt:
		default:
		}
	}}

	if got := w.close(); !got {
		t.Fatal("close() = false for a signal that arrived during unhook, want true: " +
			"draining before unhooking discards exactly the delivery signal.Stop's quiesce exists to catch")
	}
}

// TestASignalMidTransactionTakesTheCredentialFileBackOffTheDisk is the defect
// this guard exists to close, in the shape a user meets it: Ctrl-C partway
// through a multi-account import, with the first credential file already down
// and accounts.toml not yet saved.
func TestASignalMidTransactionTakesTheCredentialFileBackOffTheDisk(t *testing.T) {
	dir := withStore(t)
	t.Cleanup(setWatchInterruptForTest(deliverAt(true, false)))

	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		return s.Add(Account{Provider: provider.Claude, UUID: "u-2"}, sampleCreds("AT-2"))
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("WithStore() = %v, want ErrInterrupted", err)
	}
	if got := err.Error(); !strings.Contains(got, "reversed") {
		t.Errorf("the message for an interrupt BEFORE the save says %q; it has to say the change did not stand", got)
	}

	for _, uuid := range []string{"u-1", "u-2"} {
		leaked := filepath.Join(dir, credentialsDir, uuid+".json")
		if _, statErr := os.Stat(leaked); !os.IsNotExist(statErr) {
			t.Errorf("an interrupted transaction left a live refresh token at %s that no account names", leaked)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, accountsFile)); !os.IsNotExist(statErr) {
		t.Error("an interrupted transaction wrote the document it was interrupted before writing")
	}

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.Accounts()); got != 0 {
		t.Errorf("the store holds %d accounts after an interrupted transaction, want 0", got)
	}
}

// TestASignalAfterTheSaveLeavesTheWriteStanding is the other side of the same
// commit, and the reason mutate polls between fn and the save rather than once
// at the end. The document is written; reversing then would delete credential
// files accounts.toml now names, which is the switch-logs-you-out failure the
// reversal was built to prevent, caused by the reversal.
func TestASignalAfterTheSaveLeavesTheWriteStanding(t *testing.T) {
	dir := withStore(t)
	t.Cleanup(setWatchInterruptForTest(deliverAt(false, true)))

	err := WithStore(func(s *Store) error {
		return s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1"))
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("WithStore() = %v, want ErrInterrupted: a signal this code held may not be dropped", err)
	}
	if got := err.Error(); !strings.Contains(got, "IS saved") {
		t.Errorf("the message for an interrupt AFTER the save says %q; it has to say the change stands", got)
	}

	kept := filepath.Join(dir, credentialsDir, "u-1.json")
	if _, statErr := os.Stat(kept); statErr != nil {
		t.Errorf("the credentials of a COMMITTED transaction were removed by the interrupt: %v", statErr)
	}
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("u-1"); !ok {
		t.Error("a committed transaction was reversed because the signal arrived after it committed")
	}
}

// TestASignalOnAnAlreadyFailingTransactionIsStillReported covers the path the
// two explicit returns do not: the transaction was going to fail anyway. The
// run still took SIGINT's default disposition away, so it still owes the user a
// sentence about it — and the callback's own error is not swallowed to make
// room for that.
func TestASignalOnAnAlreadyFailingTransactionIsStillReported(t *testing.T) {
	dir := withStore(t)
	t.Cleanup(setWatchInterruptForTest(deliverAt(true, false)))

	boom := errors.New("boom")
	err := WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("WithStore() = %v, want the callback's own error to survive", err)
	}
	if !errors.Is(err, ErrInterrupted) {
		t.Errorf("WithStore() = %v, want ErrInterrupted alongside it: the run was stopped and said nothing about it", err)
	}
	leaked := filepath.Join(dir, credentialsDir, "u-1.json")
	if _, statErr := os.Stat(leaked); !os.IsNotExist(statErr) {
		t.Errorf("the reversal did not run at %s", leaked)
	}
}

// TestTheWatchOutlivesTheReversal pins the defer ORDER, which is the one
// property of mutate that no other test here would notice breaking. A process
// killed halfway through rollback leaves exactly the half-reversed credentials
// directory rollback exists to prevent, so the watch has to outlive the
// reversal — which in Go means its close is registered BEFORE the reversal's
// defer, since defers run last-registered-first.
//
// The order is read off the filesystem rather than out of a counter: unhook
// records whether the credential file was still there when it ran. Still there
// means the watch was dropped first and the reversal ran unprotected.
func TestTheWatchOutlivesTheReversal(t *testing.T) {
	dir := withStore(t)
	leaked := filepath.Join(dir, credentialsDir, "u-1.json")

	unhooked := false
	fileWasStillThere := false
	t.Cleanup(setWatchInterruptForTest(func() *interruptWatch {
		ch := make(chan os.Signal, 1)
		ch <- os.Interrupt
		return &interruptWatch{ch: ch, unhook: func() {
			unhooked = true
			_, statErr := os.Stat(leaked)
			fileWasStillThere = statErr == nil
		}}
	}))

	err := WithStore(func(s *Store) error {
		return s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1"))
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("WithStore() = %v, want ErrInterrupted", err)
	}
	if !unhooked {
		t.Fatal("the watch was never unhooked, so SIGINT stays trapped for the life of the process")
	}
	if _, statErr := os.Stat(leaked); !os.IsNotExist(statErr) {
		t.Fatalf("the reversal did not run at %s", leaked)
	}
	if fileWasStillThere {
		t.Error("the watch was dropped before the reversal ran, so a second Ctrl-C kills the process " +
			"mid-rollback and leaves the credential file this transaction created")
	}
}

// TestASuccessfulTransactionUnhooksItsWatch is the other half of the
// narrowing: the span has to END, and it has to end on the path that does not
// go anywhere near an error. A watch left registered by every ordinary write is
// the process-wide trap root.go refuses, arrived at one transaction at a time.
func TestASuccessfulTransactionUnhooksItsWatch(t *testing.T) {
	withStore(t)

	unhooked := 0
	t.Cleanup(setWatchInterruptForTest(func() *interruptWatch {
		ch := make(chan os.Signal, 1)
		return &interruptWatch{ch: ch, unhook: func() { unhooked++ }}
	}))

	if err := WithStore(func(s *Store) error {
		return s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1"))
	}); err != nil {
		t.Fatal(err)
	}
	if unhooked != 1 {
		t.Fatalf("a successful transaction unhooked its watch %d times, want exactly 1", unhooked)
	}
}

// TestANestedMutatorTakesNoWatchOfItsOwn keeps the span to one per
// transaction. `ccdad import` calls Add, SetAlias, SetDisabled and SetPrimary
// per account inside one WithStore, and every one of them reaches mutate: a
// watch per mutator would register and unregister SIGINT dozens of times inside
// a single write, and each unregister is a window in which a Ctrl-C kills the
// process with credential files already down.
func TestANestedMutatorTakesNoWatchOfItsOwn(t *testing.T) {
	withStore(t)

	watches := 0
	t.Cleanup(setWatchInterruptForTest(func() *interruptWatch {
		watches++
		return &interruptWatch{ch: make(chan os.Signal, 1), unhook: func() {}}
	}))

	if err := WithStore(func(s *Store) error {
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
			return err
		}
		if err := s.Add(Account{Provider: provider.Claude, UUID: "u-2"}, sampleCreds("AT-2")); err != nil {
			return err
		}
		return s.SetAlias("u-1", "one")
	}); err != nil {
		t.Fatal(err)
	}
	if watches != 1 {
		t.Fatalf("one transaction took %d watches, want exactly 1", watches)
	}
}

// TestAPanicInATransactionStillTakesTheCredentialFileBack is the exit a defer
// reaches and an error return never did. It is not the user-facing case — that
// is the signal — but it is the one that says the reversal belongs to LEAVING
// mutate rather than to the two returns that used to carry it, which is what
// makes the next early return someone adds safe by construction.
func TestAPanicInATransactionStillTakesTheCredentialFileBack(t *testing.T) {
	dir := withStore(t)
	leaked := filepath.Join(dir, credentialsDir, "u-1.json")

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate; mutate swallowed it")
			}
		}()
		_ = WithStore(func(s *Store) error {
			if err := s.Add(Account{Provider: provider.Claude, UUID: "u-1"}, sampleCreds("AT-1")); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if _, statErr := os.Stat(leaked); !os.IsNotExist(statErr) {
		t.Errorf("a transaction that panicked left a live refresh token at %s that no account names", leaked)
	}
}
