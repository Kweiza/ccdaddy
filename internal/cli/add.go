package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Kweiza/ccdaddy/internal/browser"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/store"
)

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

// tokenCredentialKey is ccdad's own record for a credential Claude Code does
// NOT read out of the credentials file.
//
// The 2.1.238 bundle is explicit about both kinds. An API key is persisted to
// ~/.claude.json as the top-level string primaryApiKey (function Saa), and the
// claudeAiOauth object has exactly eight keys — accessToken, refreshToken,
// expiresAt, refreshTokenExpiresAt, scopes, subscriptionType, rateLimitTier,
// clientId (function hjd) — none of which is an API key. A setup token is not
// persisted at all: `claude setup-token` prints it and tells the user to export
// CLAUDE_CODE_OAUTH_TOKEN, and the OAuth flow's setup-token branch deliberately
// skips the credential save.
//
// So neither belongs under claudeAiOauth. Putting one there would write a
// record Claude Code never writes and cannot read as a login. This key is
// ccdad's own file, plainly named as such; cclink.Activate refuses a snapshot
// with no claudeAiOauth, which makes the separation fail-closed.
const tokenCredentialKey = "ccdadToken"

// tokenRecord is what tokenCredentialKey holds.
type tokenRecord struct {
	// Kind is "api-key" or "setup-token".
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

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
			}

			surface := oauth.SurfaceClaudeAI
			if useConsole {
				surface = oauth.SurfaceConsole
			}
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
				fmt.Fprint(stderr, "Paste code here: ")
			} else {
				fmt.Fprintln(stderr, "(stdin is not a terminal, so ccdad is waiting on the browser callback only.)")
			}
		},
	})
	if err != nil {
		return loginError(err)
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
		// switch on a subscription org is not evidence (spec §5).
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

	// A first-time account has no prior record; the error is the absence, not a
	// failure, so it is deliberately ignored.
	prior, _ := s.Credentials(acct.UUID)
	creds, err := credentialBlob(result.Token, profile, prior)
	if err != nil {
		return err
	}

	// add doubles as re-authentication (spec §6.5). When the account being
	// re-authenticated is the one currently live, its other account-scoped keys
	// — trustedDeviceToken, enterpriseGateway, designOauth — are still in the
	// live file and are cheap to keep. Losing them costs a device-cap slot and a
	// gateway re-trust (spec §4.1). Guarded on identity: capturing
	// unconditionally would file the PREVIOUS account's device token under this
	// one, which is the same leak in the other direction.
	if existed && s.ActiveUUID() == acct.UUID {
		if snapshot, cerr := cclink.Capture(); cerr == nil {
			for k, v := range snapshot {
				if _, fresh := creds[k]; !fresh {
					creds[k] = v
				}
			}
		}
	}

	if err := s.Add(acct, creds); err != nil {
		return err
	}
	if err := applyAlias(s, acct.UUID, opts.alias); err != nil {
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
		if err := cclink.Activate(creds); err != nil {
			return err
		}
		if err := s.SetActive(saved.UUID); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Switched to %s.\n", saved.Label())
	} else {
		fmt.Fprintf(stderr, "Run 'ccdad switch %d' to use it.\n", saved.Idx)
	}
	return nil
}

// loginError maps the login sentinels onto the exit contract. An interrupted
// login is SIGINT's code, not a generic failure, because a supervisor keys on
// 130 to tell "the operator stopped it" from "it broke".
func loginError(err error) error {
	switch {
	case errors.Is(err, oauth.ErrLoginInterrupted):
		return WithCode(err, ExitInterrupted)
	case errors.Is(err, oauth.ErrLoginTimeout):
		return WithCode(err, ExitBlocked)
	}
	return err
}

// applyAlias routes an alias through SetAlias rather than through Account.Alias.
//
// SetAlias is the only path that enforces uniqueness, and it is also what lets
// an explicit --alias re-label an account being re-authenticated: store.Add
// deliberately preserves the stored alias over the incoming one, so assigning
// Account.Alias would silently discard --alias on every re-auth.
func applyAlias(s *store.Store, uuid, alias string) error {
	normalized := store.NormalizeAlias(alias)
	if normalized == "" {
		return nil
	}
	if err := s.SetAlias(uuid, normalized); err != nil {
		return UsageError("%s", err.Error())
	}
	return nil
}

// credentialBlob renders a token response into the account-scoped shape Claude
// Code expects under claudeAiOauth.
//
// prior is the account's previously stored claudeAiOauth, or nil. Fields it
// carries that this exchange did not return are preserved rather than dropped:
// spec §4.2 rule 3 names refreshTokenExpiresAt, rateLimitTier and clientId as
// the three clauth destroyed, and clientId is what a revocation request needs.
// Replacing the record wholesale on every `ccdad add` would reintroduce that
// exact failure one layer above cclink.
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
	payload["clientId"] = oauth.ClientID
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
		payload["subscriptionType"] = profile.OrganizationType
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
			"Claude Code reads these from the environment, not from its credentials file:\n" +
			"CLAUDE_CODE_OAUTH_TOKEN for a setup token, ANTHROPIC_API_KEY for an API key.\n" +
			"ccdad records the account but cannot install one as the live login.",
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
	cmd.Flags().BoolVar(&activate, "activate", false, "not supported for tokens; Claude Code reads them from the environment")
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

	// Refuse before doing any work. Claude Code reads neither kind of token out
	// of the credentials file, so "activating" one would mean writing a record
	// it never writes and cannot use as a login. Name the mechanism that does
	// work instead of producing a broken live file.
	if activate {
		return UsageError("an %s cannot be activated as a Claude Code login; Claude Code reads it from %s, not from the credentials file",
			kind, envVar)
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
		case errors.Is(err, identity.ErrUnauthorized):
			// Anthropic has already rejected this credential. Storing it would
			// create a managed account under a fabricated uuid that can never be
			// reconciled with a real one.
			return UsageError("that token was rejected by Anthropic; check it and try again")
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

	encoded, err := json.Marshal(tokenRecord{Kind: kind, Token: token})
	if err != nil {
		return err
	}
	creds := cclink.Blob{tokenCredentialKey: encoded}

	// Open after the network call, matching runAdd: a typo'd token should not
	// leave a freshly created ~/.ccdad behind on a machine that has never used
	// ccdad successfully.
	s, err := store.Open()
	if err != nil {
		return err
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
	if err := applyAlias(s, acct.UUID, alias); err != nil {
		return err
	}
	saved, ok := s.Get(acct.UUID)
	if !ok {
		return fmt.Errorf("the account was stored but cannot be read back; the store may be corrupt")
	}
	fmt.Fprintf(stderr, "Added %s (%s, index %d).\n", saved.Label(), saved.Kind, saved.Idx)
	fmt.Fprintf(stderr, "Claude Code reads this credential from %s; ccdad stores it but does not install it as the live login.\n", envVar)
	return nil
}
