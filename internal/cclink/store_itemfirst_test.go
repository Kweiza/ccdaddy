package cclink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// itemHolding is what the fixture hands back for a `-w` read: the compact JSON
// Claude Code itself writes into the item.
func itemHolding(t *testing.T, blob string) string {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &probe); err != nil {
		t.Fatalf("the fixture payload is not JSON: %v", err)
	}
	return blob
}

// THE ITEM FIRST, THE FILE SECOND. The two writes cannot be atomic, but the
// order was never forced. Writing the file first left the losing state this
// whole path exists to stop producing: the file moved and the login -- the item
// Claude Code reads before it -- did not.
func TestASwitchThatCannotWriteTheItemMovesNothing(t *testing.T) {
	withClaudeHome(t)
	before := `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"}}`
	path := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecurity{
		stdout:   itemHolding(t, before),
		failOp:   "add-generic-password",
		failExit: 45,
	}.install(t)

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT-new","refreshToken":"RT-new"}`)})
	if err == nil {
		t.Fatal("Activate succeeded although the keychain item could not be written")
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("the error does not say the login did not change: %v", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(after), "RT-new") {
		t.Fatalf("the credentials file was moved although the login was not:\n%s", after)
	}
}

// A machine-scoped key that only the FILE holds must survive a switch. Claude
// Code's combinator takes the plaintext fallback whenever a keychain write
// fails, so a file can hold an MCP login the item has never seen -- and the
// item is the merge base.
func TestASwitchKeepsMachineKeysOnlyTheFileHolds(t *testing.T) {
	withClaudeHome(t)
	file := `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"},` +
		`"mcpOAuth":{"server-from-the-file":{"token":"m1"}}}`
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	item := `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"},` +
		`"mcpOAuth":{"server-from-the-item":{"token":"m2"}}}`
	fakeSecurity{stdout: itemHolding(t, item)}.install(t)

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT-new","refreshToken":"RT-new"}`)}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	after, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server-from-the-file", "server-from-the-item", "RT-new"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("%q did not survive the switch:\n%s", want, after)
		}
	}
}

// A file that cannot be READ, behind an item that answered, is an error and not
// an empty base: the write replaces that file wholesale, so continuing would
// destroy machine keys at the moment ccdad cannot see what it is destroying.
func TestASwitchRefusesWhenTheFileBehindAnItemCannotBeRead(t *testing.T) {
	withClaudeHome(t)
	path := mustPath(ccpath.CredentialsPath())
	_ = os.Remove(path)
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	item := `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"}}`
	fakeSecurity{stdout: itemHolding(t, item)}.install(t)

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"AT-new","refreshToken":"RT-new"}`)})
	if err == nil {
		t.Fatal("Activate succeeded over a credentials file it could not read")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("the error does not name the unreadable file: %v", err)
	}
}
