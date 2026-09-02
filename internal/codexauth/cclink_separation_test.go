package codexauth

import (
	"encoding/json"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// The Codex record is ccdad's own and Claude Code has never heard of it, so it
// must not be in the list of keys that travel with a Claude login.
//
// Being in that list would make it account-scoped, and account-scoped is what
// Merge layers onto Claude Code's live credentials file on every switch. One
// account's Codex refresh token would then be written into the file Claude
// Code rewrites on startup, on a path with no rule on it.
func TestTheCodexKeyIsNotAccountScoped(t *testing.T) {
	if cclink.IsAccountScoped(Key) {
		t.Fatalf("%s is in cclink.AccountScopedKeys; a Codex token would then be merged into "+
			"Claude Code's live credentials file on every switch", Key)
	}
	for _, k := range cclink.AccountScopedKeys {
		if k == Key {
			t.Fatalf("%s appears in cclink.AccountScopedKeys", Key)
		}
	}
}

// Merge must leave the key alone in both directions.
//
// From the incoming side it must not be COPIED IN: a Codex snapshot handed to
// a Claude switch would put a Codex token in the live file. From the live side
// it must not be DROPPED: Merge preserves everything it does not recognize,
// and a live file that somehow holds this key belongs to a machine whose state
// this switch has no mandate to edit.
func TestMergeLeavesTheCodexKeyAlone(t *testing.T) {
	incoming := cclink.Blob{
		"claudeAiOauth": json.RawMessage(`{"accessToken":"AT"}`),
		Key:             json.RawMessage(`{"refresh_token":"RT-LEAK"}`),
	}
	out := cclink.Merge(cclink.Blob{"mcpOAuth": json.RawMessage(`{"m":1}`)}, incoming)
	if _, leaked := out[Key]; leaked {
		t.Fatalf("Merge copied %s out of an incoming snapshot into the live file", Key)
	}

	live := cclink.Blob{
		"mcpOAuth": json.RawMessage(`{"m":1}`),
		Key:        json.RawMessage(`{"refresh_token":"RT-LIVE"}`),
	}
	kept := cclink.Merge(live, cclink.Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT"}`)})
	got, ok := kept[Key]
	if !ok {
		t.Fatalf("Merge dropped %s from the live file; it preserves every key it does not recognize", Key)
	}
	if string(got) != `{"refresh_token":"RT-LIVE"}` {
		t.Fatalf("Merge rewrote the live %s record: %s", Key, got)
	}
}
