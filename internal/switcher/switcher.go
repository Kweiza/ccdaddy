package switcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// stateTimeout bounds the wait for the engine state lock. The only writers are
// this stamp and the daemon's own, both sub-second writes.
const stateTimeout = 5 * time.Second

// ErrNotClaude is a switch asked to install an account that is not a Claude
// account. This function writes Claude Code's credentials file; a Codex
// account is served by the proxy and has nothing to install here.
var ErrNotClaude = errors.New("switcher: target is not a Claude account")

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
	// Stale: the target's stored credential sits inside the window Claude Code
	// refreshes in, and no refresh could move it out. Nothing was written.
	Stale
	// Unreadable: this machine's login store could not be READ, so whether any
	// account is live is not known -- not even whether anything is logged in.
	// Nothing was written. Appended, for the reason Contended and Stale are.
	Unreadable
	// Unattributed: the credentials file holds an OAuth login this store
	// cannot name, and the caller did not establish that it is foreign. An
	// unattended swap stands down rather than write over what may be a managed
	// account mid-rotation. Unattended callers only.
	Unattributed
)

func (o Outcome) String() string {
	switch o {
	case AlreadyOn:
		return "already on that account"
	case Raced:
		return "the live login changed while the switch was being decided"
	case Overridden:
		return "another OAuth source outranks the credentials file, so a switch would change nothing"
	case Contended:
		return "another ccdad store's engine is driving this Claude Code login"
	case Stale:
		return "that account's stored login is one Claude Code would refresh on sight, and refreshing it here did not succeed"
	case Unattributed:
		return "the credentials file holds a login this store cannot name, and overwriting it could take down an account mid-rotation"
	case Unreadable:
		return "this machine's login store cannot be read, so whether an account is live cannot be established"
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

	// Now is the clock the staleness precondition reads. Nil is time.Now.
	Now func() time.Time

	// Freshen refreshes the target's stored credential and returns what to
	// install in its place. It is called ONLY when the stored one sits inside
	// cclink.SelfRefreshThreshold, and never for the account the caller
	// believes is already live.
	//
	// It is a hook rather than a direct call into the token machinery for two
	// reasons. It reaches the network, and this package's whole ordering
	// discipline is that the network is touched BEFORE Claude Code's
	// credential locks are taken (cclink.ActivateWith says why); putting it
	// behind a field keeps that visible at the one place it is invoked. And a
	// caller with no way to refresh — a test, an offline path — gets the
	// refusal rather than a package that quietly grew a dependency on the
	// token endpoint.
	//
	// Nil means "cannot refresh", which is a refusal, not permission to
	// install what is stored.
	Freshen func(uuid string) (cclink.Blob, error)

	// LiveForeign says the caller POSITIVELY established that the login in the
	// credentials file belongs to no account this store manages -- it resolved
	// the token's owner and the answer was somebody else.
	//
	// It is not "the file did not match a stored snapshot". That is the state
	// a managed account is in the moment Claude Code rotates its refresh token,
	// and an unattended swap that reads it as "nobody is live" installs over a
	// running session. Only a caller that has RESOLVED the login may set this,
	// and setting it without resolving reintroduces the loop by hand.
	//
	// Attended callers do not need it: a human is watching, so an unnameable
	// login is something to report rather than a reason to refuse.
	//
	// It is a finding about the file the caller READ, so it is checked against
	// the file under the lock like LiveUUID is: a file that has become a
	// managed account's since then is a race, not a foreign login.
	LiveForeign bool
}

func (r Request) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
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
	//
	// LiveState is the same read without the collapse, and it is the one a
	// decision to WRITE has to consult: LiveKnown false spans both "nobody is
	// logged in" and "a login this store cannot name", which want opposite
	// answers.
	Live      store.Account
	LiveKnown bool
	LiveState LiveState
	// UnknownKeys are the unrecognized top-level keys the unknown-key probe
	// found, read from the file that was actually merged. Merge preserves them;
	// the operator still needs to know a new key exists.
	UnknownKeys []string
	// EnvTokenWins reports that SOMETHING outranks the credentials file on
	// Claude Code's OAuth axis, so the swap changed nothing about what a session
	// authenticates as. Attended, the swap still happened and this is the reason
	// it appears not to have.
	//
	// It was one variable once, and the name is kept because the outcome it
	// drives is. What it is NOT any more is a variable: DisplacedBy names the
	// source, and three of the sources it can name have no variable at all --
	// printing "unset CLAUDE_CODE_OAUTH_TOKEN" for those is the failure this
	// field's own consumers were widened to stop.
	EnvTokenWins bool
	// DisplacedBy is the source that outranks the file, valid when EnvTokenWins.
	// It is what every message about this state must be written from: it carries
	// its own wording and its own remedy, and they differ per source.
	DisplacedBy identity.OAuthSource
	// DisplacedUnresolved reports that ccdad DECLINED rather than named a
	// source -- the bg-auth snapshot, which is a credential ccdad refuses to
	// read and which Claude Code consumes before anything else. The swap is
	// stood down for it because "cannot tell" and "something wins" have the same
	// consequence for an unattended engine.
	DisplacedUnresolved bool
	// ClearedKey reports that ccdad's own stored primaryApiKey was removed from
	// Claude Code's config, and ClearedKeyOwner whose it was.
	ClearedKey      bool
	ClearedKeyOwner store.Account

	// FreshenErr is why the Stale outcome could not be repaired, when a
	// Freshen hook was wired and failed. Nil with a Stale outcome means either
	// no hook was wired or the refresh succeeded and still came back inside
	// Claude Code's refresh window.
	FreshenErr error

	// CooldownErr, KeyErr, and ProfileSyncErr are failures that happened AFTER
	// the credentials file was written. They are reported rather than
	// returned: the switch is on disk by then, and turning any of them into
	// Execute's error would report a completed swap as a failure.
	CooldownErr error
	KeyErr      error
	// ProfileSyncErr is a failure to keep ~/.claude.json's oauthAccount
	// pointed at Target -- see SyncGlobalConfigIdentity. Left nil (and
	// SyncGlobalConfigIdentity never called) when EnvTokenWins: something
	// else is what a session actually authenticates as, and writing Target's
	// identity there would misdescribe it.
	ProfileSyncErr error

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

	// Ahead of the credential read, and that ordering is the whole point. A
	// Codex account's credential file exists and is readable, so the read
	// succeeds and the refusal below it would be ErrNoLogin -- "this account
	// has no Claude Code login" -- which is true and sends the user to `ccdad
	// add` for an account that is logged in perfectly well. Nothing further
	// down this function has any business running for a target it cannot
	// install.
	if req.Target.Provider != provider.Claude {
		return res, ErrNotClaude
	}

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

	// Several things outrank the credentials file, so with any of them present
	// the swap succeeds and changes nothing about what the session
	// authenticates as. Attended that is a note printed once to somebody
	// reading it; unattended it is an engine switching into the void on every
	// evaluation, which has to stop rather than accumulate.
	//
	// RESOLVED AGAINST A WORKING LOGIN, not against the one in the file. This
	// runs BEFORE the swap and the swap is about to write a login, so whether
	// that write survives cannot depend on what the file holds now -- a machine
	// whose current login is scope-less would otherwise resolve to "no source"
	// and the engine would switch into a void it had just been told about.
	//
	// The apiKeyHelper is left unfilled, as this package has no reader for
	// Claude Code's settings tree. That is unchanged from when this line read
	// one variable: the gate never saw a helper, and doctor is where one is
	// reported.
	oauthSource, resolved := identity.ProbeOAuthEnvironment().Resolve(identity.UsableLogin)
	res.DisplacedUnresolved = !resolved
	res.EnvTokenWins = !resolved ||
		(oauthSource != identity.OAuthNone && oauthSource != identity.OAuthLogin)
	if resolved {
		res.DisplacedBy = oauthSource
	}
	if res.EnvTokenWins && req.Unattended {
		res.Outcome = Overridden
		return res, nil
	}

	// The staleness precondition, and it runs HERE: after the displacement
	// gate, so a swap that was going to change nothing does not spend a
	// refresh grant to do it, and before activateWith, because Freshen reaches
	// the network and cclink.ActivateWith forbids that under Claude Code's
	// credential locks.
	//
	// Installing a credential inside Claude Code's own refresh window is what
	// turns one swap into a loop. Claude Code refreshes it on sight, the
	// rotation moves the refresh token out from under the copy in ccdad's
	// store, AttributeFile stops matching, the next evaluation reads "nobody
	// is live", and the same dead snapshot goes back in. Every pass re-presents
	// a superseded grant until the server rejects the family and BOTH sides are
	// logged out.
	//
	// The target the caller believes is already live is exempt, and that is
	// not an optimisation. Refreshing that account here would rotate the grant
	// underneath a running session — the hazard the whole path is built to
	// avoid — for a call whose answer is about to be AlreadyOn anyway.
	if req.LiveUUID != req.Target.UUID && cclink.WouldSelfRefresh(creds, req.now()) {
		if req.Freshen == nil {
			res.Outcome = Stale
			return res, nil
		}
		fresh, ferr := req.Freshen(req.Target.UUID)
		// A failed refresh is not this call's error to report. The account is
		// simply not installable right now, which is an outcome the caller
		// acts on by choosing someone else — turning it into an error would
		// make the daemon log a failure every tick for a state that repairs
		// itself on the next poll.
		if ferr != nil || cclink.WouldSelfRefresh(fresh, req.now()) {
			res.Outcome = Stale
			res.FreshenErr = ferr
			return res, nil
		}
		creds = fresh
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
		res.Live, res.LiveState = LiveStateOf(live, accounts, s.Credentials)
		res.LiveKnown = res.LiveState == LiveManaged

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
		// After the race check, not before, and the order carries the meaning.
		// Reaching here means the file is in the state the caller decided
		// against; the question left is whether that state is one an
		// unattended swap may overwrite.
		//
		// It may not, unless the caller resolved the login and found it
		// foreign. The equality above cannot stand in for this: an
		// unattributable file gives observed == "" and a caller that could not
		// attribute either passes LiveUUID == "", so the two agree and the
		// guard waves through the one write that must never happen.
		if req.Unattended && res.LiveState == LiveUnattributed && !req.LiveForeign {
			res.Outcome = Unattributed
			return nil, cclink.ErrNoChange
		}
		return creds, nil
	})
	if errors.Is(err, cclink.ErrNoChange) {
		// A takeover joined onto a stand-down is not damage: nothing was
		// written, so there is no write for another process to have raced.
		if res.Outcome == AlreadyOn && !res.EnvTokenWins {
			// The credentials file already named Target before this call, but
			// ~/.claude.json's cached display may still name whoever was live
			// before THAT switch -- Claude Code never self-corrects it (see
			// SyncGlobalConfigIdentity). Checking on every AlreadyOn, not only
			// a real Switched, is the only path that ever fixes it once it has
			// drifted.
			res.ProfileSyncErr = SyncGlobalConfigIdentity(s, req.Target, res.Live, res.LiveKnown)
		}
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
	if !res.EnvTokenWins {
		res.ProfileSyncErr = SyncGlobalConfigIdentity(s, req.Target, res.Live, res.LiveKnown)
	}
	return res, nil
}

// SyncGlobalConfigIdentity keeps ~/.claude.json's oauthAccount pointed at
// whichever account this call just confirmed live -- target.
//
// Claude Code's own token-refresh handler enriches oauthAccount's cosmetic
// fields (displayName, billingType, the trial and onboarding flags...) but
// never rewrites accountUuid, emailAddress, or organizationUuid once the
// object exists (see cclink's oauthaccount.go). A switch that only replaces
// the credentials file therefore leaves Claude Code DISPLAYING whoever was
// live before, forever -- even though the credentials file, and everything
// Claude Code actually authenticates and meters with, is already target's.
//
// If target has never been synced before, oauthAccount is reset to a minimal
// object naming just who it is; if a real snapshot was captured the last time
// ccdad switched away from target, that exact object is restored instead.
// Either way, if the object already names target, it is left untouched --
// touching it here would discard any cosmetic enrichment Claude Code's own
// refresh has since added, for no gain.
//
// previousLive and previousLiveKnown are what the caller believes was live
// immediately before this call; Execute already decides this under Claude
// Code's credential locks. They are used only to decide whose snapshot to
// back the displaced object up as, so target's own correctness never depends
// on a caller supplying them -- a caller that cannot loses only that
// courtesy. It is exported for the one other attended path that installs a
// login without going through Execute: `ccdad add --activate`.
func SyncGlobalConfigIdentity(s *store.Store, target, previousLive store.Account, previousLiveKnown bool) error {
	var captured json.RawMessage
	var capturedUUID string

	err := cclink.UpdateGlobalConfig(func(g *cclink.GlobalConfig) error {
		raw, hadCaptured := cclink.OAuthAccountSnapshot(g)
		if hadCaptured {
			if uuid, ok := cclink.OAuthAccountUUID(raw); ok {
				captured, capturedUUID = raw, uuid
				if uuid == target.UUID {
					return nil
				}
			}
		}
		if target.OAuthAccountSnapshot != "" {
			return cclink.RestoreOAuthAccountSnapshot(g, json.RawMessage(target.OAuthAccountSnapshot))
		}
		return cclink.ResetOAuthAccountIdentity(g, cclink.AccountIdentity{
			UUID:             target.UUID,
			Email:            target.Email,
			OrganizationUUID: target.OrganizationUUID,
			OrganizationType: target.Tier,
			RateLimitTier:    target.RateLimitTier,
			SeatTier:         target.SeatTier,
		})
	})
	if err != nil {
		return err
	}

	// The backup is only trustworthy when the captured object actually named
	// previousLive: with this fix rolling out onto a machine whose display has
	// been stale across SEVERAL real switches already, the object captured
	// here can belong to an account further back than previousLive, and
	// filing it under previousLive's slot would plant a wrong accountUuid
	// there for a future restore to reinstall.
	if capturedUUID == "" || capturedUUID != previousLive.UUID {
		return nil
	}
	if !previousLiveKnown || previousLive.UUID == target.UUID {
		return nil
	}
	if previousLive.OAuthAccountSnapshot == string(captured) {
		return nil
	}
	return s.SetOAuthAccountSnapshot(previousLive.UUID, captured)
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
