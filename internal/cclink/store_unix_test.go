//go:build !windows

package cclink

import (
	"encoding/json"
	"os"
	"syscall"
	"testing"
)

// Claude Code's storage-V5 change probe watches dev:ino:size:mtimeNs, so a
// running session detects a swap by inode change even at an identical
// mtime. WriteFileAtomic's sibling-temp-file-then-rename is what produces a
// new inode; an in-place write would be invisible to that probe. This is
// also the test that catches "the atomic write replaced with os.WriteFile":
// the credentials file already exists with mode 0600 before Activate runs,
// so an in-place os.WriteFile would leave both content and mode looking
// correct while silently keeping the old inode.
func TestActivateWritesViaRenameSoTheInodeChanges(t *testing.T) {
	withClaudeHome(t)
	path := writeCreds(t, `{"claudeAiOauth":{"accessToken":"old"}}`)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeIno := before.Sys().(*syscall.Stat_t).Ino

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(`{"accessToken":"new"}`)}); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterIno := after.Sys().(*syscall.Stat_t).Ino
	if afterIno == beforeIno {
		t.Fatalf("inode did not change across Activate (stayed %d); an in-place write is invisible to Claude Code's storage-V5 change probe", beforeIno)
	}
}
