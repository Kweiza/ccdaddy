package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

func claudeLogin(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT","refreshToken":"` + refresh + `"}`)}
}

// Replacing an account's stored login remembers the one replaced, so an older
// generation still sitting in a login store Claude Code has not rewritten is
// named without asking the profile endpoint -- which cannot answer for an
// expired access token.
func TestReplacingAStoredLoginRemembersTheOneReplaced(t *testing.T) {
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	a := Account{UUID: "u-1", Provider: provider.Claude}
	if err := s.Add(a, claudeLogin("RT-0")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxSupersededLogins+2; i++ {
		got, _ := s.Get("u-1")
		if err := s.Add(got, claudeLogin(fmt.Sprintf("RT-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("u-1")
	last := fmt.Sprintf("refresh:RT-%d", maxSupersededLogins+1)
	if !got.Superseded(last) {
		t.Fatalf("the login replaced last is not remembered: %q", got.SupersededLogins)
	}
	if got.Superseded(fmt.Sprintf("refresh:RT-%d", maxSupersededLogins+2)) {
		t.Fatal("the login stored now is listed as superseded")
	}
	if got.Superseded("refresh:RT-0") {
		t.Fatalf("the history is not bounded: %d entries", len(strings.Fields(got.SupersededLogins)))
	}
	if strings.Contains(got.SupersededLogins, "RT-") {
		t.Fatalf("a refresh token was written into accounts.toml: %q", got.SupersededLogins)
	}
}
