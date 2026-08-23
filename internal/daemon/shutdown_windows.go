//go:build windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has no signals and no way to reach a DETACHED_PROCESS child with
// GenerateConsoleCtrlEvent, so without this file `ccdad daemon stop` has no
// mechanism there at all — and the only fallback anyone reaches for is killing
// a pid read out of a file, which is how an unrelated process gets terminated.
//
// The mechanism is a named event, and the pid is not part of it: the event
// names the STORE, so a stopper that opens it is talking to whatever daemon
// owns that store rather than to whatever process currently holds a number. The
// pid is used only by the terminate fallback, behind the cross-check.

// waitSlice is how long the waiter parks in the kernel before looking at its
// context again. WaitForSingleObject is a BLOCKING syscall on a locked thread,
// so this — not a channel — is the granularity at which the waiter itself can
// be shut down.
const waitSlice = 250 * time.Millisecond

// requestShutdown sets the store's shutdown event.
//
// It NEVER creates the event. Creating it would manufacture the very evidence
// it is reading — the same rule the singleton probe follows for the lock file —
// and the created event would have no waiter, so the stop would be reported and
// never happen. ERROR_FILE_NOT_FOUND therefore means no daemon ever listened
// here, which is ErrNoShutdownListener rather than a failure.
func requestShutdown(int) error {
	name, err := shutdownEventName()
	if err != nil {
		return err
	}
	wide, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("encoding the shutdown event name %q: %w", name, err)
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, wide)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return fmt.Errorf("%w on %s", ErrNoShutdownListener, name)
		}
		return fmt.Errorf("opening the shutdown event %s: %w", name, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetEvent(h); err != nil {
		return fmt.Errorf("signalling the shutdown event %s: %w", name, err)
	}
	return nil
}

// forceShutdown terminates the process at target.PID, once it has proved to be
// the daemon that was recorded.
//
// PROCESS_QUERY_LIMITED_INFORMATION is deliberately the weaker of the two query
// rights: it is enough for QueryFullProcessImageName and GetProcessTimes, and
// it is granted across integrity levels where PROCESS_QUERY_INFORMATION is not.
func forceShutdown(target shutdownTarget) error {
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(target.PID))
	if err != nil {
		return fmt.Errorf("opening pid %d to terminate it: %w", target.PID, err)
	}
	defer windows.CloseHandle(h)

	facts := readProcessFacts(h)
	if allowed, why := mayTerminate(target, facts); !allowed {
		return fmt.Errorf("refusing to terminate pid %d: %s", target.PID, why)
	}
	// Exit code 1: the daemon did not choose to stop, and a process terminated
	// from outside has not exited cleanly by any definition.
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminating pid %d: %w", target.PID, err)
	}
	return nil
}

// readProcessFacts reads what the operating system will say about an open
// process handle. A fact it will not give up is left zero, which mayTerminate
// treats as a refusal rather than as a fact that does not count.
func readProcessFacts(h windows.Handle) processFacts {
	var facts processFacts

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
		facts.Image = windows.UTF16ToString(buf[:size])
	}

	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err == nil {
		facts.CreatedAt = time.Unix(0, created.Nanoseconds())
	}
	return facts
}

// watchShutdownRequest creates the store's shutdown event and stops the daemon
// when it is set.
//
// The daemon is the only creator, which is what makes a missing event genuine
// evidence that no daemon is listening.
//
// The waiter runs on a LOCKED thread. WaitForSingleObject parks an OS thread in
// a syscall and the Go scheduler cannot preempt it, so without LockOSThread the
// runtime would be down one usable thread for as long as the wait lasts — and
// the wait lasts for the daemon's whole life. Locking it makes that thread
// this waiter's own, and the timeout is what lets the waiter be shut down at
// all: there is no way to interrupt a blocking wait from Go.
func watchShutdownRequest(ctx context.Context, stop func(), log *Logger) {
	name, err := shutdownEventName()
	if err != nil {
		log.Printf("no shutdown event: %v; this daemon can only be stopped by force", err)
		return
	}
	wide, err := windows.UTF16PtrFromString(name)
	if err != nil {
		log.Printf("encoding the shutdown event name %q: %v", name, err)
		return
	}
	// Manual reset, so a request that arrives while the daemon is already
	// stopping is not silently consumed; initially unset.
	h, err := windows.CreateEvent(nil, 1, 0, wide)
	if err != nil {
		log.Printf("creating the shutdown event %s: %v; `ccdad daemon stop` will have to terminate this process", name, err)
		return
	}

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer windows.CloseHandle(h)
		for {
			if ctx.Err() != nil {
				return
			}
			ev, err := windows.WaitForSingleObject(h, uint32(waitSlice/time.Millisecond))
			switch {
			case err != nil:
				log.Printf("waiting on the shutdown event %s: %v; nothing will be listening from here on", name, err)
				return
			case ev == uint32(windows.WAIT_TIMEOUT):
				continue
			case ev == windows.WAIT_OBJECT_0:
				log.Printf("a shutdown request arrived on %s, stopping after the tick in flight", name)
				stop()
				return
			default:
				log.Printf("the shutdown event %s returned %#x; the waiter is giving up", name, ev)
				return
			}
		}
	}()
}
