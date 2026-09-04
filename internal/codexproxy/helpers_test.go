package codexproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The two bearers the fixture's launch lookup understands. A bearer of the
// second shape carries the launch pin after the colon.
const (
	unpinnedSecret = "secret-unpinned"
	pinnedPrefix   = "secret-pinned:"
)

// recordedRequest is what the fake upstream saw.
type recordedRequest struct {
	header http.Header
	body   []byte
	length int64
}

// fixture is a fake upstream, a fake account store and a fake launch lookup.
type fixture struct {
	root     string
	upstream *httptest.Server

	mu       sync.Mutex
	accounts []store.Account
	creds    map[string]cclink.Blob
	requests []recordedRequest
	logs     []string

	// accountsErr, when set, is what the account reader returns instead of rows.
	accountsErr error
}

// newFixture starts a fake upstream that runs h and records every request.
func newFixture(t *testing.T, h http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{root: filepath.Join(t.TempDir(), "ccdad"), creds: map[string]cclink.Blob{}}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{header: r.Header.Clone(), body: body, length: r.ContentLength})
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.upstream.Close)
	return f
}

// add stores one Codex account and its credential blob.
func (f *fixture) add(uuid, email, accessToken string) store.Account {
	a := store.Account{
		UUID:     uuid,
		Email:    email,
		Idx:      len(f.accounts) + 1,
		Kind:     identity.KindSubscription,
		Provider: provider.Codex,
	}
	f.mu.Lock()
	f.accounts = append(f.accounts, a)
	f.creds[uuid] = codexauth.Credential{
		AccessToken:  accessToken,
		RefreshToken: "refresh-" + uuid,
		AccountID:    "workspace-" + uuid,
		UserID:       uuid,
	}.ToBlob()
	f.mu.Unlock()
	return a
}

// config is the Config a proxy test builds a server from.
func (f *fixture) config() Config {
	return Config{
		Root:     f.root,
		Version:  "test",
		Upstream: f.upstream.URL,
		Client:   f.upstream.Client(),
		Accounts: func() ([]store.Account, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.accountsErr != nil {
				return nil, f.accountsErr
			}
			return append([]store.Account(nil), f.accounts...), nil
		},
		Credentials: func(uuid string) (cclink.Blob, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			b, ok := f.creds[uuid]
			if !ok {
				return nil, fmt.Errorf("no stored credentials for %s", uuid)
			}
			return b, nil
		},
		Lookup: func(_, bearer string) (codexlaunch.Record, codexlaunch.LookupResult, error) {
			switch {
			case bearer == unpinnedSecret:
				return codexlaunch.Record{}, codexlaunch.Valid, nil
			case strings.HasPrefix(bearer, pinnedPrefix):
				return codexlaunch.Record{Pin: strings.TrimPrefix(bearer, pinnedPrefix)}, codexlaunch.Valid, nil
			}
			return codexlaunch.Record{}, codexlaunch.Unknown, nil
		},
		Log: func(format string, a ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.logs = append(f.logs, fmt.Sprintf(format, a...))
		},
	}
}

// server builds a handler-only server: it binds nothing, so a test can drive it
// with a recorder and never take a port.
func (f *fixture) server(t *testing.T, cfg Config) *Server {
	t.Helper()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatalf("newServer() = %v, want nil", err)
	}
	return s
}

// took returns the requests the fake upstream has seen so far.
func (f *fixture) took() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// logged returns everything the Log hook was handed, already formatted.
func (f *fixture) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

// post drives one POST /responses through the handler.
func post(s *Server, bearer string, headers map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// sse is a fake upstream body: one event, then the terminator codex sends.
func sse(payload string) string {
	return "event: response.output_text.delta\ndata: " + payload + "\n\ndata: [DONE]\n\n"
}
