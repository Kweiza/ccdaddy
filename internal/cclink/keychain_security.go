package cclink

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
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

// The two deadlines below bound every spawn, and they are the whole reason this
// layer is not three lines of exec.Command. Each is defined here and nowhere
// else; a caller names which one applies.

// keychainReadTimeout bounds a LOOKUP: doctor, `which`, `status`, and the
// daemon's once-a-tick read of who is live.
//
// A locked login keychain on a headless or SSH session does not fail: it blocks
// on an unlock dialog that nobody is in front of. Claude Code allows 10 s; a
// lookup here allows less, because a healthy keychain answers in well under
// 100 ms and because the caller that matters most -- doctor -- is a diagnostic
// a user runs while waiting for it. A lookup that runs out of time decided
// nothing and changed nothing, so the short bound costs one unanswered read,
// which every caller already handles. A var so a test can shrink it rather than
// sleep.
var keychainReadTimeout = 5 * time.Second

// keychainWriteTimeout bounds every spawn on the path that REPLACES the item:
// the write itself, the read its merge base is taken from, the read that
// confirms a write which ran out of time, and the delete.
//
// It is separate from the read bound because running out of time means
// something different here. A lookup killed at its deadline changed nothing. A
// write killed at its deadline may have completed anyway -- `security` hands
// the item to securityd, and killing the client does not recall the request.
// On 2026-11-10 one did exactly that: at a load average of 100-150 an
// add-generic-password was killed at the old shared 5 s bound, the item went
// on to hold the new login, and ccdad reported "nothing was written" and left
// the credentials file on the old one. Claude Code then spent the old account
// for three hours.
//
// 30 s, derived rather than picked. Claude Code gives its own keychain spawns
// 10 s, which is what its authors consider a slow keychain; on the loaded Mac
// above one `security` call took 9.4 s, already at Claude Code's limit while
// the machine was still usable. Three times that measurement covers a machine
// under heavier load than the one that failed, without reaching a wait a user
// would mistake for a hang. The longer bound costs something only while the
// keychain is actually slow, and it is paid inside Claude Code's credential
// locks. A switch spawns up to three times under them -- the base read, the
// write, and the read-back of a write that ran out of time -- each with this
// bound and keychainWaitDelay after it, so Claude Code's own refresh can be held
// off for about 93 s in the worst case; a short bound costs a write whose
// outcome nobody knows. Even this one can be
// exceeded, which is why a write that runs out of time is read back rather
// than reported as a failure (see confirmKeychainWrite). A var so a test can
// shrink it rather than sleep.
var keychainWriteTimeout = 30 * time.Second

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
	// code is the exit status, and it is carried for the ONE failure where
	// nothing else is: a spawn that exits non-zero and prints nothing leaves
	// the number as the entire report. Zero here means "no exit status was
	// involved" -- a timeout, an unreaped child, a missing binary -- rather
	// than success, which never reaches this type.
	code int
}

func (e *KeychainError) Error() string {
	switch {
	case e.stderr != "":
		return fmt.Sprintf("security %s: %s: %s", e.Op, e.failure, e.stderr)
	case e.code != 0:
		return fmt.Sprintf("security %s: %s (exit %d)", e.Op, e.failure, e.code)
	}
	return fmt.Sprintf("security %s: %s", e.Op, e.failure)
}

// Detail is the sentence a diagnostic prints. Every failure has its own, so a
// report says what the user should do next rather than only that something went
// wrong.
//
// The silent failure gets its sentence built here rather than in the table,
// because the exit code IS the sentence: without it the report is "it failed",
// which is what the caller already knew.
func (e *KeychainError) Detail() string {
	if e.failure == failSaidNothing && e.code != 0 {
		return fmt.Sprintf("the lookup exited %d and printed nothing, so the exit code is the only evidence there is; "+
			"run it by hand to see what it does", e.code)
	}
	return keychainFailureDetail(e.failure)
}

// securityResult is one completed spawn. A non-zero code is carried here rather
// than as an error, because callers decide which codes mean what.
type securityResult struct {
	stdout string
	stderr string
	code   int
}

// runSecurity is how every subcommand is run: spawnSecurity, unless a test in
// another package has put a FakeKeychain in its place (SetKeychainForTest).
func runSecurity(ctx context.Context, bound time.Duration, args ...string) (securityResult, error) {
	securityImplMu.RLock()
	impl := securityImpl
	securityImplMu.RUnlock()
	return impl(ctx, bound, args...)
}

var (
	securityImplMu sync.RWMutex
	securityImpl   = spawnSecurity
)

// spawnSecurity runs one `security` subcommand under a wall-clock deadline with
// an explicit kill.
//
// exec.CommandContext's default cancellation is Process.Kill -- SIGKILL, not a
// request the child can ignore -- and WaitDelay bounds the wait that outlives
// it. Both are needed: the deadline stops a child that will not finish, and the
// delay stops a Wait that will not return.
//
// bound is the deadline: keychainReadTimeout for a lookup, keychainWriteTimeout
// for anything on the path that replaces the item. The caller names it rather
// than this function choosing from the subcommand, because the read a switch
// takes its merge base from is the same subcommand as a doctor lookup and does
// not get the same bound.
func spawnSecurity(ctx context.Context, bound time.Duration, args ...string) (securityResult, error) {
	// The subcommand names itself in every error, rather than being passed a
	// second time alongside the arguments: two spellings of the same thing
	// drift, and the one in the error is the one nobody re-reads.
	op := args[0]
	if keychainGOOS != "darwin" {
		return securityResult{}, ErrKeychainUnsupported
	}

	ctx, cancel := context.WithTimeout(ctx, bound)
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
		// -1 is a process that did not EXIT: a signal ended it. That is not an
		// answer from `security`, and for a write it is a request that may
		// already have reached securityd, so it is classified rather than
		// handed back as a status nobody chose.
		if exitErr.ExitCode() == -1 {
			return res, &KeychainError{Op: op, failure: failSignaled, stderr: res.stderr}
		}
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
	res, err := runSecurity(ctx, keychainReadTimeout, "find-generic-password", "-a", it.Account, "-s", it.Service)
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
		Op: "find-generic-password", failure: classifyKeychainFailure(res.stderr, res.code), stderr: res.stderr, code: res.code}
}

// Read returns the secret stored in the item, and whether there was one.
//
// The argument order is Claude Code's own, -w between the account and the
// service, so a process listing on a machine running both shows the same shape.
func (it KeychainItem) Read(ctx context.Context) (string, bool, error) {
	return it.readWithin(ctx, keychainReadTimeout)
}

// readWithin is Read under a caller-chosen deadline: the write path reads with
// keychainWriteTimeout, for the reason that bound's comment gives.
func (it KeychainItem) readWithin(ctx context.Context, bound time.Duration) (string, bool, error) {
	res, err := runSecurity(ctx, bound, "find-generic-password", "-a", it.Account, "-w", "-s", it.Service)
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
		Op: "find-generic-password", failure: classifyKeychainFailure(res.stderr, res.code), stderr: res.stderr, code: res.code}
}

// Delete removes the item. An item that was already absent is success: the
// caller asked for a state, not for an event.
//
// ONE CALLER, AND THE RULING BELOW IS ABOUT A DIFFERENT SUBJECT. removeSession
// deletes the item a `ccdad run` session or a probe created under its OWN
// scoped credential root -- an ephemeral store ccdad made, whose directory is
// being deleted in the same breath, and whose login has just been carried into
// the account's snapshot. Nothing about that is the machine's login.
//
// DELETING THE MACHINE'S OWN ITEM DURING A SWITCH is what the ruling refuses,
// and it still does. The fact it waited on is settled -- <=2.1.112 reads the
// keychain and FALLS BACK to the credentials file, so the delete redirects
// rather than logs out -- and the measurement then supplied three reasons not
// to ship it anyway:
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
	res, err := runSecurity(ctx, keychainWriteTimeout, "delete-generic-password", "-a", it.Account, "-s", it.Service)
	if err != nil {
		return err
	}
	if res.code == 0 || res.code == securityNotFoundCode {
		return nil
	}
	return &KeychainError{
		Op: "delete-generic-password", failure: classifyKeychainFailure(res.stderr, res.code), stderr: res.stderr, code: res.code}
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

// Write installs secret into the item, creating it or replacing whatever is
// there.
//
// WHY ccdad WRITES THE KEYCHAIN AT ALL, given that the header of keychain.go
// spent its length arguing ccdad should not touch it: because the premise that
// argument rested on -- that 2.1.113 dropped the backend, so the item is inert
// on anything a user can install today -- is FALSE. Measured against the
// installed 2.1.234, 2.1.238 and 2.1.251, every one of them carries a whole
// keychain backend that spawns `security find-generic-password`, and the
// combinator still reads it BEFORE the credentials file:
//
//	read(a){let o=e.read(a);if(o!==null&&o!==void 0)return o;return t.read(a)||{}}
//
// So on macOS the item is the live login, and a switch that writes only
// .credentials.json writes the store Claude Code consults SECOND. It appears to
// work -- the file changes, `which` reads it back, the daemon logs a switch --
// and Claude Code goes on authenticating as whoever the item names. That was
// observed, not deduced: with the item holding one account and the file another,
// an hour of work after a switch landed entirely on the item's account.
//
// -U is what makes this an install rather than a create: without it `security`
// refuses an item that already exists with errSecDuplicateItem, which is the
// ordinary case here -- the item is nearly always already there.
//
// THE SECRET GOES ON ARGV, and that is a real cost rather than an oversight. -X
// takes the payload as hex, and hex is what makes `security` store the bytes
// rather than interpret them; the alternative is -w, which takes the same
// plaintext on the same argv. Both are visible in a process listing for the
// life of the spawn. Claude Code's own writer passes -X the same way and only
// falls back from stdin to argv past a length limit, so ccdad is not opening an
// exposure Claude Code closes -- but it is opening one on every switch rather
// than on an occasional oversized payload. Narrowing it needs `security -i`,
// which is an interactive command stream rather than a flag, and runSecurity
// deliberately pins Stdin to DevNull so that a subcommand which decides to
// prompt cannot compete for the terminal. Changing that is a bigger change than
// this one and it is not free either.
func (it KeychainItem) Write(ctx context.Context, secret string) error {
	res, err := runSecurity(ctx, keychainWriteTimeout, "add-generic-password", "-U",
		"-a", it.Account, "-s", it.Service,
		"-X", hex.EncodeToString([]byte(secret)))
	if err != nil {
		return err
	}
	if res.code == 0 {
		return nil
	}
	return &KeychainError{
		Op: "add-generic-password", failure: classifyKeychainFailure(res.stderr, res.code), stderr: res.stderr, code: res.code}
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
// Load() CALLS THIS FIRST on macOS, which is the reverse of what this header
// used to rule. It said Load must not consult the keychain, because "since
// 2.1.113 the installed Claude Code does not read the item, so a ccdad that did
// would report a login that Claude Code will never use". The premise is false --
// keychain.go's header carries the measurement -- and with it false, the
// confident wrong answer is the one Load gave by reading the file past an item
// that exists.
//
// A value that is not JSON is an error rather than an empty blob: `security -w`
// hex-encodes data it cannot print, so unparseable output means the item is not
// holding what its name says it holds, and silently reporting "no login" would
// hide that.
func LoadKeychainCredentials(ctx context.Context) (Blob, bool, error) {
	return loadKeychainCredentialsWithin(ctx, keychainReadTimeout)
}

// loadKeychainCredentialsWithin is LoadKeychainCredentials under a caller-chosen
// deadline. The write path reads the item with keychainWriteTimeout: the base a
// switch merges onto, and the read that confirms a write which ran out of time.
func loadKeychainCredentialsWithin(ctx context.Context, bound time.Duration) (Blob, bool, error) {
	secret, ok, err := CredentialKeychainItem().readWithin(ctx, bound)
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

// SurvivesRestart reports whether err names a keychain failure a FRESH PROCESS
// on this machine would hit identically, so replacing the process cannot clear
// it.
//
// errSecInteractionNotAllowed is that failure, and the reason is the scoping
// unit. macOS refuses it per AUDIT SESSION, and a child inherits its parent's:
// the daemon's detach sets SysProcAttr{Setsid: true}, which creates a new POSIX
// session and changes neither the audit session (au_session) nor the Mach
// bootstrap namespace -- and those are what Security.framework consults to
// decide whether it may prompt. So a daemon that replaces itself over this
// fault gets a successor that fails on its own first tick.
//
// Measured 2026-08-31: three consecutive replacements, each failing 1.1 seconds
// after it started, and then a chain with no replacements left for a wedge a
// restart COULD have cleared. The remedy is a restart from a shell that already
// has the keychain, which is a thing only a person can do.
//
// Every other classification is deliberately absent. A locked keychain that a
// user unlocks, a timeout, a `security` that exited silently -- a fresh process
// really might get a different answer to any of those, and claiming otherwise
// would spend the opposite mistake.
func SurvivesRestart(err error) bool {
	var ke *KeychainError
	return errors.As(err, &ke) && ke.failure == failNoInteraction
}

// writeOutcomeUnknown reports whether a failed WRITE may have landed anyway.
//
// Only a CLEAN FAILURE RESPONSE is known not to have landed: `security` exited
// with a status, or the binary was not there to start. Everything else is
// ccdad not getting an answer -- the deadline killed it, its output pipe
// outlived it, a signal ended it, or the spawn failed some other way after it
// may have begun -- and in each the request may already have reached
// securityd. On 2026-11-10 a killed add-generic-password did land. Reading the
// item back costs one lookup; calling an unknown "nothing was written" cost a
// split login.
func writeOutcomeUnknown(err error) bool {
	var ke *KeychainError
	if !errors.As(err, &ke) {
		return false
	}
	switch ke.failure {
	case failTimedOut, failLingering, failSignaled, failOther:
		return ke.code == 0
	}
	return false
}

// UnlockLoginKeychain runs `security unlock-keychain` against the default
// keychain with this process's stdio INHERITED.
//
// INHERITING STDIO IS THE WHOLE DESIGN. `security` asks for the password on the
// TERMINAL -- not through the window server, which is why it works over SSH in
// a session that could never raise a dialog -- so inheriting stdio sends the
// password from the user's keyboard into Apple's own binary. ccdad never reads
// it, never holds it, never writes it anywhere, and never passes `-p`, which is
// the non-interactive form. It sees an exit code.
//
// That is not a formality. This password opens EVERY secret in the login
// keychain -- browser logins, wifi, certificates -- and not just the one item
// ccdad wants. A tool that prompts for it is shaped exactly like a credential
// stealer, so the prompt on screen has to be verifiably Apple's rather than
// ccdad's; securityPath being pinned absolute is the other half of that.
//
// THE DEFAULT KEYCHAIN, not a hardcoded path. `security unlock-keychain` with
// no argument unlocks whatever the user's default is -- login.keychain-db on an
// ordinary machine, and something else on a machine that has said otherwise.
//
// No timeout is imposed on the context. A person is typing.
func UnlockLoginKeychain(ctx context.Context) error {
	if keychainGOOS != "darwin" {
		return ErrKeychainUnsupported
	}
	cmd := exec.CommandContext(ctx, securityPath, "unlock-keychain")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unlocking the login keychain: %w", err)
	}
	return nil
}
