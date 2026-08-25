package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDownloadStreamsAndDigests(t *testing.T) {
	body := strings.Repeat("ccdad", 4096)
	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	dest := filepath.Join(t.TempDir(), "asset")

	sum, n, err := c.Download(context.Background(), base+"/a", dest, 1<<20)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("Download() n = %d, want %d", n, len(body))
	}
	want := sha256.Sum256([]byte(body))
	if sum != hex.EncodeToString(want[:]) {
		t.Errorf("Download() sum = %q, want %q", sum, hex.EncodeToString(want[:]))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Error("the file on disk is not the body that was served")
	}
}

// The download step owns the mode, and it is set AFTER the close rather than
// as a create mode: a create mode is masked by umask, and install.sh's bare
// chmod 0755 defeats umask deliberately.
func TestDownloadMakesTheFileExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows, where execution is decided by the extension and the ACL")
	}
	old := syscallUmask(0o077)
	t.Cleanup(func() { syscallUmask(old) })

	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	})
	dest := filepath.Join(t.TempDir(), "asset")
	if _, _, err := c.Download(context.Background(), base+"/a", dest, 1<<20); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want -rwxr-xr-x — a umask of 077 must not decide this", info.Mode().Perm())
	}
}

// An over-long body must not survive on disk as a plausible binary.
func TestDownloadRefusesAnOverLongBodyAndLeavesNoFile(t *testing.T) {
	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	})
	dest := filepath.Join(t.TempDir(), "asset")
	if _, _, err := c.Download(context.Background(), base+"/a", dest, 4999); err == nil {
		t.Fatal("Download() accepted a body larger than its limit")
	}
	// errors.Is against fs.ErrNotExist, never a match on the message: "no such
	// file or directory" is ENOENT's Unix text and Windows says something else
	// entirely.
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused download left %s behind (stat said %v)", dest, err)
	}
}

func TestDownloadReportsAStatus(t *testing.T) {
	c, base := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	dest := filepath.Join(t.TempDir(), "asset")
	_, _, err := c.Download(context.Background(), base+"/a", dest, 1<<20)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("Download() error = %v, want a 404 StatusError", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a 404 left %s behind (stat said %v)", dest, err)
	}
}
