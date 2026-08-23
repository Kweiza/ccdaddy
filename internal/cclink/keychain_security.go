package cclink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// The `security` spawns. Everything with a decision in it lives in keychain.go;
// this file is the thin layer that runs a process and turns its exit code into
// an answer.

// securityPath is Apple's system binary, pinned by ABSOLUTE PATH.
//
// Claude Code invokes it as bare "security" and lets PATH resolve it; ccdad
// deliberately does not. This is the code path that reads a live OAuth
// credential out of the Keychain, and a `security` planted earlier on PATH
// would be handed the secret. /usr/bin/security is present on every macOS, so
// there is nothing to gain from the lookup and a credential to lose.
//
// It is a var so a test can point it at a fixture; see keychain_security_test.go.
var securityPath = "/usr/bin/security"

// keychainTimeout bounds every spawn, and it is the whole reason this layer is
// not three lines of exec.Command.
//
// A locked login keychain on a headless or SSH session does not fail: it blocks
// on an unlock dialog that nobody is in front of. Claude Code allows 10 s;
// ccdad allows less, because a healthy keychain answers in well under 100 ms
// and because the caller that matters most -- doctor -- is a diagnostic a user
// runs while waiting for it. A var so a test can shrink it rather than sleep.
var keychainTimeout = 5 * time.Second

// keychainWaitDelay bounds the SECOND way a spawn can hang, which the deadline
// alone does not cover: the process exits, but something it started inherited
// the output pipe and keeps it open, so Wait blocks on a copy that will never
// end. WaitDelay closes the pipes and gives up. Without it, "kill the child" is
// not the same as "stop waiting".
var keychainWaitDelay = time.Second

// keychainGOOS is the platform this layer believes it is on. macOS is the only
// one with a Keychain, and on every other platform a spawn is a guaranteed
// waste of a process.
//
// It is a var rather than a direct runtime.GOOS test so the darwin path is
// reachable from a test on the Linux machine this project is developed on;
// otherwise the deadline, the kill and the exit-code mapping would ship
// unexercised, which is the whole substance of the file.
var keychainGOOS = runtime.GOOS

// securityNotFoundCode is errSecItemNotFound as `security` surfaces it. It is
// the one non-zero exit that is an ANSWER rather than a failure, and telling it
// apart from the others is what keeps a locked keychain from being reported as
// a clean machine.
const securityNotFoundCode = 44

// maxKeychainSecret caps a value read out of the Keychain, matching the 1 MiB
// cap Claude Code puts on the credentials file. The item is supposed to hold
// the same document.
const maxKeychainSecret = maxCredentialsSize

// ErrKeychainUnsupported means this platform has no macOS Keychain. It is not a
// failure -- callers report it as "does not apply here".
var ErrKeychainUnsupported = errors.New("the macOS Keychain does not exist on this platform")

// KeychainError is a `security` invocation that did not answer: not "the item
// is not there", which is an answer, but "ccdad could not find out".
//
// A probe that could not answer is not an absence, the same rule daemon status
// follows one layer up. A keychain that is locked and a keychain with nothing
// in it produce the same silence, and treating the first as the second reports
// a machine as clean while a stale credential sits on it.
type KeychainError struct {
	// Op is the security subcommand that failed.
	Op string
	// failure is why, classified the way Claude Code classifies it.
	failure keychainFailure
	// stderr is what security actually said, kept for the error string.
	stderr string
}

func (e *KeychainError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("security %s: %s: %s", e.Op, e.failure, e.stderr)
	}
	return fmt.Sprintf("security %s: %s", e.Op, e.failure)
}

// Detail is the sentence a diagnostic prints. Every failure has its own, so a
// report says what the user should do next rather than only that something went
// wrong.
func (e *KeychainError) Detail() string { return keychainFailureDetail(e.failure) }

// securityResult is one completed spawn. A non-zero code is carried here rather
// than as an error, because callers decide which codes mean what.
type securityResult struct {
	stdout string
	stderr string
	code   int
}

// runSecurity runs one `security` subcommand under a wall-clock deadline with
// an explicit kill.
//
// exec.CommandContext's default cancellation is Process.Kill -- SIGKILL, not a
// request the child can ignore -- and WaitDelay bounds the wait that outlives
// it. Both are needed: the deadline stops a child that will not finish, and the
// delay stops a Wait that will not return.
func runSecurity(ctx context.Context, args ...string) (securityResult, error) {
	// The subcommand names itself in every error, rather than being passed a
	// second time alongside the arguments: two spellings of the same thing
	// drift, and the one in the error is the one nobody re-reads.
	op := args[0]
	if keychainGOOS != "darwin" {
		return securityResult{}, ErrKeychainUnsupported
	}

	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, securityPath, args...)
	cmd.WaitDelay = keychainWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A nil Stdin is os.DevNull. `security` reads stdin only under -i, which
	// ccdad never passes, but inheriting the terminal would let a subcommand
	// that decided to prompt compete with whatever is reading it.
	cmd.Stdin = nil

	err := cmd.Run()
	res := securityResult{stdout: stdout.String(), stderr: stderr.String()}

	// The deadline is checked BEFORE the error is inspected, because a killed
	// child reports as an ordinary non-zero exit and would otherwise be
	// classified from an empty stderr as "other".
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, &KeychainError{Op: op, failure: failTimedOut}
	}
	if err == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller cancelled. That is the caller's business, not a keychain
		// failure, so it propagates unwrapped.
		return res, ctxErr
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The process finished but its output pipe outlived it. Whatever is in
		// the buffer may be a prefix, and a truncated credential is worse than
		// no credential.
		return res, &KeychainError{Op: op, failure: failLingering}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
		return res, nil
	}
	// "The binary is not there" has two spellings and the second one is not
	// reachable from this machine: Go's Windows lookup appends PATHEXT and, for
	// a path with no extension, ends in exec.ErrNotFound rather than in
	// fs.ErrNotExist. The keychain never runs on Windows, but the TEST forces
	// the darwin path there, and a run on the Windows CI leg would otherwise
	// classify a missing binary as "other".
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return res, &KeychainError{Op: op, failure: failSecurityMissing}
	}
	return res, &KeychainError{Op: op, failure: failOther, stderr: err.Error()}
}

// Present reports whether the item exists, WITHOUT decrypting it.
//
// The lookup deliberately omits -w. `find-generic-password` without it returns
// only the item's attributes, which needs no access to the secret and therefore
// can never raise the "wants to use your keychain" dialog -- not even for an
// item another application owns. A diagnostic that popped an auth prompt on
// someone's machine would be a worse bug than the drift it was looking for.
func (it KeychainItem) Present(ctx context.Context) (bool, error) {
	res, err := runSecurity(ctx, "find-generic-password", "-a", it.Account, "-s", it.Service)
	if err != nil {
		return false, err
	}
	switch res.code {
	case 0:
		return true, nil
	case securityNotFoundCode:
		return false, nil
	}
	return false, &KeychainError{
		Op: "find-generic-password", failure: classifyKeychainError(res.stderr), stderr: res.stderr}
}

// Read returns the secret stored in the item, and whether there was one.
//
// The argument order is Claude Code's own, -w between the account and the
// service, so a process listing on a machine running both shows the same shape.
func (it KeychainItem) Read(ctx context.Context) (string, bool, error) {
	res, err := runSecurity(ctx, "find-generic-password", "-a", it.Account, "-w", "-s", it.Service)
	if err != nil {
		return "", false, err
	}
	switch res.code {
	case 0:
		return trimKeychainSecret(res.stdout), true, nil
	case securityNotFoundCode:
		return "", false, nil
	}
	return "", false, &KeychainError{
		Op: "find-generic-password", failure: classifyKeychainError(res.stderr), stderr: res.stderr}
}

// Delete removes the item. An item that was already absent is success: the
// caller asked for a state, not for an event.
//
// NOTHING IN ccdad CALLS THIS, and that is now a ruling rather than a gap.
// Deleting the item during a switch was the proposed use. The fact it waited
// on is settled -- <=2.1.112 reads the keychain and FALLS BACK to the
// credentials file, so the delete redirects rather than logs out -- and the
// measurement then supplied three reasons not to ship it anyway:
//
//   - 2.1.113 dropped the backend, so on every Claude Code a user can install
//     today the item is inert. The spawn would buy nothing while sitting inside
//     the credential-lock window of every macOS switch.
//   - Where it is NOT inert, the item is that Claude Code's live login, so
//     deleting it unasked is "destroying a credential on every switch" -- the
//     highest-rated risk this tool carries -- with a different subject. Making it safe needs the item READ and
//     attributed against the store first -- a second spawn, and one that
//     decrypts.
//   - AND IT WOULD NOT EVEN WORK THERE. From 1.0.36 the combinator's update()
//     deletes the credentials file whenever the pre-write keychain read came
//     back empty, so the next token refresh after a delete recreates the item
//     AND unlinks the file ccdad had just redirected Claude Code to. A switch
//     that repairs a machine for a few hours and then costs it the file is not
//     a repair. On such a machine the fix is upgrading Claude Code.
//
// doctor names the item, hands over the command, and says which of those two
// machines the command is for.
func (it KeychainItem) Delete(ctx context.Context) error {
	res, err := runSecurity(ctx, "delete-generic-password", "-a", it.Account, "-s", it.Service)
	if err != nil {
		return err
	}
	if res.code == 0 || res.code == securityNotFoundCode {
		return nil
	}
	return &KeychainError{
		Op: "delete-generic-password", failure: classifyKeychainError(res.stderr), stderr: res.stderr}
}

// KeychainLookup is what a probe for the legacy credential item found.
type KeychainLookup struct {
	// Present is whether one of the candidate names is on the keychain.
	Present bool

	// Item is the candidate that was found, or the first one when none was.
	Item KeychainItem

	// Checked is every name that was looked for, in order. A report that says
	// "nothing there" has to be able to say what it looked for, and on a
	// decomposed CLAUDE_CONFIG_DIR that is two names rather than one.
	Checked []KeychainItem
}

// ProbeCredentialKeychainItem is the doctor probe: derive the names this
// environment's item could carry and ask whether any of them is there, without
// decrypting anything.
//
// The candidates are tried in order and the FIRST hit wins, so a machine that
// somehow has both spellings is reported as the newer one -- which is the one a
// Claude Code new enough to have written both would read. A probe that fails is
// returned as an error rather than continued past: "the keychain is locked" is
// not "the item is not under this name", and answering the second question with
// the first is the mistake this whole check exists to avoid.
func ProbeCredentialKeychainItem(ctx context.Context) (KeychainLookup, error) {
	items := CredentialKeychainItems()
	out := KeychainLookup{Item: items[0], Checked: items}
	for _, item := range items {
		present, err := item.Present(ctx)
		if err != nil {
			out.Item = item
			return out, err
		}
		if present {
			out.Present, out.Item = true, item
			return out, nil
		}
	}
	return out, nil
}

// LoadKeychainCredentials reads the login a Keychain-era Claude Code would be
// using on this machine. The second return is false when there is no such item,
// which is every machine running any Claude Code this project has seen.
//
// NOTHING IN ccdad CALLS THIS EITHER, and the routing question the other
// proposed use raised is answered: Load() must NOT fall back to the Keychain on
// macOS. Since
// 2.1.113 the installed Claude Code does not read the item, so a ccdad that did
// would report a login that Claude Code will never use -- a confident wrong
// answer on the overwhelming majority of machines, reached by changing the read
// path of every command (switch, which, doctor, add) for a population that
// shrinks with each release. doctor's two rows already tell the truth together
// without touching Load(): `claude-code` says there is no login in the file,
// and `keychain` says the item is there.
//
// A value that is not JSON is an error rather than an empty blob: `security -w`
// hex-encodes data it cannot print, so unparseable output means the item is not
// holding what its name says it holds, and silently reporting "no login" would
// hide that.
func LoadKeychainCredentials(ctx context.Context) (Blob, bool, error) {
	secret, ok, err := CredentialKeychainItem().Read(ctx)
	if err != nil || !ok {
		return nil, false, err
	}
	if len(secret) > maxKeychainSecret {
		return nil, false, fmt.Errorf("keychain item (over %d bytes): %w", maxKeychainSecret, ErrTooLarge)
	}
	if secret == "" {
		return Blob{}, true, nil
	}
	var b Blob
	if err := json.Unmarshal([]byte(secret), &b); err != nil {
		return nil, false, fmt.Errorf("parsing the keychain credential: %w", err)
	}
	if b == nil {
		b = Blob{}
	}
	return b, true, nil
}
