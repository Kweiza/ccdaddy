package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Kweiza/ccdaddy/internal/browser"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// pastePrompt is printed by the announcement and again after every unreadable
// line, so the two cannot drift into saying different things.
const pastePrompt = "Paste code here: "

// quarantineLiftTimeout bounds the wait for the engine state lock. The only
// other writer is the daemon stamping a cooldown, which holds it for a
// sub-second write, so this is generous already.
const quarantineLiftTimeout = 5 * time.Second

// liftQuarantine clears an auto-switch quarantine after a successful add.
//
// The quarantine fires on a dead refresh token, and re-authenticating is the
// only thing that fixes one. store.Add updates an existing uuid IN PLACE, so
// without this the user logs in again, is told it worked, and the engine goes
// on refusing to use the account with nothing anywhere saying why.
//
// It never fails the command. The account is added by the time this runs, and
// returning an error here would report a successful login as a failure.
func liftQuarantine(stderr io.Writer, uuid string) {
	var lifted bool
	// The lock is taken unconditionally rather than after a lock-free peek: a
	// peek would race a quarantine landing from the refresh that used the OLD
	// credential, which is exactly the one this is here to clear.
	if err := strategy.WithState(quarantineLiftTimeout, func(st *strategy.State) error {
		lifted = st.ClearQuarantine(uuid)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "note: the auto-switch state could not be updated (%v); "+
			"if this account was quarantined it stays out of rotation until that is fixed.\n", err)
		return
	}
	if lifted {
		fmt.Fprintln(stderr, "Lifted this account's auto-switch quarantine.")
	}
}

// Injected so a test can describe a machine it is not running on. A real
// terminal and a real browser are not things a test can arrange, and the two
// behaviours that hang on them — refusing up front, and waiting on the loopback
// alone — are the reason this command exists.
var (
	stdinIsTTY       = func() bool { return isTTY(os.Stdin) }
	browserAvailable = browser.Available
	// profileBaseURL is a seam so a test can prove the API-key path never dials
	// the profile endpoint, and can drive the rejected-token branch.
	profileBaseURL = oauth.APIBaseURL
	// login is a seam for everything that happens AFTER a successful login.
	// Without it none of it is reachable from a test: a real login needs a
	// browser and a five-minute wait, so the activate gate, the credential
	// record and the re-authentication key carry would all be untested — and
	// each of those decides what happens to a live credential.
	login = oauth.Login
	// readPassword reads the token without echoing it. It is a var so the
	// prompt path is reachable from a test at all — but note what that does and
	// does not buy: a test can drive the branch and check where the prompt is
	// written, and it CANNOT verify the terminal echo is really off, because
	// that needs a pty. The no-echo property is held by term.ReadPassword and
	// by review, not by this suite.
	readPassword = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) }
)

func newProfileClient() *identity.Client {
	c := identity.NewClient()
	c.BaseURL = profileBaseURL
	return c
}

func newAddCmd() *cobra.Command {
	var (
		useClaudeAI bool
		useConsole  bool
		noBrowser   bool
		alias       string
		activate    bool
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add [ALIAS]",
		Short: "Log in to a Claude account and manage it",
		Long: "Opens a browser and completes the login over a loopback callback.\n" +
			"If the browser cannot open, paste the code#state value the login page shows.\n" +
			"Both paths run at once, so there is nothing to retry.\n\n" +
			"Adding an account does not switch to it. Pass --activate to do both.",
		Args:          usageArgs(cobra.MaximumNArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if useClaudeAI && useConsole {
				return UsageError("--claudeai and --console are mutually exclusive")
			}
			if len(args) == 1 {
				// Cobra parses the flag first and RunE would silently overwrite
				// it, so giving both is named rather than resolved.
				if alias != "" && alias != args[0] {
					return UsageError("give the alias once: either as an argument or as --alias, not both")
				}
				alias = args[0]
			}
			if alias != "" {
				if err := store.ValidateAlias(alias); err != nil {
					return UsageError("%s", err.Error())
				}
				// Checked before the login, not only after it. A collision found
				// afterwards cannot fail the command — the account is stored and
				// the browser round trip really did succeed — so catching it here
				// is the only place it can still be a clean usage error.
				if err := aliasIsFree(alias); err != nil {
					return err
				}
			}

			surface := oauth.SurfaceClaudeAI
			if useConsole {
				surface = oauth.SurfaceConsole
			}

			// The SIGINT trap is scoped to this command and to the span of its
			// own blocking login. `add` waits minutes on a browser callback or a
			// pasted code, and oauth.Login unwinds its loopback listener when
			// the context is cancelled — that arm is only reachable with the
			// trap installed. Installing it process-wide instead would strip
			// SIGINT's default disposition from every other command while none
			// of them watch the context, which makes Ctrl-C do nothing at all.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			cmd.SetContext(ctx)

			return runAdd(cmd, addOptions{
				surface:    surface,
				tryBrowser: !noBrowser,
				alias:      alias,
				activate:   activate,
				timeout:    timeout,
			})
		},
	}

	cmd.Flags().BoolVar(&useClaudeAI, "claudeai", false, "use the subscription login (default)")
	cmd.Flags().BoolVar(&useConsole, "console", false, "use the Console login, for a credit-billed account")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not open a browser; print the URL and wait for a pasted code")
	cmd.Flags().StringVar(&alias, "alias", "", "short handle for the account")
	cmd.Flags().BoolVar(&activate, "activate", false, "switch to the account once it is added")
	cmd.Flags().DurationVar(&timeout, "timeout", oauth.DefaultLoginTimeout, "how long to wait for the login")
	return cmd
}

type addOptions struct {
	surface    oauth.Surface
	tryBrowser bool
	alias      string
	activate   bool
	timeout    time.Duration
}

func runAdd(cmd *cobra.Command, opts addOptions) error {
	stderr := cmd.ErrOrStderr()

	// A machine with no browser and no terminal has no way to complete a login.
	// Say so before spending five minutes waiting.
	canPaste := stdinIsTTY()
	canOpen := opts.tryBrowser && browserAvailable()
	if !canPaste && !canOpen {
		return WithCode(
			fmt.Errorf("no browser is available and stdin is not a terminal, so there is no way to complete a login here.\n"+
				"Run 'ccdad add-token' with a token from another machine instead"),
			ExitBlocked)
	}

	var paste oauth.PasteSource
	if canPaste {
		paste = oauth.StdinPaste()
	}

	result, err := login(cmd.Context(), oauth.LoginOptions{
		Surface:     opts.surface,
		Timeout:     opts.timeout,
		OpenBrowser: canOpen,
		Paste:       paste,
		Announce: func(manualURL string) {
			if canOpen {
				fmt.Fprintln(stderr, "Opening your browser to sign in.")
				fmt.Fprintln(stderr, "If it does not open, visit this URL and paste the code it shows:")
			} else {
				fmt.Fprintln(stderr, "Visit this URL to sign in, then paste the code it shows:")
			}
			fmt.Fprintf(stderr, "\n  %s\n\n", manualURL)
			if canPaste {
				fmt.Fprint(stderr, pastePrompt)
			} else {
				fmt.Fprintln(stderr, "(stdin is not a terminal, so ccdad is waiting on the browser callback only.)")
			}
		},
		// An unreadable paste is a re-prompt, not an abort: the loopback race
		// may still be about to win. Without this the line is swallowed in
		// silence and the user is left at a bare cursor with nothing saying
		// ccdad is still waiting.
		//
		// The prompt is re-issued unconditionally rather than behind canPaste.
		// This is reached only when a line arrived, and a line can only arrive
		// from the Paste source, which exists only when stdin is a terminal.
		Rejected: func(msg string) {
			fmt.Fprintf(stderr, "\n%s\n%s", msg, pastePrompt)
		},
	})
	if err != nil {
		return loginError(stderr, err)
	}

	// The exchange response already carries the email, so labelling the account
	// costs no extra request. The profile call is still worth making: it is what
	// classifies the account as subscription- or credit-billed.
	profile, profileErr := newProfileClient().FetchProfile(cmd.Context(), result.Token.AccessToken)
	if profileErr != nil {
		fmt.Fprintf(stderr, "warning: could not read the account profile (%v); the tier will fill in on the first usage refresh\n", profileErr)
	}

	acct := store.Account{
		UUID:  result.Token.Account.UUID,
		Email: result.Token.Account.EmailAddress,
		// No usage call has been made, so UsageShape is empty by fact rather
		// than by omission: Classify reads that as "no window evidence" and
		// only a metered billing_type can still make it credit. An overage
		// switch on a subscription org is not evidence.
		Kind: identity.Classify(profile, identity.UsageShape{}, false),
	}
	if profile != nil {
		if acct.UUID == "" {
			acct.UUID = profile.AccountUUID
		}
		if acct.Email == "" {
			acct.Email = profile.Email
		}
		acct.Tier = profile.OrganizationType
		acct.RateLimitTier = profile.RateLimitTier
		acct.OrganizationUUID = profile.OrganizationUUID
	}
	if acct.UUID == "" {
		return fmt.Errorf("the login did not identify an account; try again")
	}

	s, err := store.Open()
	if err != nil {
		return err
	}
	_, existed := s.Get(acct.UUID)

	// The stored record this account had before this login, if any. Reading it
	// can fail because there is none (a first-time account) or because it is
	// corrupt; both mean the same thing here — start from nothing — so the
	// error is deliberately ignored rather than distinguished.
	prior, _ := s.Credentials(acct.UUID)
	creds, err := credentialBlob(result.Token, profile, prior)
	if err != nil {
		return err
	}

	// add doubles as re-authentication in place. When the account being
	// re-authenticated is the one already in the live file, its other
	// account-scoped keys — trustedDeviceToken, enterpriseGateway, designOauth —
	// are sitting there and are cheap to keep. Losing them costs a device-cap
	// slot and a gateway re-trust.
	//
	// Whether the live file IS this account is decided by comparing its OAuth
	// record against the one this account last stored, and NOT by
	// store.ActiveUUID: that is ccdad's own record of what ccdad last
	// activated, which the store documents as a display hint. It goes stale the
	// moment the user runs `/login` inside Claude Code, and trusting it there
	// copies THAT account's device and design tokens into this account's
	// snapshot — a cross-account leak, which is worse than the loss this carry
	// exists to prevent.
	live, liveErr := cclink.Load()
	liveIsThisAccount := liveErr == nil &&
		switcher.CredentialIdentity(live) != "" &&
		switcher.CredentialIdentity(live) == switcher.CredentialIdentity(prior)

	// The comparison above cannot answer for an ADOPTION -- the first `ccdad
	// add` for an account Claude Code was already logged in as. There is no
	// prior record to compare against, so liveIsThisAccount is false, and the
	// keys sitting in the live file are dropped with a warning. That is the
	// single most common first run there is, and the keys it drops are the
	// expensive ones: a device-cap slot and a gateway re-trust.
	//
	// One profile call settles it. Nothing in the credentials file names an
	// account, but the account endpoint does, so resolving the LIVE login's own
	// access token says whose those keys are instead of guessing.
	//
	// It is worth exactly one request and no more, so it is gated three ways:
	// only when the cheap comparison already failed, only when there is
	// something to carry, and only when the live record actually has a token to
	// ask with. A dead or expired live token, an offline machine, or a
	// cancelled context all land on "cannot tell" and keep the old behaviour --
	// the warning -- rather than failing an otherwise successful login.
	if !liveIsThisAccount && len(carriableKeys(live)) > 0 {
		switch liveLoginOwner(cmd, acct.UUID, live) {
		case liveOwnershipSame:
			liveIsThisAccount = true
		case liveOwnershipOther:
			fmt.Fprintf(stderr,
				"note: the current login belongs to a different account, so its %s are not being stored.\n",
				strings.Join(carriableKeys(live), " and "))
			// Reported here rather than falling through to the warning below,
			// which says ccdad cannot tell. It can, now, and saying otherwise
			// would send someone looking for a problem that was just resolved.
			live = nil
		}
	}

	// This account's OWN previously stored keys carry forward unconditionally.
	// There is no ambiguity about whose they are, and store.Add replaces the
	// credential file wholesale — so without this, re-authenticating an account
	// that is not currently live deletes its trustedDeviceToken and designOauth.
	for k, v := range cclink.Extract(prior) {
		if _, fresh := creds[k]; !fresh {
			creds[k] = v
		}
	}

	if liveIsThisAccount {
		for k, v := range cclink.Extract(live) {
			if _, fresh := creds[k]; !fresh {
				creds[k] = v
			}
		}
	} else if orphaned := carriableKeys(live); len(orphaned) > 0 {
		// Nothing in the credentials file names an account, so when the live
		// login is not provably this one — above all when adopting the login
		// Claude Code already had — ccdad cannot tell whose these are and will
		// not guess. Say so: the first switch away deletes them.
		fmt.Fprintf(stderr,
			"warning: the current login carries %s, and ccdad cannot tell which account they belong to, so they are not being stored.\n"+
				"They will be dropped by the next switch; the account they belong to may need re-trusting.\n",
			strings.Join(orphaned, ", "))
	}

	if err := s.Add(acct, creds); err != nil {
		return err
	}
	liftQuarantine(stderr, acct.UUID)
	if err := applyAlias(cmd, s, acct.UUID, opts.alias); err != nil {
		return err
	}

	saved, ok := s.Get(acct.UUID)
	if !ok {
		return fmt.Errorf("the account was stored but cannot be read back; the store may be corrupt")
	}
	verb := "Added"
	if existed {
		verb = "Re-authenticated"
	}
	fmt.Fprintf(stderr, "\n%s %s (%s, index %d).\n", verb, saved.Label(), saved.Kind, saved.Idx)

	if opts.activate {
		// --activate IS a switch, so the unknown-key probe belongs here too.
		if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
			fmt.Fprintf(stderr,
				"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
				strings.Join(unknown, ", "))
		}
		if err := cclink.Activate(creds); err != nil {
			return err
		}
		if err := s.SetActive(saved.UUID); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Switched to %s.\n", saved.Label())
		// --activate writes the live credentials file without going through
		// switcher.Execute, so the claim has to be asked here or this is the
		// one attended write that never mentions a second store's engine.
		noteCredentialHomeClaim(cmd, credhome.Decide())
	} else {
		fmt.Fprintf(stderr, "Run 'ccdad switch %d' to use it.\n", saved.Idx)
	}
	return nil
}

// loginError maps the login sentinels onto the exit contract. An interrupted
// login is SIGINT's code, not a generic failure, because a supervisor keys on
// 130 to tell "the operator stopped it" from "it broke".
//
// It also carries out the other half of what parsing the authorize rejection
// was for. The callback's own bytes are long gone by here — Rejection is a
// closed enum and LogDetail is one of ccdad's own literals for it.
//
// UserMessage keeps declining and an upstream wobble apart on its own, and it
// already carries retryability: the upstream arm ends "try again shortly". What
// it fuses is RejectionRefused and RejectionUnrecognized, which share its
// default arm — "Anthropic refused the login" is the whole of what a user sees
// for either. Those are different problems: a refused authorize request means
// the request ccdad sent was wrong and will be wrong again, while a code
// outside RFC 6749's set means the endpoint said something ccdad does not
// model, and its bytes are deliberately withheld. The OAuth error code is the
// only thing left that tells the two apart, so it goes to stderr beside the
// message.
func loginError(stderr io.Writer, err error) error {
	var rejected *oauth.RejectionError
	if errors.As(err, &rejected) {
		fmt.Fprintf(stderr, "note: the authorization callback reported %s.\n", rejected.LogDetail())
	}
	switch {
	case errors.Is(err, oauth.ErrLoginInterrupted):
		return WithCode(err, ExitInterrupted)
	case errors.Is(err, oauth.ErrLoginTimeout):
		// The exit contract makes a login timeout a runtime failure, exit 1.
		// Exit 4 is arguably the better fit for "wanted, no viable target", but
		// a silent divergence in an exit code is exactly what the contract
		// exists to prevent. Raise it against README's "Exit codes" first if it
		// should change.
		return WithCode(err, ExitFailure)
	}
	return err
}

// subscriptionTypeOf maps the profile's organization_type to the short name
// Claude Code stores, mirroring its own four-entry table:
//
//	[["claude_max","max"],["claude_pro","pro"],
//	 ["claude_enterprise","enterprise"],["claude_team","team"]]
//
// Every tier predicate in Claude Code compares against the short name — the Max
// check is literally `subscriptionType === "max"` — so storing the raw value
// makes a paid entitlement invisible to the running client. An organization
// type the table does not cover yields nil rather than the raw string, matching
// what Claude Code writes when its Map misses.
func subscriptionTypeOf(organizationType string) any {
	switch organizationType {
	case "claude_max":
		return "max"
	case "claude_pro":
		return "pro"
	case "claude_enterprise":
		return "enterprise"
	case "claude_team":
		return "team"
	}
	return nil
}

// carriableKeys names the account-scoped keys the live file holds besides the
// OAuth login itself — the ones a switch will delete and that only a correct
// attribution could have preserved.
func carriableKeys(live cclink.Blob) []string {
	var out []string
	for _, k := range cclink.AccountScopedKeys {
		if k == "claudeAiOauth" {
			continue
		}
		if _, ok := live[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// applyAlias routes an alias through SetAlias rather than through Account.Alias.
//
// SetAlias is the only path that enforces uniqueness, and it is also what lets
// an explicit --alias re-label an account being re-authenticated: store.Add
// deliberately preserves the stored alias over the incoming one, so assigning
// Account.Alias would silently discard --alias on every re-auth.
func applyAlias(cmd *cobra.Command, s *store.Store, uuid, alias string) error {
	normalized := store.NormalizeAlias(alias)
	if normalized == "" {
		return nil
	}
	err := s.SetAlias(uuid, normalized)
	if err == nil {
		return nil
	}
	// The account is already stored at this point, so the command did not fail:
	// reporting exit 2 would tell a caller the login was rejected when it was
	// not, and re-running it would repeat the whole browser round trip for
	// nothing. Say what did not happen and leave the account in place.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: the account was added but the alias was not set (%v). Set another with 'ccdad alias'.\n", err)
	return nil
}

// aliasIsFree reports whether an alias can still be taken, without touching the
// store's contents. It exists so `add` can refuse a collision BEFORE spending a
// login on it.
func aliasIsFree(alias string) error {
	normalized := store.NormalizeAlias(alias)
	if normalized == "" {
		return nil
	}
	s, err := store.Open()
	if err != nil {
		return err
	}
	for _, a := range s.Accounts() {
		if store.NormalizeAlias(a.Alias) == normalized {
			return UsageError("%s: %q already belongs to %s (%s)", store.ErrAliasTaken, normalized, a.Label(), a.UUID)
		}
	}
	return nil
}

// credentialBlob renders a token response into the account-scoped shape Claude
// Code expects under claudeAiOauth.
//
// prior is the account's previously stored claudeAiOauth, or nil. Fields it
// carries that this exchange did not return are preserved rather than dropped:
// clauth's typed struct destroyed refreshTokenExpiresAt, rateLimitTier and
// clientId on every re-serialize, and clientId is what a revocation request
// needs. Replacing the record wholesale on every `ccdad add` would reintroduce
// that exact failure one layer above cclink.
func credentialBlob(tok *oauth.TokenResponse, profile *identity.Profile, prior cclink.Blob) (cclink.Blob, error) {
	payload := map[string]any{}
	if raw, ok := prior["claudeAiOauth"]; ok {
		// Best effort: a prior record we cannot parse is simply replaced by the
		// one we just obtained, which is strictly better than failing the login.
		_ = json.Unmarshal(raw, &payload)
	}

	now := time.Now().UnixMilli()
	payload["accessToken"] = tok.AccessToken
	payload["refreshToken"] = tok.RefreshToken
	payload["expiresAt"] = now + tok.ExpiresIn*1000
	// clientId is deliberately NOT written. Claude Code's login sets
	// `clientId: t?.oauthClient?.clientId`, which is undefined for the default
	// public client, so the key is absent from a first-party record — and its
	// absence is load-bearing on refresh: Claude Code computes
	// `d = Boolean((IZ(f.scopes) || f.subscriptionType) && !f.clientId)` and
	// only when d is true does it send the curated refresh scope set. A
	// synthesized clientId flips d to false, making Claude Code refresh with
	// the raw stored scopes including org:create_api_key — the exact scope the
	// refresh grant drops. A clientId that a non-default client really did
	// store survives through the prior merge above.
	if tok.RefreshTokenExpiresIn > 0 {
		payload["refreshTokenExpiresAt"] = now + tok.RefreshTokenExpiresIn*1000
	}
	if tok.Scope != "" {
		payload["scopes"] = strings.Fields(tok.Scope)
	}
	// subscriptionType and rateLimitTier follow Claude Code's own normalizer,
	// which resolves each as `fresh ?? stored ?? null` (function hjd in the
	// 2.1.238 bundle) — so a value we do not have falls back to the stored one,
	// and an explicit null is written only when neither side knows.
	if profile != nil && profile.OrganizationType != "" {
		payload["subscriptionType"] = subscriptionTypeOf(profile.OrganizationType)
	}
	if profile != nil && profile.RateLimitTier != "" {
		payload["rateLimitTier"] = profile.RateLimitTier
	}
	for _, k := range []string{"subscriptionType", "rateLimitTier"} {
		if _, ok := payload[k]; !ok {
			payload[k] = nil
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the captured login: %w", err)
	}
	return cclink.Blob{"claudeAiOauth": encoded}, nil
}

func newAddTokenCmd() *cobra.Command {
	var (
		email    string
		alias    string
		activate bool
	)

	cmd := &cobra.Command{
		Use:   "add-token [TOKEN|-]",
		Short: "Register a token directly, with no browser",
		Long: "Registers an sk-ant-oat... setup token or an sk-ant-api... API key.\n" +
			"Use this on a headless machine, or when a token came from somewhere else.\n" +
			"'-' reads the token from stdin; with no argument ccdad prompts without echoing.\n\n" +
			"An API key can be made the live credential: --activate writes it to Claude Code's\n" +
			"config as primaryApiKey and removes the OAuth login sitting in front of it.\n" +
			"A setup token cannot — Claude Code reads one from CLAUDE_CODE_OAUTH_TOKEN only,\n" +
			"so run Claude Code with that variable exported instead.",
		Args:          usageArgs(cobra.MaximumNArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if alias != "" {
				if err := store.ValidateAlias(alias); err != nil {
					return UsageError("%s", err.Error())
				}
			}
			token, err := readToken(cmd, args)
			if err != nil {
				return err
			}
			isAPIKey, err := detectTokenType(token)
			if err != nil {
				return UsageError("%s", err.Error())
			}
			return runAddToken(cmd, strings.TrimSpace(token), isAPIKey, email, alias, activate)
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "label for the account (optional)")
	cmd.Flags().StringVar(&alias, "alias", "", "short handle for the account")
	cmd.Flags().BoolVar(&activate, "activate", false, "make an API key the live credential (not available for a setup token)")
	return cmd
}

func readToken(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 1 && args[0] != "-" {
		return args[0], nil
	}
	if len(args) == 1 && args[0] == "-" {
		var line string
		if _, err := fmt.Fscanln(cmd.InOrStdin(), &line); err != nil {
			// Nothing on stdin is a caller mistake, not a runtime failure. The
			// scan error is deliberately not wrapped: it adds nothing a user can
			// act on, and a token containing spaces could otherwise appear in an
			// "expected newline" message.
			return "", UsageError("no token on stdin")
		}
		return line, nil
	}
	if !stdinIsTTY() {
		return "", UsageError("no token given; pass it as an argument or as '-' to read stdin")
	}
	// The prompt goes to stderr so `ccdad add-token > file` does not capture it.
	fmt.Fprint(cmd.ErrOrStderr(), "Token: ")
	// Read without echoing: the token must not reach the scrollback.
	raw, err := readPassword()
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("reading the token: %w", err)
	}
	return string(raw), nil
}

// detectTokenType classifies a raw token by its prefix. No network call is made.
func detectTokenType(token string) (isAPIKey bool, err error) {
	t := strings.TrimSpace(token)
	switch {
	case strings.HasPrefix(t, "sk-ant-oat"):
		return false, nil
	case strings.HasPrefix(t, "sk-ant-api"):
		return true, nil
	default:
		return false, fmt.Errorf("that does not look like a Claude token; expected it to start with sk-ant-oat or sk-ant-api")
	}
}

// syntheticEmail labels an account whose token cannot be resolved to one.
//
// The label is derived from the token's own fingerprint rather than from the
// account count: a count changes when an unrelated account is removed and the
// store recompacts, so a counted label would churn on an account that never
// changed, and store.Add would write the new wrong value over the old one.
func syntheticEmail(isAPIKey bool, fingerprint string) string {
	if isAPIKey {
		return fmt.Sprintf("api-key-%s@token.local", fingerprint)
	}
	return fmt.Sprintf("setup-token-%s@token.local", fingerprint)
}

// shortHash gives a stable synthetic id for a token that cannot name itself.
// It is a fingerprint, never a way back to the token.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func runAddToken(cmd *cobra.Command, token string, isAPIKey bool, email, alias string, activate bool) error {
	stderr := cmd.ErrOrStderr()
	fingerprint := shortHash(token)

	kind := "setup-token"
	envVar := "CLAUDE_CODE_OAUTH_TOKEN"
	uuidPrefix := "token-"
	if isAPIKey {
		kind, envVar, uuidPrefix = "api-key", "ANTHROPIC_API_KEY", "apikey-"
	}

	// Refuse before doing any work, and only for the kind that genuinely has
	// nowhere to go: a setup token is read from the environment ONLY, so
	// "activating" one would mean writing a record Claude Code never writes and
	// cannot use. An API key does have a home — ~/.claude.json's primaryApiKey —
	// and is activated further down, after it has been stored.
	if activate && !isAPIKey {
		return setupTokenRefusal("that token")
	}

	acct := store.Account{
		Email: email,
		Kind:  identity.Classify(nil, identity.UsageShape{}, isAPIKey),
		UUID:  uuidPrefix + fingerprint,
	}

	if !isAPIKey {
		// A setup token IS a bearer, so one profile call gives us both the real
		// email and the account classification. An API key is not: no endpoint
		// resolves one to an account, so that path makes no request at all.
		profile, err := newProfileClient().FetchProfile(cmd.Context(), token)
		switch {
		case err != nil && cmd.Context().Err() != nil:
			// An interrupted lookup is not a flaky network. Filing it as one
			// stores the account under a fabricated token-<hash> uuid and
			// reports success, so the next run stores the SAME account again
			// under its real uuid.
			return WithCode(err, ExitInterrupted)
		case errors.Is(err, identity.ErrUnauthorized):
			// Anthropic has already rejected this credential. Storing it would
			// create a managed account under a fabricated uuid that can never be
			// reconciled with a real one.
			return UsageError("that token was rejected by Anthropic; check it and try again")
		case errors.Is(err, identity.ErrForbidden):
			// The ORDINARY answer for a setup token rather than a fault, which
			// is why it is not a refusal. The lookup is scoped and
			// `claude setup-token` does not grant that scope, so this arm is the
			// one every setup token takes -- and while a 403 folded into
			// ErrUnauthorized, the arm above refused all of them, with a message
			// that blamed a token that works.
			//
			// What the refusal costs is the account's IDENTITY, not its
			// usability: the token still authenticates a session, which is what
			// `ccdad run` needs. So the account is stored under the synthetic
			// label below and the cost is named here rather than discovered
			// later from a store full of token-<hash> rows.
			fmt.Fprintf(stderr,
				"note: Anthropic refused the profile lookup for this token on scope; it asks for "+
					"user:profile or user:office.\nccdad cannot read the account's uuid, email or tier, "+
					"so it is stored under a synthetic label -- pass --email to give it one you will "+
					"recognise.\nThe token still works with 'ccdad run'; it can never be ranked or "+
					"switched to.\n")
		case err != nil:
			fmt.Fprintf(stderr, "warning: could not resolve the token to an account (%v); storing it under a synthetic label\n", err)
		default:
			acct.UUID = profile.AccountUUID
			if acct.Email == "" {
				acct.Email = profile.Email
			}
			acct.Tier = profile.OrganizationType
			acct.RateLimitTier = profile.RateLimitTier
			acct.OrganizationUUID = profile.OrganizationUUID
			acct.Kind = identity.Classify(profile, identity.UsageShape{}, false)
		}
	}

	encoded, err := json.Marshal(cclink.TokenRecord{Kind: kind, Token: token})
	if err != nil {
		return err
	}
	creds := cclink.Blob{cclink.TokenKey: encoded}

	// Open after the network call, matching runAdd: a typo'd token should not
	// leave a freshly created ~/.ccdad behind on a machine that has never used
	// ccdad successfully.
	s, err := store.Open()
	if err != nil {
		return err
	}

	// A setup token resolves to a real account uuid, and that account may
	// already be managed through a browser login. store.Add replaces the
	// credential file wholesale, so writing only the token record would destroy
	// that account's claudeAiOauth — refresh token included, which nothing else
	// has a copy of. The two are different credentials for the same account and
	// both are worth keeping.
	if existing, cerr := s.Credentials(acct.UUID); cerr == nil {
		for k, v := range existing {
			if _, fresh := creds[k]; !fresh {
				creds[k] = v
			}
		}
	}
	if acct.Email == "" {
		// A synthetic label must not churn on re-add, so an existing one wins.
		if existing, ok := s.Get(acct.UUID); ok && existing.Email != "" {
			acct.Email = existing.Email
		} else {
			acct.Email = syntheticEmail(isAPIKey, fingerprint)
		}
	}

	if err := s.Add(acct, creds); err != nil {
		return err
	}
	liftQuarantine(stderr, acct.UUID)
	if err := applyAlias(cmd, s, acct.UUID, alias); err != nil {
		return err
	}
	saved, ok := s.Get(acct.UUID)
	if !ok {
		return fmt.Errorf("the account was stored but cannot be read back; the store may be corrupt")
	}
	fmt.Fprintf(stderr, "Added %s (%s, index %d).\n", saved.Label(), saved.Kind, saved.Idx)

	if !activate {
		if isAPIKey {
			fmt.Fprintf(stderr, "Run 'ccdad switch %d' to make it the credential Claude Code uses.\n", saved.Idx)
		} else {
			fmt.Fprintf(stderr, "Claude Code reads a setup token from %s only; export it to use this account.\n", envVar)
		}
		return nil
	}

	// --activate IS a switch, so the drift probe and the cooldown stamp belong
	// here exactly as they do on `switch`. Reached only for an API key: the
	// setup-token case was refused before any work was done.
	live, liveErr := cclink.Load()
	if liveErr != nil {
		fmt.Fprintf(stderr, "note: could not read the current login (%v); activating anyway\n", liveErr)
	}
	if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
		fmt.Fprintf(stderr,
			"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
			strings.Join(unknown, ", "))
	}
	if err := activateAPIKeyAccount(token); err != nil {
		return err
	}
	if err := s.SetActive(saved.UUID); err != nil {
		return err
	}
	noteCooldown(cmd, switcher.RecordSwitch(saved.UUID))
	fmt.Fprintf(stderr, "Switched to %s.\n", saved.Label())
	noteDisplacingAuth(cmd, token)
	return nil
}

// liveOwnership is what a probe of the live login concluded.
type liveOwnership uint8

const (
	// liveOwnershipUnknown means the question could not be answered: no token
	// to ask with, a rejected token, an unreachable endpoint, a cancelled
	// command. It is the value every failure maps to, because none of them is
	// evidence either way.
	liveOwnershipUnknown liveOwnership = iota
	// liveOwnershipSame means the live login is the account just authenticated.
	liveOwnershipSame
	// liveOwnershipOther means it provably belongs to someone else.
	liveOwnershipOther
)

// liveAccessToken is the access token in the live credentials file, or "".
func liveAccessToken(live cclink.Blob) string {
	raw, ok := live["claudeAiOauth"]
	if !ok {
		return ""
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.AccessToken
}

// liveLoginOwner resolves the live login to an account and compares it to uuid.
//
// The token it sends is a credential that may belong to a DIFFERENT account
// than the one being added, and that is the whole point: the question is whose
// it is. It goes to the same endpoint, over the same TLS, in the same
// Authorization header that every other profile call in this package uses, so
// it is not a new class of exposure -- but it IS a request made with a token
// the user did not just hand us, which is why it happens only on the branch
// that has something to gain from it.
//
// Every failure is liveOwnershipUnknown rather than an error. The login has
// already succeeded by the time this runs; refusing to store an account because
// a best-effort probe timed out would turn an optimisation into an outage.
func liveLoginOwner(cmd *cobra.Command, uuid string, live cclink.Blob) liveOwnership {
	token := liveAccessToken(live)
	if token == "" || uuid == "" {
		return liveOwnershipUnknown
	}
	profile, err := newProfileClient().FetchProfile(cmd.Context(), token)
	if err != nil || profile == nil || profile.AccountUUID == "" {
		return liveOwnershipUnknown
	}
	if profile.AccountUUID == uuid {
		return liveOwnershipSame
	}
	return liveOwnershipOther
}
