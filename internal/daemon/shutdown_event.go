package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// Everything in this file is platform-independent on purpose, and it is the
// half of the Windows shutdown path that can be tested at all: none of the
// Win32 calls beside it compile — let alone run — on the machine this
// repository is developed on, so without a seam here the whole mechanism would
// ship unexecuted and be discovered broken by a user.

// shutdownEventPrefix is §8.4's `Local\ccdad-shutdown-<hash>`.
//
// `Local\` is a per-SESSION namespace, and that is a real limitation rather
// than an implementation detail: a daemon started inside an RDP session cannot
// be stopped from the console session, because the two see different objects
// of the same name. `Global\` would fix it and costs SeCreateGlobalPrivilege,
// which a standard user does not have — so the object would fail to create at
// all for exactly the users least able to diagnose it. Per-session is the
// deliberate choice; `ccdad daemon stop` from another session falls through to
// the guarded terminate.
const shutdownEventPrefix = `Local\ccdad-shutdown-`

// shutdownEventNameFor derives the event name for a store directory.
//
// The store path is HASHED rather than embedded, and that is a hard constraint
// rather than a tidiness one: a kernel object name may not contain a backslash
// past its namespace prefix, and every Windows store path has several. The hash
// also bounds the name, which has a 260-character limit a deep path would
// otherwise reach.
//
// The path is cleaned and lowercased first so that two spellings of one store
// produce one name. Lowercasing is correct here and would not be on a
// case-sensitive filesystem — this name is only ever used on Windows, where the
// filesystem is case-insensitive and `C:\Users\x` and `c:\users\x` are the same
// directory. It is the SAME reason ChildEnv resolves symlinks: the daemon and
// the process trying to stop it must derive one name from one store, however
// each of them spelled it.
func shutdownEventNameFor(store string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(path.Clean(toSlash(store)))))
	// Half the digest. 32 hex characters is far past collision relevance for a
	// set whose size is "directories on one machine", and it keeps the whole
	// name inside 60 characters.
	return shutdownEventPrefix + hex.EncodeToString(sum[:16])
}

// shutdownEventName is the event name for this process's store.
func shutdownEventName() (string, error) {
	root, err := storeRoot()
	if err != nil {
		return "", err
	}
	// Resolved, for the same reason ChildEnv resolves: the daemon was handed a
	// canonical store and the CLI may have been given a symlink to it.
	return shutdownEventNameFor(resolvePath(root)), nil
}

// toSlash and windowsBase do what filepath would, for WINDOWS paths, on
// whatever platform this is compiled for — and that is the point rather than an
// inconvenience. filepath's separator is chosen at BUILD time, so on the Linux
// machine this repository is developed on filepath.Base(`C:\ccdad\ccdad.exe`)
// is the whole string and filepath.Clean leaves every backslash alone: the test
// would pass while measuring something the Windows binary never does. Both
// failed exactly that way before these existed.
func toSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func windowsBase(p string) string {
	slashed := toSlash(p)
	if i := strings.LastIndexByte(slashed, '/'); i >= 0 {
		return slashed[i+1:]
	}
	return slashed
}

// processFacts is what can be read back about whatever holds a pid right now.
// Both fields are zero when the operating system would not say.
type processFacts struct {
	// Image is the full path of the running image, as
	// QueryFullProcessImageName reports it.
	Image string
	// CreatedAt is the process creation time, which is the field that makes
	// this a cross-check rather than a guess: a recycled pid belongs to a
	// process created LATER than the daemon published its start.
	CreatedAt time.Time
}

// shutdownTarget is what the stopping side knows about the daemon it means to
// terminate: the pid it read from the pidfile, the image a ccdad daemon must be
// running, and when the daemon itself said it started.
type shutdownTarget struct {
	PID       int
	Image     string
	StartedAt time.Time
}

const (
	// maxStartupLag is how long a daemon may take between being created and
	// publishing its start time. It covers AcquireSingleton's three retries,
	// opening the log, sweeping temp files and writing the pidfile — all of it
	// milliseconds, with a minute of room for a machine under an antivirus
	// scan.
	maxStartupLag = time.Minute
	// startupGrace absorbs the rounding between a FILETIME and time.Now on the
	// same machine. A process cannot be created after it publishes, so this is
	// clock granularity and nothing else.
	startupGrace = 2 * time.Second
)

// mayTerminate reports whether the process now holding want.PID is still the
// daemon that was recorded, and says why not when it is not.
//
// TerminateProcess on a recycled pid kills an innocent process — that is the
// entire hazard §10.3 lists this cross-check against, and it is not remote: a
// daemon that crashed hours ago leaves its pid in the pidfile, and Windows
// reissues pids aggressively.
//
// It fails CLOSED at every step. A fact that could not be read is a refusal
// rather than a fact that does not count, because "the process would not say
// what it is" and "the process is what we expected" must never reach the same
// branch.
func mayTerminate(want shutdownTarget, got processFacts) (bool, string) {
	switch {
	case want.PID <= 0:
		return false, fmt.Sprintf("%d is not a pid", want.PID)
	case want.Image == "":
		return false, "the image a ccdad daemon runs as could not be determined"
	case got.Image == "":
		return false, fmt.Sprintf("the process at pid %d would not say what image it is running", want.PID)
	case !strings.EqualFold(windowsBase(got.Image), windowsBase(want.Image)):
		return false, fmt.Sprintf("pid %d is now running %s, not %s",
			want.PID, windowsBase(got.Image), windowsBase(want.Image))
	case want.StartedAt.IsZero():
		return false, fmt.Sprintf("no daemon start time was published, so a recycled pid %d cannot be ruled out", want.PID)
	case got.CreatedAt.IsZero():
		return false, fmt.Sprintf("the creation time of pid %d could not be read", want.PID)
	case got.CreatedAt.After(want.StartedAt.Add(startupGrace)):
		return false, fmt.Sprintf("pid %d was created at %s, after the daemon published %s — the pid has been reused",
			want.PID, got.CreatedAt.Format(time.RFC3339), want.StartedAt.Format(time.RFC3339))
	case got.CreatedAt.Before(want.StartedAt.Add(-maxStartupLag)):
		return false, fmt.Sprintf("pid %d was created at %s, more than %s before the daemon published %s",
			want.PID, got.CreatedAt.Format(time.RFC3339), maxStartupLag, want.StartedAt.Format(time.RFC3339))
	}
	return true, ""
}
