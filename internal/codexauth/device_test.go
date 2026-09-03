package codexauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// withAuthBase points every device and token call at a local server for the
// duration of one test. The real issuer is Cloudflare-challenged on its browser
// pages and rate-limited on its API ones; a test that reached it would be
// testing OpenAI's availability.
func withAuthBase(t *testing.T, base string) {
	t.Helper()
	saved := authBase
	t.Cleanup(func() { authBase = saved })
	authBase = base
}

// bodyOf reads a request body in a handler, failing the test rather than the
// request when it cannot.
func bodyOf(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	return string(b)
}

// The exported URLs are the contract the rest of ccdad and its documentation
// name. Pinning them here is what stops a refactor of authBase from silently
// moving an endpoint.
func TestTheOAuthConstantsAreTheMeasuredOnes(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{ClientID, "app_EMoamEEZ73f0CkXaXp7hrann"},
		{AuthBaseURL, "https://auth.openai.com"},
		{DeviceAuthURL, "https://auth.openai.com/api/accounts/deviceauth/usercode"},
		{DeviceTokenURL, "https://auth.openai.com/api/accounts/deviceauth/token"},
		{DeviceVerifyURL, "https://auth.openai.com/codex/device"},
		{DeviceRedirectURI, "https://auth.openai.com/deviceauth/callback"},
		{TokenURL, "https://auth.openai.com/oauth/token"},
	} {
		if tc.got != tc.want {
			t.Errorf("constant = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestStartDeviceLoginAsksForAUserCode(t *testing.T) {
	var gotPath, gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		gotBody = bodyOf(t, r)
		w.Header().Set("Content-Type", "application/json")
		// interval is a STRING on the wire. Typed as a number here the
		// unmarshal fails and the whole login dies at step one.
		io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"ABCD-EFGH","interval":"7"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	before := time.Now()
	start, err := StartDeviceLogin(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}

	if gotPath != "/api/accounts/deviceauth/usercode" {
		t.Errorf("path = %q, want /api/accounts/deviceauth/usercode", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("the request body is not JSON: %v (%s)", err, gotBody)
	}
	if req["client_id"] != ClientID {
		t.Errorf("client_id = %v, want %q", req["client_id"], ClientID)
	}
	if len(req) != 1 {
		t.Errorf("request body = %s, want the client id and nothing else", gotBody)
	}
	if start.DeviceAuthID != "dev-1" || start.UserCode != "ABCD-EFGH" {
		t.Errorf("start = %+v", start)
	}
	if start.Interval != 7*time.Second {
		t.Errorf("Interval = %s, want 7s parsed out of the string form", start.Interval)
	}
	if start.ExpiresAt.Before(before.Add(DeviceLoginWindow)) {
		t.Errorf("ExpiresAt = %s, want at least %s out", start.ExpiresAt, DeviceLoginWindow)
	}
}

// An absent or unreadable interval is not a licence to poll as fast as the
// process can loop. The server sends the field as a string and a build that
// read it as 0 would hammer the endpoint until it expired the code.
func TestStartDeviceLoginFloorsAnUnusableInterval(t *testing.T) {
	for _, body := range []string{
		`{"device_auth_id":"dev-1","user_code":"C"}`,
		`{"device_auth_id":"dev-1","user_code":"C","interval":""}`,
		`{"device_auth_id":"dev-1","user_code":"C","interval":"0"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		withAuthBase(t, srv.URL)
		start, err := StartDeviceLogin(context.Background(), srv.Client())
		srv.Close()
		if err != nil {
			t.Fatalf("StartDeviceLogin() error = %v for %s", err, body)
		}
		if start.Interval != DefaultDeviceInterval {
			t.Errorf("Interval = %s for %s, want the %s floor", start.Interval, body, DefaultDeviceInterval)
		}
	}
}

// `usercode` is an accepted spelling of the same field, so a response that uses
// it must not produce an empty code the user is then asked to type.
func TestStartDeviceLoginAcceptsTheUsercodeSpelling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"device_auth_id":"dev-1","usercode":"WXYZ","interval":"5"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	start, err := StartDeviceLogin(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("StartDeviceLogin() error = %v", err)
	}
	if start.UserCode != "WXYZ" {
		t.Errorf("UserCode = %q, want WXYZ", start.UserCode)
	}
}

func TestStartDeviceLoginReportsANonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	if _, err := StartDeviceLogin(context.Background(), srv.Client()); err == nil {
		t.Fatal("StartDeviceLogin() = nil for a 503")
	} else if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want it to name the status", err)
	}
}

// The whole flow: poll until the code is approved, then exchange the
// authorization code with the PKCE verifier the poll handed back.
func TestPollDeviceLoginPollsThenExchanges(t *testing.T) {
	var polls int
	var pollBody, exchangeBody, exchangeType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/token":
			polls++
			pollBody = bodyOf(t, r)
			switch polls {
			case 1:
				// 403 means "not approved yet".
				w.WriteHeader(http.StatusForbidden)
				return
			case 2:
				// 404 means the same thing as 403; the continue-vs-fail split
				// must treat both as "not yet" rather than only one of them.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"authorization_code":"code-1","code_challenge":"chal","code_verifier":"ver"}`)
		case "/oauth/token":
			exchangeType = r.Header.Get("Content-Type")
			exchangeBody = bodyOf(t, r)
			io.WriteString(w, `{"id_token":"`+jwt(fullPayload)+`","access_token":"AT","refresh_token":"RT"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	var slept []time.Duration
	start := DeviceStart{
		DeviceAuthID: "dev-1",
		UserCode:     "ABCD",
		Interval:     3 * time.Second,
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	cred, err := PollDeviceLogin(context.Background(), srv.Client(), start,
		func(d time.Duration) { slept = append(slept, d) })
	if err != nil {
		t.Fatalf("PollDeviceLogin() error = %v", err)
	}

	if polls != 3 {
		t.Errorf("polls = %d, want 3", polls)
	}
	if len(slept) != 2 || slept[0] != 3*time.Second || slept[1] != 3*time.Second {
		t.Errorf("slept = %v, want two waits of the advertised interval", slept)
	}
	var poll map[string]any
	if err := json.Unmarshal([]byte(pollBody), &poll); err != nil {
		t.Fatalf("the poll body is not JSON: %v (%s)", err, pollBody)
	}
	if poll["device_auth_id"] != "dev-1" || poll["user_code"] != "ABCD" {
		t.Errorf("poll body = %s, want the device id and the user code", pollBody)
	}
	if len(poll) != 2 {
		t.Errorf("poll body = %s, want those two fields and nothing else", pollBody)
	}

	if exchangeType != "application/x-www-form-urlencoded" {
		t.Errorf("exchange Content-Type = %q, want form encoding — the exchange is form and the refresh is JSON, and mixing them is refused upstream", exchangeType)
	}
	form, err := url.ParseQuery(exchangeBody)
	if err != nil {
		t.Fatalf("the exchange body is not form-encoded: %v", err)
	}
	for _, tc := range []struct{ key, want string }{
		{"grant_type", "authorization_code"},
		{"code", "code-1"},
		{"redirect_uri", DeviceRedirectURI},
		{"client_id", ClientID},
		{"code_verifier", "ver"},
	} {
		if got := form.Get(tc.key); got != tc.want {
			t.Errorf("exchange %s = %q, want %q", tc.key, got, tc.want)
		}
	}

	if cred.AccessToken != "AT" {
		t.Errorf("AccessToken = %q, want %q", cred.AccessToken, "AT")
	}
	if cred.RefreshToken != "RT" {
		t.Errorf("RefreshToken = %q, want %q", cred.RefreshToken, "RT")
	}
	if cred.UserID != "user-abc" {
		t.Errorf("UserID = %q, want the chatgpt_user_id out of the id_token", cred.UserID)
	}
	if cred.AccountID != "ws-1" {
		t.Errorf("AccountID = %q, want the chatgpt_account_id out of the id_token", cred.AccountID)
	}
	if cred.LastRefresh.IsZero() {
		t.Error("LastRefresh is zero; the credential was minted now")
	}
}

// A code nobody approved must stop, and it must stop for the reason it stopped
// for. Without the expiry the loop is unbounded.
func TestPollDeviceLoginGivesUpWhenTheCodeExpires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	start := DeviceStart{
		DeviceAuthID: "dev-1",
		UserCode:     "ABCD",
		Interval:     time.Second,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	_, err := PollDeviceLogin(context.Background(), srv.Client(), start, func(time.Duration) {
		t.Error("an expired code must not sleep")
	})
	if err == nil {
		t.Fatal("PollDeviceLogin() = nil for an expired code")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want it to say the code expired", err)
	}
}

// Only 403 and 404 mean "waiting". Anything else is a failure to report rather
// than a reason to keep asking.
func TestPollDeviceLoginStopsOnAnUnexpectedStatus(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	start := DeviceStart{DeviceAuthID: "d", UserCode: "c", Interval: time.Second, ExpiresAt: time.Now().Add(time.Hour)}
	// The sleep FAILS rather than no-ops. A build that treated a 500 as a wait
	// state would otherwise spin against this handler for the code's whole
	// hour-long life and be caught only by go test's own timeout, which is a
	// ten-minute panic rather than a test result.
	_, err := PollDeviceLogin(context.Background(), srv.Client(), start, func(time.Duration) {
		t.Fatal("a 500 must not be waited on")
	})
	if err == nil {
		t.Fatal("PollDeviceLogin() = nil for a 500")
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1 — a 500 is not a wait state", polls)
	}
}

func TestPollDeviceLoginReportsAFailedExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/accounts/deviceauth/token" {
			io.WriteString(w, `{"authorization_code":"c","code_challenge":"h","code_verifier":"v"}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	start := DeviceStart{DeviceAuthID: "d", UserCode: "c", Interval: time.Second, ExpiresAt: time.Now().Add(time.Hour)}
	_, err := PollDeviceLogin(context.Background(), srv.Client(), start, func(time.Duration) {})
	if err == nil {
		t.Fatal("PollDeviceLogin() = nil for a refused exchange")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %q, want it to name the status", err)
	}
}
