package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	code, _, stderr, top := runRoot(t, "codex", "add")
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

	_, _, stderr, _ := runRoot(t, "codex", "add")
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

	code, _, stderr, top := runRoot(t, "codex", "add")
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

	code, _, stderr, top := runRoot(t, "codex", "add", "--allow-workspace-member")
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

	cmd := newCodexAddCmd()
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
	if code, _, _, top := runRoot(t, "codex", "add"); code != ExitOK {
		t.Fatalf("the first add failed: %s", top)
	}

	stubCodexDevice(t, memberPayload, nil)
	code, _, _, top := runRoot(t, "codex", "add", "--allow-workspace-member")
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

	if code, _, _, top := runRoot(t, "codex", "add"); code != ExitOK {
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

	code, _, _, top := runRoot(t, "codex", "add")
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
	if !strings.Contains(top, "add") {
		t.Errorf("the refusal does not name a subcommand:\n%s", top)
	}
}

// Both new paths are ALLOWED in a scoped session. A session scopes Claude
// Code's credential and config homes and nothing else, and this command writes
// only ccdad's own store — which is also why `ccdad add` stays refused and this
// one does not: that one activates a Claude login.
func TestTheCodexPathsAreAllowedInsideARunSession(t *testing.T) {
	for _, path := range []string{"ccdad codex", "ccdad codex add"} {
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
	if code, _, _, top := runRoot(t, "codex", "add"); code != ExitOK {
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
