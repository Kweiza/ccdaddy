package codexauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// withStore points ccdad's store at a temporary directory for one test, the
// way internal/store's own suite does.
func withStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ccdad")
	t.Setenv("CCDAD_HOME", dir)
	return dir
}

func seedCodex(t *testing.T, uuid string, cred Credential) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	acct := store.Account{
		UUID:             uuid,
		Email:            uuid + "@example.com",
		Provider:         provider.Codex,
		Kind:             identity.KindSubscription,
		OrganizationUUID: cred.AccountID,
	}
	if err := s.Add(acct, cred.ToBlob()); err != nil {
		t.Fatalf("seeding %s: %v", uuid, err)
	}
}

func storedCredential(t *testing.T, uuid string) Credential {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.Credentials(uuid)
	if err != nil {
		t.Fatal(err)
	}
	cred, ok, err := FromBlob(blob)
	if err != nil || !ok {
		t.Fatalf("FromBlob() = %v, %v, %v", cred, ok, err)
	}
	return cred
}

func storedAccount(t *testing.T, uuid string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("%s is not in the store", uuid)
	}
	return a
}

// The classifier is Codex-specific on purpose. ccdad's Claude-side wire
// classifier calls refresh_token_reused a TRANSIENT failure, and used here it
// would retry a grant the server has already burned — once per client retry,
// forever, against an account nothing can save but a new login.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		err      error
		wantKind OutcomeKind
		wantCode string
	}{
		{"401 with no body at all", 401, "", nil, Terminal, "unauthorized"},
		{"401 with an unrelated body", 401, `{"error":"nope"}`, nil, Terminal, "nope"},
		{"400 invalid_grant", 400, `{"error":{"code":"invalid_grant"}}`, nil, Terminal, "invalid_grant"},
		{"400 invalid_grant as a bare string", 400, `{"error":"invalid_grant"}`, nil, Terminal, "invalid_grant"},
		{"expired, in error.code", 403, `{"error":{"code":"refresh_token_expired"}}`, nil, Terminal, "refresh_token_expired"},
		{"reused, top level", 403, `{"code":"refresh_token_reused"}`, nil, Terminal, "refresh_token_reused"},
		{"invalidated, in a 500", 500, `{"error":{"code":"refresh_token_invalidated"}}`, nil, Terminal, "refresh_token_invalidated"},
		{"the code is matched case-insensitively", 403, `{"code":"REFRESH_TOKEN_REUSED"}`, nil, Terminal, "refresh_token_reused"},
		{"503", 503, `{"error":"upstream"}`, nil, Transient, "http_503"},
		{"429 from the token endpoint", 429, "", nil, Transient, "http_429"},
		{"400 that is not invalid_grant", 400, `{"error":{"code":"bad_request"}}`, nil, Transient, "http_400"},
		{"a timeout", 0, "", context.DeadlineExceeded, Transient, "timeout"},
		{"a cancelled context", 0, "", context.Canceled, Transient, "cancelled"},
		{"any other transport failure", 0, "", errors.New("connection reset"), Transient, "transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, code := Classify(tc.status, []byte(tc.body), tc.err)
			if kind != tc.wantKind || code != tc.wantCode {
				t.Errorf("Classify() = %s, %q; want %s, %q", kind, code, tc.wantKind, tc.wantCode)
			}
		})
	}
}

// The single-use grant is the whole reason this type exists. N callers seeing
// the same 401 must produce ONE exchange: a second one presents a token the
// server has already rotated, which is reuse detection, which is terminal.
func TestNConcurrentRefreshesSpendOneGrant(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT-old", RefreshToken: "RT-old", AccountID: "ws-1", UserID: "user-1"})

	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		io.WriteString(w, `{"id_token":"ID-new","access_token":"AT-new","refresh_token":"RT-new"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	r := NewRefresher(RefresherConfig{Client: srv.Client()})

	const callers = 8
	var wg sync.WaitGroup
	outcomes := make([]Outcome, callers)
	errs := make([]error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = r.Refresh(context.Background(), "user-1", "AT-old")
		}(i)
	}
	wg.Wait()

	if got := posts.Load(); got != 1 {
		t.Fatalf("the token endpoint saw %d exchanges, want exactly 1 — a second one presents a spent grant", got)
	}
	var rotated, adopted int
	for i, o := range outcomes {
		if errs[i] != nil {
			t.Fatalf("caller %d: Refresh() error = %v", i, errs[i])
		}
		switch o.Kind {
		case Rotated:
			rotated++
		case Adopted:
			adopted++
		default:
			t.Errorf("caller %d: Kind = %s, want Rotated or Adopted", i, o.Kind)
		}
		if o.Credential.AccessToken != "AT-new" {
			t.Errorf("caller %d: AccessToken = %q, want the rotated one", i, o.Credential.AccessToken)
		}
	}
	if rotated != 1 || adopted != callers-1 {
		t.Errorf("rotated = %d, adopted = %d; want 1 and %d", rotated, adopted, callers-1)
	}
	if got := storedCredential(t, "user-1"); got.AccessToken != "AT-new" || got.RefreshToken != "RT-new" {
		t.Errorf("stored credential = %+v, want the rotated pair", got)
	}
}

// The response's three fields are each optional and each overwritten only when
// present. A build that wrote them unconditionally would blank a refresh token
// on a response that only rotated the access token.
func TestRefreshKeepsAFieldTheResponseOmitted(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{IDToken: "ID-old", AccessToken: "AT-old", RefreshToken: "RT-old"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"AT-new"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	out, err := NewRefresher(RefresherConfig{Client: srv.Client()}).Refresh(context.Background(), "user-1", "AT-old")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if out.Kind != Rotated {
		t.Fatalf("Kind = %s, want Rotated", out.Kind)
	}
	got := storedCredential(t, "user-1")
	if got.AccessToken != "AT-new" {
		t.Errorf("AccessToken = %q, want AT-new", got.AccessToken)
	}
	if got.RefreshToken != "RT-old" {
		t.Errorf("RefreshToken = %q, want the stored one kept — the response omitted it", got.RefreshToken)
	}
	if got.IDToken != "ID-old" {
		t.Errorf("IDToken = %q, want the stored one kept", got.IDToken)
	}
	if got.LastRefresh.IsZero() {
		t.Error("LastRefresh is zero after a rotation")
	}
}

// The mark is a compare-and-swap on the REJECTED token. Written unconditionally
// it would quarantine an account whose grant a concurrent login had already
// replaced.
func TestATerminalFailureMarksTheAccountForRelogin(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT", RefreshToken: "RT"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"refresh_token_reused"}}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	var logged []string
	r := NewRefresher(RefresherConfig{
		Client: srv.Client(),
		Log:    func(format string, a ...any) { logged = append(logged, format) },
	})
	out, err := r.Refresh(context.Background(), "user-1", "AT")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if out.Kind != Terminal || out.Code != "refresh_token_reused" {
		t.Fatalf("outcome = %s/%q, want Terminal/refresh_token_reused", out.Kind, out.Code)
	}
	if len(logged) != 1 {
		t.Errorf("logged %d lines, want exactly one", len(logged))
	}
	acct := storedAccount(t, "user-1")
	if !acct.NeedsRelogin(RefreshTokenHash("RT")) {
		t.Errorf("CodexReloginFor = %q, want the hash of the rejected token", acct.CodexReloginFor)
	}
	if acct.NeedsRelogin(RefreshTokenHash("RT-different")) {
		t.Error("the mark matches a token that was never rejected")
	}
}

// A transient failure teaches nothing about the grant, so it must not mark the
// account — and it must stop the next caller reaching the endpoint at all,
// because codex answers a 401 with six retries in six seconds.
func TestATransientFailureCoolsDownInsteadOfMarking(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT", RefreshToken: "RT"})

	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r := NewRefresher(RefresherConfig{Client: srv.Client(), Now: func() time.Time { return now }})

	first, err := r.Refresh(context.Background(), "user-1", "AT")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if first.Kind != Transient || first.Code != "http_503" {
		t.Fatalf("first outcome = %s/%q, want Transient/http_503", first.Kind, first.Code)
	}
	if acct := storedAccount(t, "user-1"); acct.CodexReloginFor != "" {
		t.Errorf("CodexReloginFor = %q; a transient failure says nothing about the grant", acct.CodexReloginFor)
	}

	second, err := r.Refresh(context.Background(), "user-1", "AT")
	if err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if second.Kind != Transient || second.Code != "cooldown" {
		t.Errorf("second outcome = %s/%q, want Transient/cooldown", second.Kind, second.Code)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("the endpoint saw %d attempts, want 1 — the second was inside the cooldown", got)
	}

	// Past the cooldown the account is tried again: a cooldown is a pause, not
	// a quarantine.
	now = now.Add(DefaultRefreshCooldown + time.Second)
	if _, err := r.Refresh(context.Background(), "user-1", "AT"); err != nil {
		t.Fatalf("third Refresh() error = %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("the endpoint saw %d attempts, want 2 once the cooldown lapsed", got)
	}
}

// An account another machine drives is not this machine's to spend. Refreshing
// it rotates a grant the other ccdad is about to present.
func TestElsewhereIsNeverRefreshed(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT", RefreshToken: "RT"})
	seedCodex(t, "user-2", Credential{AccessToken: "AT2", RefreshToken: "RT2"})
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOwned([]string{"user-2"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an Elsewhere account reached the token endpoint")
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	out, err := NewRefresher(RefresherConfig{Client: srv.Client()}).Refresh(context.Background(), "user-1", "AT")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if out.Kind != Transient || out.Code != "elsewhere" {
		t.Errorf("outcome = %s/%q, want Transient/elsewhere", out.Kind, out.Code)
	}
}

// A caller whose access token is not the stored one has already been overtaken.
// Exchanging again would spend a grant for a token it already has.
func TestARefreshTriggeredByAStaleTokenAdoptsInstead(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT-current", RefreshToken: "RT"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an adopt reached the token endpoint")
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	out, err := NewRefresher(RefresherConfig{Client: srv.Client()}).Refresh(context.Background(), "user-1", "AT-stale")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if out.Kind != Adopted {
		t.Fatalf("Kind = %s, want Adopted", out.Kind)
	}
	if out.Credential.AccessToken != "AT-current" {
		t.Errorf("AccessToken = %q, want the stored one", out.Credential.AccessToken)
	}
}

func TestRefreshingAnAccountWithNoCodexCredentialIsAnError(t *testing.T) {
	withStore(t)
	if _, err := NewRefresher(RefresherConfig{}).Refresh(context.Background(), "nobody", "AT"); err == nil {
		t.Fatal("Refresh() = nil for an account that is not in the store")
	}
}

func TestTheRefreshRequestIsTheJSONOne(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT", RefreshToken: "RT"})

	var gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody = bodyOf(t, r)
		io.WriteString(w, `{"access_token":"AT-new"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	if _, err := NewRefresher(RefresherConfig{Client: srv.Client()}).Refresh(context.Background(), "user-1", "AT"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	// The exchange is form-encoded and the refresh is JSON. They are two
	// different requests to one URL and the encodings are not interchangeable.
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	for _, want := range []string{`"client_id":"` + ClientID + `"`, `"grant_type":"refresh_token"`, `"refresh_token":"RT"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body = %s, want it to carry %s", gotBody, want)
		}
	}
}

// holdStoreLock takes ccdad's cross-process store lock in a background
// goroutine and keeps it until the returned func is called.
//
// It shortens store.LockTimeout first, so a caller that finds the lock busy
// gives up in 50 ms rather than in the five seconds a real contention wait
// takes. The cleanup releases the goroutine and restores the timeout, in that
// order, so a test that forgets to call release still tears down rather than
// leaking a locked store into the next one.
func holdStoreLock(t *testing.T) func() {
	t.Helper()
	saved := store.LockTimeout
	store.LockTimeout = 50 * time.Millisecond

	held := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = store.WithStore(func(*store.Store) error {
			close(held)
			<-done
			return nil
		})
	}()
	<-held

	var once sync.Once
	release := func() {
		once.Do(func() { close(done) })
		<-finished
	}
	t.Cleanup(func() {
		release()
		store.LockTimeout = saved
	})
	return release
}

// A busy store is a PAUSE, not a verdict. Nothing was learned about the grant
// and the token endpoint was never reached, so returning an error here would
// have the caller read contention as a refresh failure — and a cooldown would
// be wrong too, because no attempt was made.
func TestABusyStoreLockIsTransientOnTheRead(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT", RefreshToken: "RT"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the token endpoint was reached while the store was locked")
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	release := holdStoreLock(t)
	defer release()

	out, err := NewRefresher(RefresherConfig{Client: srv.Client()}).Refresh(context.Background(), "user-1", "AT")
	if err != nil {
		t.Fatalf("Refresh() error = %v; a busy store is an outcome, not an error", err)
	}
	if out.Kind != Transient || out.Code != "lock_busy" {
		t.Errorf("outcome = %s/%q, want Transient/lock_busy", out.Kind, out.Code)
	}
}

// The grant is single-use, so a rotation the store could not take must not be
// thrown away. The POST already spent it and the only copy of the new refresh
// token is in this process; dropping it would leave the stored pair naming a
// token the issuer has burned, and the account's next refresh would be
// reuse-detected and Terminal — an account destroyed by a lock.
func TestARotationTheStoreCouldNotTakeIsHeldAndLandedNext(t *testing.T) {
	withStore(t)
	seedCodex(t, "user-1", Credential{AccessToken: "AT-old", RefreshToken: "RT-old"})

	var posts atomic.Int64
	// The release func travels back over a channel rather than through a shared
	// variable: the handler runs on the server's goroutine and the assertions
	// run on the test's, and the channel is what orders the two.
	released := make(chan func(), 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			// Taken WHILE the exchange is in flight, so the save that follows
			// it finds the store busy. That window is the whole of this test.
			released <- holdStoreLock(t)
		}
		io.WriteString(w, `{"access_token":"AT-new","refresh_token":"RT-new"}`)
	}))
	defer srv.Close()
	withAuthBase(t, srv.URL)

	r := NewRefresher(RefresherConfig{Client: srv.Client()})

	first, err := r.Refresh(context.Background(), "user-1", "AT-old")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if first.Kind != Rotated || first.Credential.AccessToken != "AT-new" {
		t.Fatalf("first outcome = %s carrying %q, want Rotated carrying AT-new",
			first.Kind, first.Credential.AccessToken)
	}
	// Reading the files takes no lock, so this is readable while the lock is
	// still held.
	if got := storedCredential(t, "user-1"); got.RefreshToken != "RT-old" {
		t.Fatalf("stored RefreshToken = %q, want RT-old — the busy store took nothing", got.RefreshToken)
	}
	(<-released)()

	second, err := r.Refresh(context.Background(), "user-1", "AT-old")
	if err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if second.Kind != Adopted {
		t.Fatalf("second outcome = %s, want Adopted — the held pair lands before the read, so the stored access token is no longer AT-old", second.Kind)
	}
	if second.Credential.AccessToken != "AT-new" {
		t.Errorf("second AccessToken = %q, want AT-new", second.Credential.AccessToken)
	}
	if got := storedCredential(t, "user-1"); got.AccessToken != "AT-new" || got.RefreshToken != "RT-new" {
		t.Errorf("stored credential = %+v, want the held pair landed", got)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("the token endpoint saw %d exchanges, want 1 — the second call lands the held pair instead of spending another grant", got)
	}
}
