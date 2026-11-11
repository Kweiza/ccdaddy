package cclink

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ForbidRealSecurityForTest makes a spawn of the real binary FAIL LOUDLY: the
// other packages' test mains rely on it (cclinktest.Isolate) to catch a test
// that would otherwise read the developer's own keychain.
func TestASpawnOfTheRealSecurityPanicsOnceForbidden(t *testing.T) {
	fakeSecurity{}.install(t)
	realSecurityForbidden.Store(true)
	t.Cleanup(func() { realSecurityForbidden.Store(false) })

	defer func() {
		if recover() == nil {
			t.Fatal("a forbidden spawn of security did not panic")
		}
	}()
	_, _ = spawnSecurity(context.Background(), "find-generic-password", "-s", "x")
}

// The seam cclinktest plugs into. A runner is handed the bound as its context's
// deadline, and a runner that gives up at that deadline is a timed-out keychain
// call -- for a write, one whose outcome is unknown and is read back. That is
// the whole contract a fake keychain needs to stand for the 2026-11-10
// incident from another package.
func TestARunnerThatGivesUpAtItsDeadlineIsATimedOutWrite(t *testing.T) {
	var deadlineIn time.Duration
	t.Cleanup(SetSecurityRunnerForTest(func(ctx context.Context, args ...string) (SecurityAnswer, error) {
		if d, ok := ctx.Deadline(); ok {
			deadlineIn = time.Until(d)
		}
		return SecurityAnswer{}, context.DeadlineExceeded
	}))

	err := testItem.Write(context.Background(), "{}")
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) || kcErr.failure != failTimedOut {
		t.Fatalf("Write = %v, want a timed-out *KeychainError", err)
	}
	if !writeOutcomeUnknown(err) {
		t.Fatal("a write whose runner gave up at its deadline is not read back")
	}
	if deadlineIn <= 0 || deadlineIn > keychainWriteTimeout {
		t.Fatalf("the runner's deadline was %s away, want within the write bound %s", deadlineIn, keychainWriteTimeout)
	}
}
