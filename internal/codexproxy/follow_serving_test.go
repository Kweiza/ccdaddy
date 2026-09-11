package codexproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
)

func TestExistingConversationUsesNewServingCredentialsOnItsNextRequest(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "replay off", true: "replay on"}[replay], func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(`{"type":"response.completed"}`))
			})
			f.add("uuid-a", "a@example.com", "access-a")
			f.add("uuid-b", "b@example.com", "access-b")
			cfg := f.config()
			cfg.CrossAccountReplay = replay
			s := f.server(t, cfg)
			const body = `{"input":[{"type":"message","role":"user","content":"existing conversation"},{"type":"reasoning","encrypted_content":"opaque-history"}]}`
			steps := []struct{ account, turnState, wantState string }{
				{"uuid-a", "from-before-restart", ""},
				{"uuid-a", "from-a", "from-a"},
				{"uuid-b", "from-a", ""},
				{"uuid-b", "from-b", "from-b"},
			}
			for i, step := range steps {
				f.serving(t, step.account)
				w := post(s, unpinnedSecret, map[string]string{threadIDHeader: "existing-thread", turnStateHeader: step.turnState}, body)
				if w.Code != http.StatusOK {
					t.Fatalf("request %d: status %d", i, w.Code)
				}
				took := f.took()
				if len(took) != i+1 {
					t.Fatalf("request %d retried despite a successful response", i)
				}
				req := took[i]
				token := "Bearer access-a"
				if step.account == "uuid-b" {
					token = "Bearer access-b"
				}
				if req.header.Get("Authorization") != token || req.header.Get(accountIDHeader) != "workspace-"+step.account {
					t.Fatalf("request %d used wrong account headers: %s / %s", i, req.header.Get("Authorization"), req.header.Get(accountIDHeader))
				}
				if got := req.header.Get(turnStateHeader); got != step.wantState {
					t.Fatalf("request %d turn state = %q, want %q", i, got, step.wantState)
				}
				if string(req.body) != body {
					t.Fatalf("request %d changed conversation history", i)
				}
			}
			if uuid, _ := s.threadAccount("existing-thread"); uuid != "uuid-b" {
				t.Fatalf("last responding account = %s", uuid)
			}
			if !strings.Contains(strings.Join(f.logs, "\n"), "is using account uuid-b") {
				t.Fatal("account change was not logged")
			}
		})
	}
}

func TestExplicitLaunchPinStillWinsAfterServingChanges(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sse(`{"type":"response.completed"}`)) })
	f.add("uuid-a", "a@example.com", "access-a")
	f.add("uuid-b", "b@example.com", "access-b")
	s := f.server(t, f.config())
	for _, uuid := range []string{"uuid-a", "uuid-b"} {
		f.serving(t, uuid)
		w := post(s, pinnedPrefix+"uuid-a", map[string]string{threadIDHeader: "pinned-thread"}, `{"input":[]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
	}
	for _, req := range f.took() {
		if req.header.Get("Authorization") != "Bearer access-a" {
			t.Fatal("explicit pin followed the serving pointer")
		}
	}
}

func TestMissingServingPointerFallsBackToTheLastRespondingAccount(t *testing.T) {
	_, s := threeAccounts(t)
	s.rememberThread("thread-1", "uuid-b")
	order, pinned := s.chooseOrder(codexlaunch.Record{}, "thread-1")
	if pinned || len(order) == 0 || order[0] != "uuid-b" {
		t.Fatalf("fallback order = %v, pinned %v", order, pinned)
	}
}
