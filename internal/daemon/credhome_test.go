package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/credhome"
)

// The refusal that the whole item exists for: a daemon whose store is not the
// one driving this Claude Code login does not start.
func TestRunRefusesACredentialHomeAnotherStoreDrives(t *testing.T) {
	isolate(t)
	other := filepath.Join(t.TempDir(), "other-ccdad")
	claimHeldByAnotherStore(t, other)

	err := Run(context.Background(), Options{})
	if !errors.Is(err, credhome.ErrClaimed) {
		t.Fatalf("Run() = %v, want ErrClaimed", err)
	}
	// Not ErrSingletonHeld. The two refusals are different questions with
	// different fixes — one says stop your other daemon, the other says stop
	// your other STORE — and a caller that could not tell them apart would
	// print the wrong instruction.
	if errors.Is(err, ErrSingletonHeld) {
		t.Error("Run() reported the credential-home refusal as a lost singleton race")
	}

	// The singleton has to be back. A daemon that kept it after refusing would
	// make `ccdad daemon status` report a daemon that is not there — and would
	// hide the state from the auto-start hook, which gates on exactly that.
	held, herr := SingletonHeld()
	if herr != nil {
		t.Fatal(herr)
	}
	if held {
		t.Error("the singleton is still held after Run refused; a refused daemon must give it back")
	}

	// The refusal reaches daemon.log. Spawn hands the child /dev/null, so a
	// message written before OpenLog is a message nobody can ever read — which
	// is why the claim is taken after the log is open rather than beside the
	// singleton.
	body, rerr := os.ReadFile(mustPath(LogPath()))
	if rerr != nil {
		t.Fatalf("no daemon.log was written before the refusal: %v", rerr)
	}
	if !strings.Contains(string(body), other) {
		t.Errorf("daemon.log does not name the store that holds the claim (%s):\n%s", other, body)
	}
}

// A filesystem that cannot lock the credential home DEGRADES: the daemon runs
// without the claim rather than refusing.
//
// Refusing there would take ccdad off every machine whose home directory is on
// a network mount — an ordinary configuration, and one where the store itself
// can be perfectly local — to guard a hazard that needs a second store to exist
// at all. It would also create an auto-start treadmill: a daemon that refuses
// releases the singleton, so every allow-listed command would fork another one.
func TestRunKeepsGoingWhenTheCredentialHomeCannotBeLocked(t *testing.T) {
	isolate(t)
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticked := make(chan struct{})
	var once sync.Once
	// The stop comes from OUTSIDE the tick. A tick that waited on the context
	// it is itself meant to trigger deadlocks against itself, and the failure
	// presents as a hung daemon rather than as a hung test.
	go func() {
		select {
		case <-ticked:
		case <-time.After(20 * time.Second):
		}
		cancel()
	}()
	err := Run(ctx, Options{
		Interval: time.Millisecond,
		Tick: func(context.Context) error {
			once.Do(func() { close(ticked) })
			return nil
		},
	})
	select {
	case <-ticked:
	default:
		t.Fatal("the daemon never ticked; it refused a credential home it should have degraded past")
	}
	if err != nil {
		t.Fatalf("Run() = %v, want nil — an unlockable credential home is a degraded mode, not a failure", err)
	}

	body, rerr := os.ReadFile(mustPath(LogPath()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	// The degraded state has to be IN the log. It is invisible everywhere else
	// on the machine, and a daemon running unguarded without saying so is the
	// state this whole change exists to make reportable.
	if !strings.Contains(string(body), "WITHOUT the credential-home claim") {
		t.Errorf("daemon.log does not say the daemon is running unguarded:\n%s", body)
	}
}

// The published document records which credential home this daemon is actually
// driving, which is the only way a reader can catch a daemon that resolved a
// different one — the `ccdad run --full-profile` case.
func TestRunPublishesTheCredentialHomeItDrives(t *testing.T) {
	isolate(t)
	want := mustPath(credhome.Home())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Let it publish its first status, then stop it.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if s, ok, err := ReadStatus(); err == nil && ok && s.PID != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if err := Run(ctx, Options{Interval: time.Millisecond}); err != nil {
		t.Fatal(err)
	}

	s, ok, err := ReadStatus()
	if err != nil || !ok {
		t.Fatalf("ReadStatus() = (%v, %v)", ok, err)
	}
	if s.CredentialHome != want {
		t.Errorf("status.credentialHome = %q, want %q", s.CredentialHome, want)
	}
}

// The claim is given back on the way out, so a daemon that has stopped does not
// lock the next one out of the credential home.
func TestRunReleasesTheClaimOnShutdown(t *testing.T) {
	isolate(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if s, err := credhome.Probe(); err == nil && s.Held {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if err := Run(ctx, Options{Interval: time.Millisecond}); err != nil {
		t.Fatal(err)
	}

	s, err := credhome.Probe()
	if err != nil {
		t.Fatal(err)
	}
	if s.Held {
		t.Error("the credential-home claim is still held after the daemon stopped")
	}
}
