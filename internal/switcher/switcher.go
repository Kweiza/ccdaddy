package switcher

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// stateTimeout bounds the wait for the engine state lock. The only writers are
// this stamp and the daemon's own, both sub-second writes.
const stateTimeout = 5 * time.Second

// ErrNoLogin is a target that cannot become the credentials file's login.
//
// A setup-token account is the ordinary case: `claude setup-token` prints the
// token and deliberately skips saving it, so Claude Code reads it from the
// environment and there is no file to install it into. An api-key account is
// installable, but by a different route entirely — two writes to ~/.claude.json
// and the credentials file — which is the CLI's to perform and which the engine
// never asks for, because an account with no quota windows has nothing for a
// usage-aware ranking to compare.
//
// It is returned BEFORE any lock is taken. Handing cclink a snapshot it will
// refuse would put a doomed switch in front of Claude Code's own refresh.
var ErrNoLogin = errors.New("this account has no Claude Code login to install")

// Installable reports whether an account can become the live login at all.
//
// A SETUP-TOKEN account is stored but has no claudeAiOauth record and no file
// Claude Code would read it from, so there is nothing to install. Left in the
// pool it can rank first and then fail the switch, turning a strategy the user
// asked for into an error they cannot act on — so it is excluded from the
// ranking rather than rejected after winning it. An explicit `switch <ACCT>`
// still names one and still gets the message that says how to use it.
//
// An API-KEY account IS installable: `switch` writes primaryApiKey into
// ~/.claude.json and clears the login in front of it. It is still never chosen
// by the engine, but for a different reason and in a different place —
// strategy.eligible drops identity.KindAPIKey because an account with no quota
// windows has nothing for a usage-aware ranking to compare. Saying so here
// instead would state the same exclusion twice and let the two spellings
// disagree.
//
// The signature takes the (Blob, error) pair a lookup returns, so a caller can
// hand it one call rather than unpacking at every site.
func Installable(creds cclink.Blob, err error) bool {
	if err != nil {
		return false
	}
	if _, hasOAuth := creds["claudeAiOauth"]; hasOAuth {
		return true
	}
	rec, isToken := cclink.TokenRecordOf(creds)
	return isToken && rec.Kind == cclink.APIKeyKind
}

// Outcome is what one Execute did. Everything except Switched left the
// credentials file exactly as it found it.
type Outcome uint8

const (
	// NotSwitched is the zero value, and it is deliberately not Switched:
	// Execute fills the Outcome in as it goes, so a call that returned an error
	// leaves it here. A zero value meaning "switched" would report every
	// failure as a success to any caller that read the Result before the error.
	NotSwitched Outcome = iota
	// Switched: the target is now the credentials file's login.
	Switched
	// AlreadyOn: the target was already the login, decided under the lock.
	AlreadyOn
	// Raced: the live login is no longer the one the decision was made
	// against, so an unattended swap stood down. Attended callers never see
	// this.
	Raced
	// Overridden: CLAUDE_CODE_OAUTH_TOKEN is set, so the swap would change
	// nothing about what Claude Code uses. Unattended callers only.
	Overridden
	// Contended: another ccdad STORE's engine is driving this credential home,
	// so an unattended swap stood down rather than undo its work — which it
	// would then undo back, forever. Unattended callers only; see credhome.
	//
	// Appended at the end of this block on purpose. Nothing persists these
	// numbers today, and appending is what keeps that true if something ever
	// does.
	Contended
)

func (o Outcome) String() string {
	switch o {
	case AlreadyOn:
		return "already on that account"
	case Raced:
		return "the live login changed while the switch was being decided"
	case Overridden:
		return "CLAUDE_CODE_OAUTH_TOKEN is set, so a switch would change nothing"
	case Contended:
		return "another ccdad store's engine is driving this Claude Code login"
	case Switched:
		return "switched"
	default:
		return "the switch did not complete"
	}
}

// Request is one swap.
type Request struct {
	// Target is the account to install.
	Target store.Account

	// LiveUUID is the account the caller observed as live when it decided, or
	// "" for a file it could not attribute.
	//
	// Unattended, it is a PRECONDITION: if the file under the lock names anyone
	// else, the swap stands down with Raced. That guard cannot be replaced by
	// the anti-flap cooldown, because the engine read the cooldown before the
	// hand switch that stamped it.
	//
	// Attended it is ignored. A human typed the command and is watching the
	// result, so a login that changed underneath is something to report, not a
	// reason to refuse what they just asked for.
	LiveUUID string

	// Unattended marks a caller with nobody watching: the daemon, or a cron
	// line. It turns two warnings into refusals — see LiveUUID and Overridden —
	// because a notice printed to a log nobody reads is not a notice.
	Unattended bool

	// Force installs the target even when it is already live. It bypasses the
	// already-on answer and nothing else.
	Force bool
}

// Result is what happened, in facts rather than sentences. The caller owns the
// words: the CLI prints them, the daemon logs them, and neither has to parse
// the other's.
type Result struct {
	Outcome Outcome
	// Target is the account Execute was asked for.
	Target store.Account
	// Live is who the credentials file attributed to under the lock, and
	// LiveKnown whether it attributed at all.
	Live      store.Account
	LiveKnown bool
	// UnknownKeys are the unrecognized top-level keys the unknown-key probe
	// found, read from the file that was actually merged. Merge preserves them;
	// the operator still needs to know a new key exists.
	UnknownKeys []string
	// EnvTokenWins reports that CLAUDE_CODE_OAUTH_TOKEN is set. Attended, the
	// swap still happened and this is the reason it appears not to have.
	EnvTokenWins bool
	// ClearedKey reports that ccdad's own stored primaryApiKey was removed from
	// Claude Code's config, and ClearedKeyOwner whose it was.
	ClearedKey      bool
	ClearedKeyOwner store.Account

	// CooldownErr and KeyErr are failures that happened AFTER the credentials
	// file was written. They are reported rather than returned: the switch is
	// on disk by then, and turning either into Execute's error would report a
	// completed swap as a failure.
	CooldownErr error
	KeyErr      error

	// Claim is what the credential-home claim said at the moment of the write.
	//
	// Unattended, a StandDown here IS the Contended outcome and nothing was
	// written. Attended it is a warning and the swap happened anyway: a human
	// typed the command and is watching, and the useful thing to tell them is
	// that another store's engine is likely to switch it back — not to refuse.
	//
	// Its Notice is set when the claim could not be read definitively, which is
	// never a reason to stand down and always a reason to say something.
	Claim credhome.Verdict
}

// activateWith is the locked read-decide-write, as a var so a test can describe
// a lock takeover without arranging one.
var activateWith = cclink.ActivateWith

// claimVerdict asks who is driving this credential home, as a var so a test can
// describe a second store's engine without starting one.
//
// The stub is a convenience for the OUTCOME tests here, not evidence that the
// mechanism works: internal/credhome proves that against a real second process,
// because a claim is a kernel fact and a fake cannot establish one.
var claimVerdict = credhome.Decide

// Execute performs the swap: the whole sequence, for both callers.
//
// The order is the design, and two steps of it are ordering constraints rather
// than preferences:
//
//   - The already-on answer and the LiveUUID precondition are decided INSIDE
//     Claude Code's credential locks, against the file as it is at the moment
//     of the write. Deciding them from a pre-lock read means deciding from a
//     file that may have been replaced while this call waited for the lock.
//   - SetActive comes after Activate has returned, never around it.
//     store/lock.go names the store lock the OUTER one: a caller may take
//     Claude Code's credential locks while holding it, and nothing may take the
//     store lock while holding a credential lock. Two callers that pick
//     opposite orders deadlock, and reproducing that needs a daemon and a CLI
//     racing.
//
// Nothing is recorded for a write that did not land. A compromised lock means
// Claude Code may have written after us, so SetActive would assert a login that
// is not there and the cooldown would hold the engine off its own retry.
func Execute(s *store.Store, req Request) (Result, error) {
	res := Result{Target: req.Target}

	creds, err := s.Credentials(req.Target.UUID)
	if err != nil {
		return res, err
	}
	// An account can hold both a browser login and a token. The OAuth record is
	// what goes in the credentials file and is the stronger credential, so a
	// token sitting beside it must not divert the switch.
	if _, hasOAuth := creds["claudeAiOauth"]; !hasOAuth {
		return res, fmt.Errorf("%s: %w", req.Target.Label(), ErrNoLogin)
	}

	// Claude Code reads CLAUDE_CODE_OAUTH_TOKEN in preference to the credentials
	// file, so with it set the swap succeeds and changes nothing about what the
	// session authenticates as. Attended that is a note printed once to somebody
	// reading it; unattended it is an engine switching into the void on every
	// evaluation, which has to stop rather than accumulate.
	res.EnvTokenWins = os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != ""
	if res.EnvTokenWins && req.Unattended {
		res.Outcome = Overridden
		return res, nil
	}

	accounts := s.Accounts()
	err = activateWith(func(live cclink.Blob) (cclink.Blob, error) {
		res.UnknownKeys = cclink.UnknownKeys(live)
		// AttributeFile, not AttributeLogin, and deliberately: this asks "is the
		// FILE already this account", because the file is what a switch
		// rewrites. AttributeLogin would answer about the environment instead,
		// which switching does not touch — and that override is reported
		// separately through EnvTokenWins.
		//
		// It is also the only acceptable hysteresis baseline. store.go
		// documents ActiveUUID as a display HINT that goes stale the moment the
		// user runs /login inside Claude Code.
		res.Live, res.LiveKnown = AttributeFile(live, accounts, s.Credentials)

		observed := ""
		if res.LiveKnown {
			observed = res.Live.UUID
		}
		if res.LiveKnown && res.Live.UUID == req.Target.UUID && !req.Force {
			res.Outcome = AlreadyOn
			return nil, cclink.ErrNoChange
		}
		// Decided HERE, under Claude Code's credential locks, for the reason
		// the doc comment gives about the other two preconditions: this call
		// may have waited up to cclink.LockTimeout for each of three locks, and
		// an answer read before that wait is an answer about a machine that has
		// had half a minute to change.
		//
		// Before the LiveUUID precondition, not after. When another store's
		// engine holds the claim it is also the likeliest reason the live login
		// moved, and "another engine is driving this login" is the answer that
		// tells the operator what to do; "the login changed underneath us" only
		// describes the symptom.
		res.Claim = claimVerdict()
		if req.Unattended && res.Claim.StandDown {
			res.Outcome = Contended
			return nil, cclink.ErrNoChange
		}
		if req.Unattended && observed != req.LiveUUID {
			res.Outcome = Raced
			return nil, cclink.ErrNoChange
		}
		return creds, nil
	})
	if errors.Is(err, cclink.ErrNoChange) {
		// A takeover joined onto a stand-down is not damage: nothing was
		// written, so there is no write for another process to have raced.
		return res, nil
	}
	if err != nil {
		return res, err
	}

	res.Outcome = Switched
	if err := s.SetActive(req.Target.UUID); err != nil {
		return res, err
	}
	res.CooldownErr = RecordSwitch(req.Target.UUID)
	res.ClearedKeyOwner, res.ClearedKey, res.KeyErr = releaseManagedAPIKey(s)
	return res, nil
}

// RecordSwitch stamps the anti-flap cooldown after a swap has succeeded.
//
// An EXPLICIT switch stamps it too, and that is the point: the user has just
// chosen an account, and a daemon evaluating ten seconds later must not
// immediately override the choice. Stamping before the swap would let a switch
// that FAILED hold the engine off its own retry.
//
// It is exported because Execute is not the only way an account becomes the
// live credential: installing an api-key account writes ~/.claude.json instead,
// and a cooldown that only some switches stamped would let the engine override
// exactly the ones it did not see.
func RecordSwitch(uuid string) error {
	err := strategy.WithState(stateTimeout, func(st *strategy.State) error {
		st.RecordSwitch(uuid, time.Now())
		return nil
	})
	if err != nil {
		return fmt.Errorf("the auto-switch cooldown could not be recorded: %w", err)
	}
	return nil
}

// releaseManagedAPIKey removes a stored primaryApiKey that belongs to a ccdad
// account, and reports whose it was.
//
// The login just installed outranks any stored API key, so nothing is broken by
// leaving one — but it becomes the credential again the moment that login goes
// away, silently and as a different account.
//
// A key ccdad does not recognise is LEFT ALONE. It came from Claude Code's own
// `/login`, it is inert for as long as the login being installed is live, and
// deleting a credential ccdad did not create is not a side effect a switch gets
// to have.
//
// The peek before the lock is not an optimisation of the ordinary path so much
// as the whole point of it: on a machine with no API-key accounts there is
// nothing to clear, and taking Claude Code's config lock on every switch to
// discover that would put ccdad in the way of a running session for no reason.
// The peek can race a key that appears after it — and the cost of losing that
// race is one stale inert key, which the next switch clears.
func releaseManagedAPIKey(s *store.Store) (store.Account, bool, error) {
	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		return store.Account{}, false, err
	}
	stored, ok := cclink.PrimaryAPIKey(cfg)
	if !ok {
		return store.Account{}, false, nil
	}
	owner, ok := APIKeyOwner(s.Accounts(), s.Credentials, stored)
	if !ok {
		return store.Account{}, false, nil
	}

	var cleared bool
	err = cclink.UpdateGlobalConfig(func(g *cclink.GlobalConfig) error {
		// Re-checked under the lock rather than trusting the peek: Claude Code
		// may have replaced the key while ccdad waited, and clearing THAT one
		// would delete a credential ccdad never installed.
		current, ok := cclink.PrimaryAPIKey(g)
		if !ok || current != stored {
			return nil
		}
		cleared = cclink.ClearPrimaryAPIKey(g)
		return nil
	})
	if err != nil {
		return store.Account{}, false, err
	}
	return owner, cleared, nil
}
