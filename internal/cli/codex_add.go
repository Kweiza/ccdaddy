package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// defaultCodexHTTPClient builds the client codexHTTPClient hands out by
// default, and it is also what runCodexAdd falls back to if that seam ever
// returns nil.
//
// CheckRedirect refuses every redirect the device flow could be sent. The
// token exchange POSTs a body carrying the authorization code AND the
// code_verifier, and if the auth host ever answered with a 307 or 308, Go's
// default client would replay that body — both secrets included — to
// whatever target the redirect named. Returning http.ErrUseLastResponse stops
// the client from following it and hands the redirect response back
// unchanged, which the existing non-200 handling in codexauth already treats
// as a failed exchange.
func defaultCodexHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The login seams.
//
// Injected for the reason add.go's `login` is: a real device login needs a
// person to open a page and type a code, so without these every branch AFTER
// the approval — the workspace-member refusal, the two-workspace refusal, the
// store row — would be unreachable from a test, and each of those decides what
// happens to a live credential.
var (
	codexDeviceStart = codexauth.StartDeviceLogin
	codexDevicePoll  = codexauth.PollDeviceLogin
	// codexDeviceSleep is the wait between polls. It is the command's own and
	// not the poller's, so a test drives the loop without waiting.
	codexDeviceSleep = time.Sleep
	// codexHTTPClient is a fresh client per login. The stdlib transport is
	// deliberate: it honours the user's proxy environment, which is how this
	// works on a machine that reaches the internet through one.
	codexHTTPClient = defaultCodexHTTPClient
)

// codexLoginTimeout bounds the whole login. It is longer than the device code's
// own fifteen-minute life so the code expires first and the user is told which
// of the two happened.
const codexLoginTimeout = 20 * time.Minute

func newAddCodexCmd() *cobra.Command {
	var allowWorkspaceMember bool

	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Log in to a Codex account and manage it",
		Long: "Prints a code and a page to enter it on, then waits for you to approve it.\n" +
			"There is no browser to open and nothing to paste back.\n\n" +
			"Adding an account does not switch to it. It is stored but does not yet serve\n" +
			"codex.\n\n" +
			"This never touches codex's own home and never runs 'codex login' or\n" +
			"'codex logout'.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCodexAdd(cmd, allowWorkspaceMember)
		},
	}
	cmd.Flags().BoolVar(&allowWorkspaceMember, "allow-workspace-member", false,
		"add a seat inside somebody else's workspace without being asked")
	return cmd
}

func runCodexAdd(cmd *cobra.Command, allowWorkspaceMember bool) error {
	// context.WithTimeout(cmd.Context(), ...) is this package's own shape --
	// update.go does the same for every network step -- so a client that goes
	// away cancels the login it started.
	ctx, cancel := context.WithTimeout(cmd.Context(), codexLoginTimeout)
	defer cancel()

	stderr := cmd.ErrOrStderr()
	client := codexHTTPClient()
	if client == nil {
		// codexHTTPClient is a seam a test can reassign, and
		// StartDeviceLogin/PollDeviceLogin dereference the client with no nil
		// check of their own -- a nil client here panics deep inside the
		// device flow instead of failing this command cleanly.
		client = defaultCodexHTTPClient()
	}

	start, err := codexDeviceStart(ctx, client)
	if err != nil {
		return err
	}
	// stderr, not stdout: this is prose for a person, and stdout is where a
	// machine-readable answer would go if this command ever grew one.
	fmt.Fprintf(stderr, "Open %s and enter this code:\n\n    %s\n\nWaiting for you to approve it.\n",
		codexauth.DeviceVerifyURL, start.UserCode)

	cred, err := codexDevicePoll(ctx, client, start, codexDeviceSleep)
	if err != nil {
		return err
	}

	// The identity comes out of the id_token and out of nothing else. There is
	// no second lookup to make: the claim set already carries the user, the
	// workspace and the tier.
	claims, err := codexauth.DecodeClaims(cred.IDToken)
	if err != nil {
		return fmt.Errorf("the login succeeded and ccdad could not read its identity: %w", err)
	}
	if claims.UserID == "" {
		return fmt.Errorf("the login succeeded and carried no user id, so there is nothing to file it under")
	}
	if claims.AccountID == "" {
		return fmt.Errorf("the login succeeded and carried no workspace id, so ccdad cannot name the quota it shares")
	}

	label := claims.Email
	if label == "" {
		label = claims.UserID
	}

	if title, member := codexWorkspaceSeat(claims); member && !allowWorkspaceMember {
		// One person holding several subscriptions is a different thing from
		// several people sharing one, and only the first is what this feature
		// is for. A member seat is therefore a deliberate act rather than a
		// default.
		if !stdinIsTTY() {
			return UsageError("%s is a member seat in the workspace %q rather than an owner one.\n"+
				"Rotating between seats in one shared workspace is a different thing from one person holding several subscriptions.\n"+
				"Pass --allow-workspace-member to add it anyway.", label, title)
		}
		fmt.Fprintf(stderr, "%s is a member seat in the workspace %q rather than an owner one.\n"+
			"Rotating between seats in one shared workspace is a different thing from one person\n"+
			"holding several subscriptions.\nAdd it anyway? [y/N] ", label, title)
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintln(stderr, "Left alone.")
			return WithCode(errSilent, ExitNothingToDo)
		}
	}

	// The credential's identity fields are rebuilt from the CLAIMS rather than
	// trusted from the device exchange, so one decode decides both what is
	// stored and what the row says about it.
	cred.UserID, cred.AccountID = claims.UserID, claims.AccountID

	acct := store.Account{
		UUID:     claims.UserID,
		Email:    claims.Email,
		Provider: provider.Codex,
		// Always. A Codex account is metered by its quota windows and there is
		// no credit axis to classify it onto.
		Kind: identity.KindSubscription,
		// The raw plan type, verbatim. An unrecognized tier is one this build
		// has not seen rather than an error.
		Tier: claims.PlanType,
		// The WORKSPACE. It is the quota and sharing-group key, and it is not
		// the account: two seats in one team workspace share it.
		OrganizationUUID: claims.AccountID,
	}

	if err := store.WithStore(func(s *store.Store) error {
		// One user in two workspaces is out of scope for this release, and it
		// is refused rather than merged: store.Add updates an existing uuid IN
		// PLACE, so accepting it would replace the first workspace's seat with
		// the second's and lose the first — including its credential.
		if existing, ok := s.Get(acct.UUID); ok && existing.OrganizationUUID != "" && existing.OrganizationUUID != acct.OrganizationUUID {
			return UsageError("%s is already stored in the workspace %q and this login is in %q.\n"+
				"ccdad manages one workspace per Codex user; remove the stored one first if you meant to move it.",
				label, existing.OrganizationUUID, acct.OrganizationUUID)
		}
		return s.Add(acct, cred.ToBlob())
	}); err != nil {
		return err
	}

	fmt.Fprintf(stderr, "Added %s (codex", label)
	if acct.Tier != "" {
		fmt.Fprintf(stderr, ", %s", acct.Tier)
	}
	// Only what this build can back up. Switching shipped with the lane that
	// serves these accounts, so a command IS namable now -- and it is worth
	// naming, because a first Codex account is stored without becoming the one
	// ccdad serves codex from, and nothing else on this path says so.
	//
	// The account is named OUTRIGHT, and not left to `ccdad switch --provider
	// codex`, even though that is the form which ranks. Ranking needs usage
	// readings and there are none at this instant: nothing has polled an
	// account this old, and no daemon was started to poll it either, because
	// autoStartCommands has no entry for this command. The ranking form
	// therefore answers ExitBlocked when it is run here -- the exit
	// TestATargetlessCodexSwitchWithNoReadingsIsBlocked pins -- and advice
	// that refuses the moment it is followed is worse than no advice.
	fmt.Fprintf(stderr, ").\nIt is logged in and stored.\n"+
		"`ccdad switch %s` makes it the account ccdad serves codex from.\n", label)
	return nil
}

// codexWorkspaceSeat names the workspace this login is in and reports whether
// the seat is somebody else's rather than the user's own.
//
// A seat is a MEMBER seat when the workspace this login is scoped to is not the
// identity's default one, or the role in it is not owner. Both halves matter: a
// non-default workspace is one the user was invited into, and a non-owner role
// in the default one is the same fact seen from the other side.
//
// A claim set that names no matching organization reports NOT a member, and
// that is deliberate. Nothing in it says the seat belongs to somebody else, and
// refusing on absence would refuse every login whose issuer stopped sending the
// organizations claim — which is a field ccdad does not control and cannot
// require.
func codexWorkspaceSeat(claims codexauth.Claims) (string, bool) {
	for _, o := range claims.Organizations {
		if o.ID != claims.AccountID {
			continue
		}
		title := o.Title
		if title == "" {
			title = o.ID
		}
		if o.IsDefault && strings.EqualFold(o.Role, "owner") {
			return title, false
		}
		return title, true
	}
	return "", false
}
