package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// codexJWT builds an id_token the way the issuer writes one. Nothing checks the
// signature, here or in production, so the third segment is a constant.
func codexJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc([]byte(payload)) + ".c2ln"
}

// ownerPayload is a seat that owns its own default workspace: the ordinary
// case, added with no confirmation.
const ownerPayload = `{"email":"person@example.com","exp":1790688319,
 "https://api.openai.com/auth":{"chatgpt_user_id":"user-abc","chatgpt_account_id":"ws-1",
   "chatgpt_plan_type":"pro",
   "organizations":[{"id":"ws-1","title":"Personal","role":"owner","is_default":true}]}}`

// memberPayload is a seat inside somebody else's workspace.
const memberPayload = `{"email":"person@example.com","exp":1790688319,
 "https://api.openai.com/auth":{"chatgpt_user_id":"user-abc","chatgpt_account_id":"ws-2",
   "chatgpt_plan_type":"team",
   "organizations":[{"id":"ws-2","title":"Acme Corp","role":"member","is_default":false}]}}`

// stubCodexDevice stands in for the device-code endpoints. It is the fake
// device server: a real one needs a person to approve a code, so the two seams
// are what make every branch after the approval reachable at all.
func stubCodexDevice(t *testing.T, payload string, pollErr error) *int {
	t.Helper()
	savedStart, savedPoll, savedSleep := codexDeviceStart, codexDevicePoll, codexDeviceSleep
	t.Cleanup(func() {
		codexDeviceStart, codexDevicePoll, codexDeviceSleep = savedStart, savedPoll, savedSleep
	})
	var slept int
	codexDeviceStart = func(context.Context, *http.Client) (codexauth.DeviceStart, error) {
		return codexauth.DeviceStart{
			DeviceAuthID: "dev-1",
			UserCode:     "ABCD-EFGH",
			Interval:     5 * time.Second,
			ExpiresAt:    time.Now().Add(15 * time.Minute),
		}, nil
	}
	codexDevicePoll = func(_ context.Context, _ *http.Client, start codexauth.DeviceStart, sleep func(time.Duration)) (codexauth.Credential, error) {
		if pollErr != nil {
			return codexauth.Credential{}, pollErr
		}
		sleep(start.Interval)
		return codexauth.Credential{
			IDToken:      codexJWT(payload),
			AccessToken:  "AT",
			RefreshToken: "RT",
			AccountID:    "ignored-by-the-command",
			UserID:       "ignored-by-the-command",
			LastRefresh:  time.Now().UTC(),
		}, nil
	}
	codexDeviceSleep = func(time.Duration) { slept++ }
	return &slept
}

func TestCodexAddStoresTheAccountFromTheIDTokenClaims(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s\ntop: %s", code, stderr, top)
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// The uuid is the chatgpt_user_id and NOT the account id. Two seats in one
	// team workspace share the account id, so keying on it would let a
	// colleague's login overwrite the first seat in place.
	acct, ok := s.Get("user-abc")
	if !ok {
		t.Fatalf("the account was not stored under its chatgpt_user_id: %+v", s.Accounts())
	}
	if acct.Provider != provider.Codex {
		t.Errorf("Provider = %q, want codex", acct.Provider)
	}
	if acct.Kind != identity.KindSubscription {
		t.Errorf("Kind = %v, want KindSubscription — a Codex account is always a subscription", acct.Kind)
	}
	if acct.Tier != "pro" {
		t.Errorf("Tier = %q, want the raw chatgpt_plan_type", acct.Tier)
	}
	if acct.OrganizationUUID != "ws-1" {
		t.Errorf("OrganizationUUID = %q, want the chatgpt_account_id — the workspace", acct.OrganizationUUID)
	}
	if acct.Email != "person@example.com" {
		t.Errorf("Email = %q", acct.Email)
	}

	blob, err := s.Credentials("user-abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := blob[codexauth.Key]; !ok {
		t.Fatalf("the stored blob has no %s key: %v", codexauth.Key, blob)
	}
	cred, present, err := codexauth.FromBlob(blob)
	if err != nil || !present {
		t.Fatalf("FromBlob() = %v, %v, %v", cred, present, err)
	}
	if cred.AccessToken != "AT" || cred.RefreshToken != "RT" {
		t.Errorf("stored credential = %+v", cred)
	}
	if cred.AccountID != "ws-1" || cred.UserID != "user-abc" {
		t.Errorf("stored credential identity = %q/%q, want the claims' own", cred.UserID, cred.AccountID)
	}
	// The login is a ccdad account, not a Claude Code one.
	assertNoLiveCredentials(t)
}

func TestCodexAddPrintsTheCodeAndTheVerificationPage(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, nil)

	_, _, stderr, _ := runRoot(t, "add", "codex")
	for _, want := range []string{"ABCD-EFGH", codexauth.DeviceVerifyURL} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
	}
}

// Rotating between seats in one shared workspace is a different thing from one
// person holding several subscriptions, so a member seat is a deliberate act.
// With no terminal to ask at, it has to be typed.
func TestCodexAddRefusesAWorkspaceMemberSeatWithNoTerminal(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	stubCodexDevice(t, memberPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitUsage, stderr, top)
	}
	if !strings.Contains(top, "Acme Corp") {
		t.Errorf("the refusal does not name the workspace:\n%s", top)
	}
	if !strings.Contains(top, "--allow-workspace-member") {
		t.Errorf("the refusal does not name the flag that answers it:\n%s", top)
	}
	s, _ := store.Open()
	if len(s.Accounts()) != 0 {
		t.Errorf("the refused login was stored anyway: %+v", s.Accounts())
	}
}

func TestCodexAddAcceptsAWorkspaceMemberSeatWithTheFlag(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	stubCodexDevice(t, memberPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex", "--allow-workspace-member")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s\ntop: %s", code, stderr, top)
	}
	s, _ := store.Open()
	if _, ok := s.Get("user-abc"); !ok {
		t.Fatal("the flagged login was not stored")
	}
}

func TestCodexAddAsksBeforeAddingAWorkspaceMemberSeatOnATerminal(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubCodexDevice(t, memberPayload, nil)

	cmd := newAddCodexCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	err, _, errOut := runCmd(t, cmd)
	if CodeFor(err) != ExitNothingToDo {
		t.Fatalf("CodeFor = %d, want %d (%v)", CodeFor(err), ExitNothingToDo, err)
	}
	if !strings.Contains(errOut, "Acme Corp") {
		t.Errorf("the prompt does not name the workspace:\n%s", errOut)
	}
	s, _ := store.Open()
	if len(s.Accounts()) != 0 {
		t.Errorf("answering 'n' still stored the account: %+v", s.Accounts())
	}
}

// One user in two workspaces is out of scope for this release, and it is a
// refusal rather than a silent overwrite: store.Add updates an existing uuid in
// place, so accepting it would replace the first workspace's seat with the
// second's and lose the first.
func TestCodexAddRefusesTheSameUserInADifferentWorkspace(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, nil)
	if code, _, _, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("the first add failed: %s", top)
	}

	stubCodexDevice(t, memberPayload, nil)
	code, _, _, top := runRoot(t, "add", "codex", "--allow-workspace-member")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\ntop: %s", code, ExitUsage, top)
	}
	if !strings.Contains(top, "ws-1") || !strings.Contains(top, "ws-2") {
		t.Errorf("the refusal does not name both workspaces:\n%s", top)
	}
	s, _ := store.Open()
	acct, _ := s.Get("user-abc")
	if acct.OrganizationUUID != "ws-1" {
		t.Errorf("OrganizationUUID = %q; the refused second login overwrote the first", acct.OrganizationUUID)
	}
}

// ccdad owns login, refresh and quota for a Codex account and codex holds no
// token at all. Writing into codex's own home would be the file-swap design
// this one exists instead of.
func TestCodexAddNeverTouchesTheCodexHome(t *testing.T) {
	isolate(t)
	codexHome := filepath.Join(t.TempDir(), "dot-codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	stubCodexDevice(t, ownerPayload, nil)

	if code, _, _, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, top)
	}
	entries, err := os.ReadDir(codexHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("CODEX_HOME gained %d entries; ccdad never writes codex's home", len(entries))
	}
}

func TestCodexAddReportsAFailedLoginAndStoresNothing(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, errors.New("the device code expired before it was approved"))

	code, _, _, top := runRoot(t, "add", "codex")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d\ntop: %s", code, ExitFailure, top)
	}
	s, _ := store.Open()
	if len(s.Accounts()) != 0 {
		t.Errorf("a failed login stored an account: %+v", s.Accounts())
	}
}

// The bare parent is a usage error, as every other command group in this tree
// is: cobra's own answer would be help text on stdout at exit 0.
func TestCodexWithNoSubcommandIsAUsageError(t *testing.T) {
	isolate(t)
	code, _, _, top := runRoot(t, "codex")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\ntop: %s", code, ExitUsage, top)
	}
	if !strings.Contains(top, "exec") || !strings.Contains(top, "shim") {
		t.Errorf("the refusal does not name the subcommands this group still has:\n%s", top)
	}
}

// `ccdad codex add` is one release old and is REMOVED rather than kept as a
// hidden alias. An alias exits 0, so a script still spelling it that way would
// go on working and its author would never learn the name had moved; a usage
// error naming the spelling that works is the only shape they read.
//
// This fires from the parent's own Args, which cobra reaches before any
// persistent pre-run hook, so the tombstone is what a user sees from inside a
// `ccdad run` session too.
//
// The flagged row is the shape a real caller has: the old login took flags, and
// --allow-workspace-member now lives on a leaf this group does not define. Args
// runs after flag parsing, so that row answered `unknown flag` and the tombstone
// never ran — the one run it was supposed to cost taught its reader a flag name
// instead of the rename. It reaches the tombstone only because the group
// whitelists unknown flags.
func TestTheOldCodexAddSpellingIsATombstoneAndNotAnAlias(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare", []string{"codex", "add"}},
		{"with a flag the old login took", []string{"codex", "add", "--allow-workspace-member"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)

			code, _, _, top := runRoot(t, tc.args...)

			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d\ntop: %s", code, ExitUsage, top)
			}
			if !strings.Contains(top, "ccdad add codex") {
				t.Errorf("the tombstone does not name the spelling that works:\n%s", top)
			}
			if strings.Contains(top, "unknown flag") {
				t.Errorf("pflag answered ahead of Args, so the run cost the caller a flag "+
					"name rather than the rename:\n%s", top)
			}
		})
	}
}

// Both new paths are ALLOWED in a scoped session. A session scopes Claude
// Code's credential and config homes and nothing else, and this command writes
// only ccdad's own store — which is also why `ccdad add claude` stays refused
// and this one does not: that one activates a Claude login.
func TestTheCodexPathsAreAllowedInsideARunSession(t *testing.T) {
	for _, path := range []string{"ccdad codex", "ccdad add codex"} {
		if !scopedSessionAllowed[path] {
			t.Errorf("%q has no allowed verdict", path)
		}
		if _, refused := scopedSessionRefusals[path]; refused {
			t.Errorf("%q is classified twice", path)
		}
	}
}

func TestCodexAddHonoursThePollingIntervalItWasGiven(t *testing.T) {
	isolate(t)
	slept := stubCodexDevice(t, ownerPayload, nil)
	if code, _, _, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, top)
	}
	if *slept != 1 {
		t.Errorf("the sleep seam was called %d times, want 1 — the command must hand its own sleep to the poller", *slept)
	}
}

// The device-code client is the caller's own construction, and the token
// exchange it drives POSTs a body carrying the authorization code and the
// code_verifier. If the auth host ever answered with a redirect, a client that
// followed it would replay that body -- secrets included -- at whatever target
// the redirect named. This proves the client codexHTTPClient hands out refuses
// to follow one: the redirect target below must never see a request land.
func TestCodexHTTPClientNeverFollowsARedirect(t *testing.T) {
	var redirectTargetHit bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	client := codexHTTPClient()
	if client == nil {
		t.Fatal("codexHTTPClient() = nil")
	}
	req, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader("code=auth-code&code_verifier=pkce-secret"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() = %v, want the unfollowed redirect response and a nil error", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want %d (the redirect itself, not followed)", res.StatusCode, http.StatusTemporaryRedirect)
	}
	if redirectTargetHit {
		t.Error("the redirect target received a request; the exchange body would have been replayed to it")
	}
}

// codexAdviceCommandRE finds every quoted `ccdad WORD` or 'ccdad WORD' phrase
// -- the shape this file's own advice sentences use -- without also matching
// plain prose that happens to say "ccdad" (e.g. "the account ccdad serves
// codex from"), which carries no quote before it.
var codexAdviceCommandRE = regexp.MustCompile("[`']ccdad ([a-z][a-z0-9-]*)")

// TestCodexAddAdviceNamesOnlyCommandsThatWorkForACodexAccount guards the
// success message and the Long help text against telling a Codex user to run
// a ccdad command that either does not exist or refuses a Codex account
// outright -- the mistake this command's advice actually shipped with: it told
// every first-time user to run `ccdad switch <email>`, and switch refused a
// Codex account by name. `switch` itself has since stopped refusing -- it
// repoints the codex proxy -- but run.go and probe.go still refuse the same
// way, with the same "is a Codex account" phrase, so the guard stays useful
// for those. This is this command's own version of
// TestTheAdviceToRunStatusRefreshNamesAFlagThatExists in list_test.go.
func TestCodexAddAdviceNamesOnlyCommandsThatWorkForACodexAccount(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("the add itself failed: %s%s", stderr, top)
	}

	texts := map[string]string{
		"the success message": stderr,
		"the Long help text":  newAddCodexCmd().Long,
	}

	for label, text := range texts {
		for _, m := range codexAdviceCommandRE.FindAllStringSubmatch(text, -1) {
			name := m[1]
			root := NewRootCmd()
			sub, _, err := root.Find([]string{name})
			if err != nil || sub == root {
				t.Errorf("%s tells the user to run `ccdad %s`, which is not a real command", label, name)
				continue
			}
			// "1" is the one account this test stored, addressed by its
			// display index -- the same shape TestSwitchToACodexAccountByIndexRepoints
			// in switch_test.go names the same account with.
			_, _, rerr, rtop := runRoot(t, name, "1")
			if strings.Contains(rerr, "is a Codex account") || strings.Contains(rtop, "is a Codex account") {
				t.Errorf("%s tells the user to run `ccdad %s`, which refuses a Codex account:\n%s%s",
					label, name, rerr, rtop)
			}
		}
	}
}

// codexAdviceWholeLineRE takes a backticked command line WHOLE -- flags,
// arguments and all -- where codexAdviceCommandRE above takes only the verb.
// The difference is the whole point of the test below: a prefix of the advised
// line can run when the line itself refuses.
var codexAdviceWholeLineRE = regexp.MustCompile("`ccdad ([^`]+)`")

// TestTheCodexAddAdviceRunsAtTheMomentItIsPrinted runs what the success
// message just told the user to run, verbatim, on the store the add left
// behind.
//
// TestCodexAddAdviceNamesOnlyCommandsThatWorkForACodexAccount asks a narrower
// question and cannot see this one: it re-spells each verb with an account
// reference of its own, so `ccdad switch --provider codex` is checked as
// `ccdad switch 1`. Those two exit differently at this instant. A first Codex
// account has no usage reading yet and `ccdad add codex` starts no daemon to
// take one, so the ranking form answers ExitBlocked while naming the account
// answers ExitOK -- which is how a line advising the ranking form shipped
// green in the commit whose whole subject was pointing advice at spellings
// that run.
//
// Exit 0 and nothing weaker: this line is read by somebody who has just
// finished a login and is being told what to do next, and a usage error or a
// refusal is not a next step. The serving pointer is then checked as well,
// because exit 0 alone is a hole -- `ccdad status` clears it too, and would
// leave a message that runs and does nothing green.
func TestTheCodexAddAdviceRunsAtTheMomentItIsPrinted(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("the add itself failed: %s%s", stderr, top)
	}

	advised := codexAdviceWholeLineRE.FindAllStringSubmatch(stderr, -1)
	if len(advised) == 0 {
		t.Fatalf("the success message hands the user no command line at all, so this test "+
			"is asserting nothing:\n%s", stderr)
	}
	for _, m := range advised {
		argv := strings.Fields(m[1])
		code, _, rerr, rtop := runRoot(t, argv...)
		if code != ExitOK {
			t.Errorf("the add printed `ccdad %s` and running exactly that exits %d:\n%s%s",
				m[1], code, rerr, rtop)
		}
	}

	// "user-abc" is ownerPayload's chatgpt_user_id, which is the uuid the add
	// above filed the account under.
	if got := servingNow(t); got != "user-abc" {
		t.Errorf("serving = %q, want user-abc -- following the advice left ccdad serving "+
			"codex from somewhere else, so the sentence describes a command it does not name",
			got)
	}
}

// codexWorkspaceSeat has three branches a mutation test found unpinned: the
// no-organizations fallthrough, the case-sensitive role comparison, and the
// workspace-id guard. Each case below exists to catch exactly one of them.
func TestCodexWorkspaceSeat(t *testing.T) {
	tests := []struct {
		name       string
		claims     codexauth.Claims
		wantTitle  string
		wantMember bool
	}{
		{
			// Catches a mutated fallthrough that returns member=true when the
			// issuer sends no organizations claim at all: the docstring calls
			// this branch deliberate, and this pins it.
			name: "no organizations claim at all is not a member",
			claims: codexauth.Claims{
				AccountID: "ws-1",
			},
			wantTitle:  "",
			wantMember: false,
		},
		{
			// Catches a role comparison that stops being case-insensitive: an
			// issuer sending "Owner" must still read as an owner.
			name: "a capitalized Owner role is still an owner",
			claims: codexauth.Claims{
				AccountID: "ws-1",
				Organizations: []codexauth.Organization{
					{ID: "ws-1", Title: "Personal", Role: "Owner", IsDefault: true},
				},
			},
			wantTitle:  "Personal",
			wantMember: false,
		},
		{
			// Catches a deleted `o.ID != claims.AccountID` guard: with it gone,
			// the loop would judge the seat from the FIRST organization listed
			// rather than the one the login is actually scoped to.
			name: "a non-matching organization listed first is skipped",
			claims: codexauth.Claims{
				AccountID: "ws-2",
				Organizations: []codexauth.Organization{
					{ID: "ws-1", Title: "Other Workspace", Role: "member", IsDefault: true},
					{ID: "ws-2", Title: "Personal", Role: "owner", IsDefault: true},
				},
			},
			wantTitle:  "Personal",
			wantMember: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, member := codexWorkspaceSeat(tc.claims)
			if title != tc.wantTitle || member != tc.wantMember {
				t.Errorf("codexWorkspaceSeat() = (%q, %v), want (%q, %v)", title, member, tc.wantTitle, tc.wantMember)
			}
		})
	}
}

// A second `codex add` for the same user in the same workspace is
// re-authentication rather than a fresh account, and store.Add's own contract
// (store.go's Add doc comment) is that the alias, the display index and the
// disabled/primary flags survive it because they belong to the user rather
// than to the login. This is the codex path's own proof of that promise: the
// different-workspace refusal was covered already, but re-adding the SAME
// workspace was not, so a regression that lost these fields here would have
// been silent.
func TestCodexAddReauthenticatesTheSameAccountInPlace(t *testing.T) {
	isolate(t)
	stubCodexDevice(t, ownerPayload, nil)
	if code, _, _, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("the first add failed: %s", top)
	}
	if code, _, _, top := runRoot(t, "alias", "1", "codex-main"); code != ExitOK {
		t.Fatalf("setting the alias failed: %s", top)
	}
	if code, _, _, top := runRoot(t, "disable", "1"); code != ExitOK {
		t.Fatalf("disabling failed: %s", top)
	}

	stubCodexDevice(t, ownerPayload, nil)
	if code, _, _, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("the re-add failed: %s", top)
	}

	s, _ := store.Open()
	acct, ok := s.Get("user-abc")
	if !ok {
		t.Fatal("the account disappeared across the re-add")
	}
	if acct.Alias != "codex-main" {
		t.Errorf("Alias = %q, want it to survive the re-add", acct.Alias)
	}
	if !acct.Disabled {
		t.Error("Disabled = false, want it to survive the re-add")
	}
}

// The nil-client guard in runCodexAdd exists so a codexHTTPClient seam that
// ever returns nil cannot panic deep inside StartDeviceLogin/PollDeviceLogin.
// Nothing in production reassigns that seam, so without a test driving it
// through nil the guard is dead code no run ever measures. This makes it live:
// it points the seam at a function that returns nil and confirms the command
// still completes, having handed the two device-flow seams a real, non-nil
// client instead.
func TestCodexAddFallsBackToARealClientWhenTheSeamReturnsNil(t *testing.T) {
	isolate(t)
	savedClient := codexHTTPClient
	t.Cleanup(func() { codexHTTPClient = savedClient })
	codexHTTPClient = func() *http.Client { return nil }

	savedStart, savedPoll, savedSleep := codexDeviceStart, codexDevicePoll, codexDeviceSleep
	t.Cleanup(func() { codexDeviceStart, codexDevicePoll, codexDeviceSleep = savedStart, savedPoll, savedSleep })

	var sawStartClient, sawPollClient *http.Client
	codexDeviceStart = func(_ context.Context, client *http.Client) (codexauth.DeviceStart, error) {
		sawStartClient = client
		return codexauth.DeviceStart{
			DeviceAuthID: "dev-1",
			UserCode:     "ABCD-EFGH",
			Interval:     5 * time.Second,
			ExpiresAt:    time.Now().Add(15 * time.Minute),
		}, nil
	}
	codexDevicePoll = func(_ context.Context, client *http.Client, start codexauth.DeviceStart, sleep func(time.Duration)) (codexauth.Credential, error) {
		sawPollClient = client
		sleep(start.Interval)
		return codexauth.Credential{
			IDToken:      codexJWT(ownerPayload),
			AccessToken:  "AT",
			RefreshToken: "RT",
			LastRefresh:  time.Now().UTC(),
		}, nil
	}
	codexDeviceSleep = func(time.Duration) {}

	code, _, _, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: %s", code, top)
	}
	if sawStartClient == nil || sawPollClient == nil {
		t.Fatal("the device-flow seams received a nil client; the fallback in runCodexAdd did not fire")
	}
	// Dereferencing a field is exactly what a real codexauth call does first.
	// If the fallback had not fired, this client would be nil and the next two
	// lines would already have panicked.
	_ = sawStartClient.Timeout
	_ = sawPollClient.Timeout
}

// codexShimEnvironment describes a machine the shim installer can actually
// succeed on. Every line of it is an input the installer reads from outside the
// process — the login shell, the live PATH, the two package-manager variables,
// and where this binary sits — so a test that left any of them alone would pass
// or fail depending on whose shell ran it.
func codexShimEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "ccdad"))
}

// The shim is what makes typing `codex` reach ccdad at all, and until this the
// add said nothing about it — at exactly the moment `ccdad doctor` starts
// warning that codex is unrouted.
func TestAddingACodexAccountInstallsTheShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}

	body, err := os.ReadFile(shimPath())
	if err != nil {
		t.Fatalf("the add stored the account and installed no shim: %v", err)
	}
	if string(body) != codexShimBody {
		t.Errorf("the shim is %q, want %q", body, codexShimBody)
	}
	rc := filepath.Join(os.Getenv("HOME"), ".bashrc")
	block, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("no startup file registers the shim directory: %v", err)
	}
	if !strings.Contains(string(block), shimDir()) {
		t.Errorf("%s does not register %s, so a new terminal still finds the real codex:\n%s", rc, shimDir(), block)
	}
	// Said out loud. A command that edits the user's startup files silently is
	// the worse half of doing it automatically.
	if !strings.Contains(stderr, shimPath()) {
		t.Errorf("the add wrote %s and never said so:\n%s", shimPath(), stderr)
	}
}

// TestASecondCodexAddSaysNothingAboutTheShimAndMovesNoTimestamp is idempotence,
// and it asserts TIMESTAMPS rather than bytes because bytes cannot see the
// failure: replaceFile renames a fresh file over the path, so a rewrite with
// identical content still moves the stamp, and a dotfiles repository then
// reports a change that is not one.
func TestASecondCodexAddSaysNothingAboutTheShimAndMovesNoTimestamp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	stubCodexDevice(t, ownerPayload, nil)

	if code, _, stderr, top := runRoot(t, "add", "codex"); code != ExitOK {
		t.Fatalf("the first add = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}

	home := os.Getenv("HOME")
	// Both startup files bash gets, not just the one: targetFiles writes the
	// interactive file AND the login file, and a guard dropped from the writer
	// moves whichever of them this test forgot.
	touched := []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".profile"), shimPath()}
	past := time.Now().Add(-72 * time.Hour)
	stamps := make(map[string]time.Time, len(touched))
	for _, path := range touched {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatalf("the first add left no %s to stamp: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stamps[path] = info.ModTime()
	}

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("the second add = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	for _, path := range touched {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(stamps[path]) {
			t.Errorf("the second add moved %s's timestamp from %s to %s; the bytes may be identical "+
				"and a dotfiles repository still sees a change",
				path, stamps[path].Format(time.RFC3339), info.ModTime().Format(time.RFC3339))
		}
	}
	// And it says nothing. Exit 3 out of the installer is the whole rule: the
	// world is already as the user asked, so there is nothing to report.
	//
	// "uninstall" is in the list because the undo line belongs to the arm that
	// WROTE something. Printed unconditionally it would land here, on a run that
	// changed nothing, and read as an offer to take back a machine ccdad has
	// just left exactly as it found it.
	for _, word := range []string{"PATH", "shim", "uninstall", shimPath()} {
		if strings.Contains(stderr, word) {
			t.Errorf("the second add still talks about the shim (%q):\n%s", word, stderr)
		}
	}
}

// TestAShimThatCannotBeInstalledDoesNotFailTheAdd is what the hook's signature
// is for. The account is stored and the success line is already printed by the
// time the shim is attempted, so a shim that cannot be installed must not turn
// that into a failure.
//
// SHELL="" drives it because that refusal needs no privilege and no platform:
// ccdad cannot tell which startup file to write, says so, and names the two
// ways out it already has.
func TestAShimThatCannotBeInstalledDoesNotFailTheAdd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	t.Setenv("SHELL", "")
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("a shim that could not be installed failed the add: exit %d, want %d\n%s%s",
			code, ExitOK, stderr, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("user-abc"); !ok {
		t.Fatalf("the login succeeded and the account is not stored: %+v", s.Accounts())
	}
	if !strings.Contains(stderr, "does not reach ccdad yet") {
		t.Errorf("the add never says the shim is missing, so nothing tells the user codex is unrouted:\n%s", stderr)
	}
	// The refusal's OWN remedy, passed through rather than replaced. Re-running
	// `ccdad codex shim install` here would refuse in exactly the same place.
	for _, want := range []string{"$SHELL is unset", "--shell"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal's remedy (%q) did not reach the user:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "to try again") {
		t.Errorf("a refusal that names its own remedy was told to try again unchanged:\n%s", stderr)
	}
}

// TestTheAddThatInstalledTheShimNamesTheUndo is the other half of the automatic
// install. Nothing here was asked for: a login created or edited ~/.bashrc and
// ~/.profile and put an executable called `codex` ahead of the real one on the
// user's PATH, and the only `ccdad uninstall` on the machine is inside the
// block written into the startup file — which is not a place the person who
// has just finished a login reads.
//
// `ccdad uninstall` is the command that takes all three back, and it names the
// shim in its own enumeration, so this pointer lands somewhere rather than
// leaving the user to find out what a removal would cover.
func TestTheAddThatInstalledTheShimNamesTheUndo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	stubCodexDevice(t, ownerPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stderr, top)
	}
	if !strings.Contains(stderr, "`ccdad uninstall`") {
		t.Errorf("the add edited the user's startup files and wrote an executable onto their PATH "+
			"without naming a way to take any of it back:\n%s", stderr)
	}
}

// TestARefusedCodexAddWritesNoShimAndNoStartupFile holds WHERE the hook is
// called from, which is the one thing about it a reader cannot check by reading
// it: the call sits after the success line and after every refusal above it,
// and nothing but this test says so.
//
// The member-seat refusal with no terminal is the sharpest driver there is. It
// exits 2 having stored nothing at all, so every file below belongs to a login
// ccdad declined — and hoisting the call to the top of runCodexAdd leaves the
// package green while that same refusal creates two startup files and puts an
// executable named `codex` ahead of the real one on the user's PATH.
func TestARefusedCodexAddWritesNoShimAndNoStartupFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	stubEnvironment(t, false, false)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	stubCodexDevice(t, memberPayload, nil)

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitUsage, stderr, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts()) != 0 {
		t.Fatalf("the refused login was stored, so this test is no longer describing an add that "+
			"stored nothing: %+v", s.Accounts())
	}
	// fs.ErrNotExist and not "is the content right": the file must not be there
	// at all. A startup file this add created is one the user gets to keep,
	// because nothing on the refusal path knows to remove it again.
	for _, path := range []string{shimPath(), filepath.Join(os.Getenv("HOME"), ".bashrc")} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an add that stored nothing wrote %s (stat: %v)", path, err)
		}
	}
}

// TestAShimTheInstallerCouldNotWriteTellsTheUserToTryAgain is the generic
// failure arm, and it is the only one that gets "try again": a refusal that
// names its own remedy — csh, an unset $SHELL, an ACL, a shim owned by somebody
// else — is passed through untouched, and TestAShimThatCannotBeInstalledDoesNotFailTheAdd
// holds that half. Deleting the arm below leaves only its ABSENCE pinned, which
// is a green that means nothing.
//
// A DIRECTORY where the shim script goes is what drives it. The installer gets
// past its own refusals, past the store, and fails on the write itself, which
// is exactly the shape the arm is for: something went wrong that nothing on the
// machine has said anything about, and running the install again is the only
// thing the user can be told.
func TestAShimTheInstallerCouldNotWriteTellsTheUserToTryAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("there is no shim on Windows; the installer's own refusal has its own test")
	}
	isolate(t)
	realShimAutoInstall(t)
	codexShimEnvironment(t)
	stubCodexDevice(t, ownerPayload, nil)
	if err := os.MkdirAll(shimPath(), 0o700); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, top := runRoot(t, "add", "codex")
	if code != ExitOK {
		t.Fatalf("a shim that could not be written failed the add: exit %d, want %d\n%s%s",
			code, ExitOK, stderr, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("user-abc"); !ok {
		t.Fatalf("the login succeeded and the account is not stored: %+v", s.Accounts())
	}
	if !strings.Contains(stderr, "Run `ccdad codex shim install` to try again.") {
		t.Errorf("the shim could not be written and the user is left with nothing to run:\n%s", stderr)
	}
}
