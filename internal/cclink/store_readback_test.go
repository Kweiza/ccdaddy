package cclink

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The read-back has the WRITE bound, not the lookup bound: it asks the same
// slow keychain that just failed to answer a write in time. Every spawn here
// takes 1.5 s, a lookup is allowed 0.3 s and the write path 6 s, so a read-back
// held to the lookup bound cannot confirm the write that landed.
func TestTheReadBackIsHeldToTheWriteBound(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecurity{item: statefulItem(t, oldLogin), hang: 1500 * time.Millisecond, writeHang: time.Minute}.install(t)
	keychainReadTimeout = 300 * time.Millisecond
	keychainWriteTimeout = 6 * time.Second

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v: the read-back did not get the write bound", err)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the file was not written after a confirmed write:\n%s", got)
	}
}
