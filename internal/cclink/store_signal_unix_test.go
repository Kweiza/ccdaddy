//go:build !windows

package cclink

import (
	"encoding/json"
	"strings"
	"testing"
)

// A write ended by a SIGNAL after it landed is not a failure `security`
// reported: the process did not exit, so it gave no answer, and the request had
// reached the store. It is read back like a timeout. Unix only, because a
// process killing itself on Windows exits with a status rather than a signal.
func TestAWriteEndedByASignalIsReadBack(t *testing.T) {
	item, file := slowKeychain(t, fakeSecurity{writeSignaled: true})

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v, want nil: the item holds the new login", err)
	}
	if got := itemNow(t, item); !strings.Contains(got, "RT-new") {
		t.Fatalf("the item does not hold the new login: %s", got)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("a write ended by a signal was not read back, and the file was left behind:\n%s", got)
	}
}
