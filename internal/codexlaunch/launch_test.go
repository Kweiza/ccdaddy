package codexlaunch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// withRoot gives one test its own store root. This package takes the root as an
// argument rather than resolving it, so nothing here touches the environment.
func withRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ccdad")
}

// plant writes the two files of a launch that nothing holds open — exactly what
// a launcher killed with SIGKILL leaves behind.
func plant(t *testing.T, root, secret, pin string) (lockPath, jsonPath string) {
	t.Helper()
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := hashOf(secret)
	lockPath = filepath.Join(dir, h+".lock")
	jsonPath = filepath.Join(dir, h+".json")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(Record{Pin: pin, StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return lockPath, jsonPath
}

func TestALaunchIsValidWhileItIsOpen(t *testing.T) {
	root := withRoot(t)
	l, err := Create(root, "uuid-pinned")
	if err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	// 64, written as a literal rather than as 2*secretBytes. An assertion
	// computed from the constant it is checking moves with that constant, so
	// shrinking the secret shrinks the test with it: measured, setting
	// secretBytes to 4 left every other assertion in this file green. The
	// number that matters is not the implementation's, it is the 32 bytes of
	// entropy this bearer is required to carry.
	if len(l.Secret()) != 64 {
		t.Fatalf("Secret() is %d characters, want 64 hex characters for a 32-byte secret", len(l.Secret()))
	}
	rec, res, err := Lookup(root, l.Secret())
	if err != nil {
		t.Fatalf("Lookup() error = %v, want nil", err)
	}
	if res != Valid {
		t.Fatalf("Lookup() = %v, want Valid while the launcher holds the lock", res)
	}
	if rec.Pin != "uuid-pinned" {
		t.Fatalf("Pin = %q, want uuid-pinned", rec.Pin)
	}
	if rec.StartedAt.IsZero() {
		t.Error("StartedAt was never stamped")
	}
}

// Age is never a criterion: only the lock is.
func TestARecordOlderThanADayWithAHeldLockIsStillValid(t *testing.T) {
	root := withRoot(t)
	l, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	old := time.Now().Add(-25 * time.Hour)
	dir := Dir(root)
	h := hashOf(l.Secret())
	for _, name := range []string{h + ".lock", h + ".json"} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	if _, res, err := Lookup(root, l.Secret()); err != nil || res != Valid {
		t.Fatalf("Lookup() = (%v, %v), want (Valid, nil) for a day-old launch whose lock is held", res, err)
	}
}

func TestALaunchWhoseLauncherIsGoneIsDeadAndItsFilesAreRemoved(t *testing.T) {
	root := withRoot(t)
	lockPath, jsonPath := plant(t, root, "abandoned-secret", "uuid-a")

	rec, res, err := Lookup(root, "abandoned-secret")
	if err != nil {
		t.Fatalf("Lookup() error = %v, want nil", err)
	}
	if res != Dead {
		t.Fatalf("Lookup() = %v, want Dead when nothing holds the lock", res)
	}
	if rec.Pin != "" {
		t.Errorf("a dead launch returned a record: %+v", rec)
	}
	for _, p := range []string{lockPath, jsonPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after a Dead lookup", filepath.Base(p))
		}
	}
}

func TestAnUnknownBearerIsUnknown(t *testing.T) {
	root := withRoot(t)
	l, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	for _, bearer := range []string{"", "not-a-secret"} {
		_, res, err := Lookup(root, bearer)
		if err != nil {
			t.Fatalf("Lookup(%q) error = %v, want nil", bearer, err)
		}
		if res != Unknown {
			t.Errorf("Lookup(%q) = %v, want Unknown", bearer, res)
		}
	}
}

func TestADeletePendingOpenIsAnUnknownLaunchNotAnError(t *testing.T) {
	transient := errors.New("a dead launch file is being deleted")
	original := retryableLookupOpen
	retryableLookupOpen = func(err error) bool { return errors.Is(err, transient) }
	t.Cleanup(func() { retryableLookupOpen = original })

	for _, action := range []string{
		"reading a codex launch record",
		"locking a codex launch record",
	} {
		rec, res, err := lookupOpenFailure(action, fmt.Errorf("open: %w", transient))
		if err != nil || res != Unknown || rec != (Record{}) {
			t.Errorf("%s during a reap = (%+v, %v, %v), want (zero record, unknown, nil)", action, rec, res, err)
		}
	}

	ordinary := errors.New("the launch directory is unreadable")
	_, res, err := lookupOpenFailure("reading a codex launch record", ordinary)
	if res != Unknown || !errors.Is(err, ordinary) {
		t.Fatalf("ordinary open failure = (%v, %v), want (unknown, wrapped original error)", res, err)
	}
}

func TestReapRemovesOnlyTheDeadRecords(t *testing.T) {
	root := withRoot(t)
	live, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	plant(t, root, "dead-one", "")
	plant(t, root, "dead-two", "")

	n, err := Reap(root)
	if err != nil {
		t.Fatalf("Reap() error = %v, want nil", err)
	}
	if n != 2 {
		t.Fatalf("Reap() = %d, want 2", n)
	}
	if _, res, _ := Lookup(root, live.Secret()); res != Valid {
		t.Fatalf("the live launch reads %v after a sweep, want Valid", res)
	}
	if again, err := Reap(root); err != nil || again != 0 {
		t.Fatalf("second Reap() = (%d, %v), want (0, nil)", again, err)
	}
}

func TestReapToleratesAStoreWithNoLaunchesDirectory(t *testing.T) {
	n, err := Reap(withRoot(t))
	if err != nil || n != 0 {
		t.Fatalf("Reap() on an empty store = (%d, %v), want (0, nil)", n, err)
	}
}

func TestCloseReleasesTheLockAndRemovesBothFiles(t *testing.T) {
	root := withRoot(t)
	l, err := Create(root, "")
	if err != nil {
		t.Fatal(err)
	}
	secret := l.Secret()
	if err := l.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if _, res, _ := Lookup(root, secret); res != Unknown {
		t.Fatalf("after Close the bearer reads %v, want Unknown", res)
	}
	entries, err := os.ReadDir(Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the launches directory still holds %d entries after Close", len(entries))
	}
}

// Nothing else in this file would notice a Create that minted the same secret
// every time, and the shape that assertion has to survive is not the obvious
// one. A constant-secret implementation does not fail on the comparison below:
// both launches resolve to the same record name, flock contends across open
// file descriptions even within a single process, and the SECOND Create
// therefore returns "a freshly generated codex launch name is already locked"
// before there is anything to compare. Measured with the entropy neutered that
// way, this is where it fails. So the error is checked first and either shape
// — a refused Create, or two equal secrets — fails the test.
func TestTwoLaunchesInOneStoreGetDifferentSecrets(t *testing.T) {
	root := withRoot(t)
	first, err := Create(root, "")
	if err != nil {
		t.Fatalf("first Create() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Create(root, "")
	if err != nil {
		t.Fatalf("second Create() = %v, want nil", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if first.Secret() == second.Secret() {
		t.Fatal("two launches in one store minted the same secret")
	}
	if hashOf(first.Secret()) == hashOf(second.Secret()) {
		t.Fatal("two launches in one store landed on the same record name")
	}
	entries, err := os.ReadDir(Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("two launches left %d files behind, want 4: a lock and a record each", len(entries))
	}
}

// The proxy answers every request concurrently and looks a bearer up on each
// one, so two probes of the SAME dead launch is the ordinary case rather than
// an exotic one: a launcher killed with SIGKILL orphans a codex that keeps
// issuing parallel requests under the secret it was given.
//
// The loop is not decoration, it is what makes the test see anything. Measured
// against an exclusive probe: 200 rounds of two probes produced 15 to 44 false
// Valid answers out of 400, so ONE round would have passed about four times in
// five. A single-round version of this test would have been green against the
// bug it was written for.
//
// What is asserted is NEVER VALID, not always Dead, and that distinction is the
// difference between a regression test and a flake. The probe that loses the
// race legitimately answers Unknown: the winner removed the record between the
// loser's read of the json and its own. Measured on a correct implementation,
// that is 0 to 4 of 400 answers on a twelve-processor machine and 198 of 400 at
// GOMAXPROCS=1, so a test demanding Dead from every probe would be red against
// a tree with nothing wrong with it. Both answers refuse the request, which is
// the property being defended.
func TestConcurrentLookupsNeverCallADeadLaunchValid(t *testing.T) {
	const rounds = 200
	// Said out loud rather than left to a green that means nothing. With one
	// processor the goroutines run to completion in turn: the first probe
	// finishes reaping before the second reads anything, so the second answers
	// Unknown and the false Valid this test hunts cannot occur. Measured at
	// GOMAXPROCS=1, 198 of 400 answers were Unknown and the exclusive-probe
	// mutation PASSED.
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("this test needs two probes genuinely in flight at once")
	}
	root := withRoot(t)

	// A live launch is probed alongside every round of the storm. A shared
	// probe must still lose to the exclusive lock its launcher holds, and that
	// is what makes probing with a shared lock an answer rather than merely a
	// quiet one.
	live, err := Create(root, "uuid-live")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })

	var mu sync.Mutex
	counts := map[LookupResult]int{}
	for i := range rounds {
		secret := fmt.Sprintf("abandoned-secret-%d", i)
		plant(t, root, secret, "uuid-a")

		var wg sync.WaitGroup
		wg.Add(3)
		for range 2 {
			go func() {
				defer wg.Done()
				_, res, err := Lookup(root, secret)
				// Two probes reaching the Dead branch together remove the same
				// two files, and the loser of that removal must not turn into
				// an error on the request path.
				if err != nil {
					t.Errorf("Lookup of a dead launch = %v, want no error while another probe reaps the same record", err)
				}
				mu.Lock()
				counts[res]++
				mu.Unlock()
			}()
		}
		go func() {
			defer wg.Done()
			if _, res, err := Lookup(root, live.Secret()); err != nil || res != Valid {
				t.Errorf("the live launch read (%v, %v) while dead records were being reaped around it, want (valid, <nil>)", res, err)
			}
		}()
		wg.Wait()
	}

	if counts[Valid] != 0 {
		t.Fatalf("%d of %d probes called a dead launch valid", counts[Valid], 2*rounds)
	}
	if counts[Dead] == 0 {
		t.Fatalf("no probe answered Dead across %d rounds, so nothing was ever planted or ever read", rounds)
	}
}
