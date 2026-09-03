package codexusage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// pinnedCapture is the measured body, verbatim in every numeric field.
//
// It was taken with a real bearer against the live endpoint: HTTP 200,
// application/json, 940 bytes. The account is a lapsed free tier, which is why
// there is one 30-day primary window and a null secondary.
//
// The three identity values are placeholders. The capture's own were withheld
// from the record deliberately — they are a real person's user id, workspace id
// and email address — and the SHAPE is what this fixture is for: what each
// field is called, where it sits, and what type it carries.
const pinnedCapture = `{"user_id":"user-pinned","account_id":"ws-pinned","email":"pinned@example.com","plan_type":"free",
 "rate_limit":{"allowed":true,"limit_reached":false,
   "primary_window":{"used_percent":14,"limit_window_seconds":2592000,
                     "reset_after_seconds":2425058,"reset_at":1790688319},
   "secondary_window":null},
 "code_review_rate_limit":null,"additional_rate_limits":null}`

func TestParseReadsThePinnedCapture(t *testing.T) {
	snap, id, err := Parse([]byte(pinnedCapture))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if id.UserID != "user-pinned" {
		t.Errorf("UserID = %q", id.UserID)
	}
	if id.AccountID != "ws-pinned" {
		t.Errorf("AccountID = %q — the workspace, which is what the header carries", id.AccountID)
	}
	if id.Email != "pinned@example.com" {
		t.Errorf("Email = %q", id.Email)
	}
	if id.PlanType != "free" {
		t.Errorf("PlanType = %q, want the raw tier verbatim", id.PlanType)
	}

	if !snap.CodexPrimary.Present {
		t.Fatal("CodexPrimary is absent")
	}
	if p, ok := snap.CodexPrimary.Percent(); !ok || p != 14 {
		t.Errorf("CodexPrimary.Percent() = %v, %v; want 14, true — used_percent is already a percent", p, ok)
	}
	if d, ok := snap.CodexPrimary.Length(); !ok || d != 2592000*time.Second {
		t.Errorf("CodexPrimary.Length() = %s, %v; want 720h out of limit_window_seconds", d, ok)
	}
	// reset_at is epoch SECONDS, and it is the field ccdad reads.
	// reset_after_seconds is the same instant as a countdown and would drift by
	// the request latency.
	at, ok := snap.CodexPrimary.Reset()
	if !ok {
		t.Fatal("CodexPrimary.Reset() reported absent")
	}
	if want := time.Unix(1790688319, 0).UTC(); !at.Equal(want) {
		t.Errorf("CodexPrimary.Reset() = %s, want %s", at, want)
	}

	if snap.CodexSecondary.Present {
		t.Error("CodexSecondary is present; the capture's is null")
	}
	// This account is metered on a 30-day window, so it is weekly-or-longer by
	// measurement rather than by name.
	if !usage.IsWeeklyOf(usage.WindowCodexPrimary, snap.CodexPrimary) {
		t.Error("a 30-day window did not read as weekly-or-longer")
	}
}

// TestParsePinnedPaidCapture reads the SECOND measured body: a paid account,
// which is the only place the two-window shape occurs for real. The free-tier
// capture above has one 30-day window and a null secondary, so on its own it
// leaves the two-window path proven by a body this repository wrote rather
// than one the endpoint sent.
//
// The three identity fields in the file are placeholders. The capture's own
// were a real person's user id, workspace id and address, and the SHAPE is
// what the fixture is for.
func TestParsePinnedPaidCapture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "usage-paid.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, id, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if id.PlanType == "free" || id.PlanType == "" {
		t.Fatalf("PlanType = %q; the paid fixture is the point of this test", id.PlanType)
	}
	if !snap.CodexPrimary.Present || !snap.CodexSecondary.Present {
		t.Fatalf("a paid capture reported primary=%v secondary=%v; both windows are what it is for",
			snap.CodexPrimary.Present, snap.CodexSecondary.Present)
	}
	// Both lengths come from limit_window_seconds and from nothing else. A
	// length inferred from the window's NAME is the bug this fixture exists to
	// catch: the labels are data-driven upstream and a paid account's primary
	// is hours where a free account's is a month.
	primary, ok := snap.CodexPrimary.Length()
	if !ok {
		t.Fatal("CodexPrimary reported no length; limit_window_seconds was present in the capture")
	}
	secondary, ok := snap.CodexSecondary.Length()
	if !ok {
		t.Fatal("CodexSecondary reported no length; limit_window_seconds was present in the capture")
	}
	if want := 18000 * time.Second; primary != want {
		t.Errorf("CodexPrimary.Length() = %s, want %s out of limit_window_seconds", primary, want)
	}
	if want := 604800 * time.Second; secondary != want {
		t.Errorf("CodexSecondary.Length() = %s, want %s out of limit_window_seconds", secondary, want)
	}
	if secondary <= primary {
		t.Errorf("the secondary window (%s) is not longer than the primary (%s); the two are the wrong way round",
			secondary, primary)
	}
}

func TestParseReadsBothWindowsWhenBothArePresent(t *testing.T) {
	body := `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":3.5,"limit_window_seconds":18000,"reset_at":1790688319},` +
		`"secondary_window":{"used_percent":61,"limit_window_seconds":604800,"reset_at":1790788319}}}`
	snap, _, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if p, ok := snap.CodexPrimary.Percent(); !ok || p != 3.5 {
		t.Errorf("CodexPrimary.Percent() = %v, %v; want the fractional percent verbatim", p, ok)
	}
	if d, _ := snap.CodexPrimary.Length(); d != 5*time.Hour {
		t.Errorf("CodexPrimary.Length() = %s, want 5h", d)
	}
	if d, _ := snap.CodexSecondary.Length(); d != 7*24*time.Hour {
		t.Errorf("CodexSecondary.Length() = %s, want 168h", d)
	}
}

// A window that reported no percentage is UNKNOWN. As a zero it looks like the
// emptiest account in the fleet and every ranking hands it the next session.
func TestParseNeverReadsAnUnreportedFieldAsZero(t *testing.T) {
	snap, _, err := Parse([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{}}}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !snap.CodexPrimary.Present {
		t.Fatal("a present-but-empty window lost its presence")
	}
	if p, ok := snap.CodexPrimary.Percent(); ok {
		t.Errorf("Percent() = %v, true; an absent used_percent is not 0%%", p)
	}
	if _, ok := snap.CodexPrimary.Reset(); ok {
		t.Error("an absent reset_at came back as a value")
	}
	if _, ok := snap.CodexPrimary.Length(); ok {
		t.Error("an absent limit_window_seconds came back as a length")
	}
}

func TestParseRefusesABodyThatIsNotAUsageResponse(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not JSON", `<html>`},
		{"not an object", `[1,2]`},
		{"a null body", `null`},
		{"an object with none of the known keys", `{"detail":"forbidden"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Parse([]byte(tc.body)); err == nil {
				t.Error("Parse() = nil for a body that is not a usage response")
			}
		})
	}
	if _, _, err := Parse([]byte(`{"detail":"forbidden"}`)); !errors.Is(err, usage.ErrNoUsageFields) {
		t.Errorf("Parse() = %v, want it to wrap usage.ErrNoUsageFields", err)
	}
}

// An account with no rate_limit object at all is still a reading: the identity
// and the tier came back and the quota did not.
func TestParseAcceptsAResponseWithNoRateLimitObject(t *testing.T) {
	snap, id, err := Parse([]byte(`{"user_id":"u","plan_type":"plus"}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if id.PlanType != "plus" {
		t.Errorf("PlanType = %q", id.PlanType)
	}
	if snap.CodexPrimary.Present || snap.CodexSecondary.Present {
		t.Error("a response with no rate_limit produced a window")
	}
}

func TestFetchSendsTheBearerTheAccountAndTheUserAgent(t *testing.T) {
	var gotPath, gotAuth, gotAccount, gotUA, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, pinnedCapture)
	}))
	defer srv.Close()
	withUsageURL(t, srv.URL+"/backend-api/wham/usage")

	snap, id, err := Fetch(context.Background(), srv.Client(), "AT", "ws-1", "1.2.3")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	// The path prefix is not interchangeable: /backend-api/wham/usage answers
	// 200 and /backend-api/api/codex/usage answers a Cloudflare-shaped 403.
	if gotPath != "/backend-api/wham/usage" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer AT" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccount != "ws-1" {
		t.Errorf("ChatGPT-Account-Id = %q", gotAccount)
	}
	// ccdad names itself. It does not send codex's own user agent, which would
	// be a pinned-version lie about which client made the request.
	if gotUA != "ccdad/1.2.3" {
		t.Errorf("User-Agent = %q, want ccdad/1.2.3", gotUA)
	}
	if id.PlanType != "free" || !snap.CodexPrimary.Present {
		t.Errorf("Fetch() returned %+v / %+v", id, snap.CodexPrimary)
	}
}

// The bearer alone is enough for a 200; the account header only populates the
// account-scoped fields. An empty value must not be sent as an empty header.
func TestFetchOmitsTheAccountHeaderWhenThereIsNoWorkspace(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Chatgpt-Account-Id"]
		io.WriteString(w, pinnedCapture)
	}))
	defer srv.Close()
	withUsageURL(t, srv.URL)

	if _, _, err := Fetch(context.Background(), srv.Client(), "AT", "", "1.2.3"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if present {
		t.Error("an empty workspace was sent as an empty ChatGPT-Account-Id header")
	}
}

func TestFetchReportsA429AsARateLimitCarryingItsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	withUsageURL(t, srv.URL)

	_, _, err := Fetch(context.Background(), srv.Client(), "AT", "ws-1", "1.2.3")
	if err == nil {
		t.Fatal("Fetch() = nil for a 429")
	}
	if !errors.Is(err, usage.ErrRateLimited) {
		t.Errorf("Fetch() = %v, want it to unwrap to usage.ErrRateLimited", err)
	}
	var se *usage.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Fetch() = %v, want a *usage.StatusError", err)
	}
	if se.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d", se.Status)
	}
	d, ok := se.RetryAfter()
	if !ok || d != 42*time.Second {
		t.Errorf("RetryAfter() = %v, %v; want 42s, true", d, ok)
	}
}

func TestFetchReportsA401Distinctly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withUsageURL(t, srv.URL)

	_, _, err := Fetch(context.Background(), srv.Client(), "AT", "ws-1", "1.2.3")
	if !errors.Is(err, usage.ErrUnauthorized) {
		t.Errorf("Fetch() = %v, want it to unwrap to usage.ErrUnauthorized — a 401 means refresh and retry", err)
	}
}

func TestFetchErrorsNeverCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "upstream said something about AT-secret")
	}))
	defer srv.Close()
	withUsageURL(t, srv.URL)

	_, _, err := Fetch(context.Background(), srv.Client(), "AT-secret", "ws-1", "1.2.3")
	if err == nil {
		t.Fatal("Fetch() = nil for a 500")
	}
	if strings.Contains(err.Error(), "AT-secret") {
		t.Errorf("error = %q; a live token must never reach an error string", err)
	}
}

// withUsageURL points Fetch at a local server for one test.
func withUsageURL(t *testing.T, url string) {
	t.Helper()
	saved := usageURL
	t.Cleanup(func() { usageURL = saved })
	usageURL = url
}
