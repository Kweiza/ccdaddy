//go:build windows

package release

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Download's write path is net/http into os.Create into io.Copy --
// CreateFileW and WriteFile and nothing else -- so it never creates a
// Zone.Identifier stream on the file it writes. That is the claim
// internal/cli's update_windows.go rests on when it says replaceBinary needs
// no Mark-of-the-Web handling, but update_windows_test.go cannot prove it:
// the file it stages is always written with os.WriteFile in its own fixture,
// so it never carries a marking to begin with, and no implementation of
// replaceBinary could make that assertion fail for a real reason. This is
// the test that can, because it runs the real download path -- a real HTTP
// response through the real Client.Download -- against an implementation
// that a shell-out (curl.exe, BITS, anything that goes through
// COM/urlmon) WOULD fail: any of those leave a Zone.Identifier behind.
func TestDownloadLeavesNoZoneIdentifier(t *testing.T) {
	dir := t.TempDir()

	// The positive control. If this fails, the test below is checking
	// nothing — and it must fail LOUDLY rather than skip, for the same
	// reason internal/cli/update_windows_test.go's does: GitHub's Windows
	// runners give NTFS for the workspace, so alternate data streams are
	// always available here.
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
		t.Fatalf("this filesystem does not carry alternate data streams (%v), so the assertion below would be vacuous", err)
	}
	if _, err := os.Stat(control + ":Zone.Identifier"); err != nil {
		t.Fatalf("a Zone.Identifier written by this test cannot be read back (%v); the assertion below would be vacuous", err)
	}

	// The negative control: proof the probe can report ABSENCE before the
	// real assertion depends on it. os.Open is used rather than os.Stat
	// because it reaches CreateFileW directly, which is definitively
	// stream-aware -- opening path:Stream opens that stream specifically and
	// reports ERROR_FILE_NOT_FOUND when it is absent, a stronger guarantee
	// than anything documented for the GetFileAttributesEx call behind
	// os.Stat.
	if f, err := os.Open(control + ":No.Such.Stream"); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("the probe cannot tell an absent stream from a present one (%v), so the assertion below would be vacuous", err)
	}

	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the new ccdad"))
	})
	dest := filepath.Join(dir, "asset")
	if _, _, err := c.Download(context.Background(), base+"/a", dest, 1<<20); err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	f, err := os.Open(dest + ":Zone.Identifier")
	if err == nil {
		_ = f.Close()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the downloaded file carries a zone marking (open said %v)", err)
	}
}
