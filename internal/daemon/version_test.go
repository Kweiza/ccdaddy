package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// The version rule is decided at START, once, and the daemon refuses to run
// rather than refusing every tick.
//
// The document is the one a ccdad that predates Codex support leaves behind: it
// read a version-2 file, dropped the key it could not name, and wrote the whole
// thing back -- a version-2 header over rows with no provider. That is a fact
// about the machine, and re-deciding it on a cadence would turn one refusal
// into a log line per tick.
//
// The context carries a deadline because Run BLOCKS: without the check it
// reaches the loop and stays there until the context ends, so a background
// context would hang this test rather than fail it.
func TestRunRefusesAStoreWhoseRowsHaveNoProvider(t *testing.T) {
	root := isolate(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rewritten := "version = 2\n\n[[accounts]]\nuuid = \"u-x\"\nkind = \"subscription\"\n" +
		"idx = 1\nadded_at = 2026-09-02T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := Run(ctx, Options{})
	if !errors.Is(err, store.ErrProviderMissing) {
		t.Fatalf("Run() = %v, want ErrProviderMissing", err)
	}

	// The singleton has to be back. A daemon that kept it after refusing would
	// make `ccdad daemon status` report a daemon that is not there.
	held, herr := SingletonHeld()
	if herr != nil {
		t.Fatal(herr)
	}
	if held {
		t.Error("Run() kept the singleton after refusing to start")
	}

	// The version check runs BEFORE the "up" log line, not after it: a start
	// this build refuses must never log "ccdad daemon up, pid N" and then
	// "not starting" on the very next line, which is what a reader followed
	// into daemon.log to find the real reason a supervisor's ten-second wait
	// had already timed out over.
	raw, lerr := os.ReadFile(filepath.Join(root, LogFileName))
	if lerr != nil {
		t.Fatal(lerr)
	}
	if strings.Contains(string(raw), "up, pid") {
		t.Errorf("daemon.log logged \"up\" before refusing to start:\n%s", raw)
	}
	if !strings.Contains(string(raw), "not starting") {
		t.Errorf("daemon.log does not say why the daemon did not start:\n%s", raw)
	}
}

// The check at start is narrowed to ErrProviderMissing specifically, not to
// "the store failed to load" in general. CheckVersionAt is s.load(), which
// also returns "reading accounts.toml: …" for a document this process cannot
// open and "parsing accounts.toml: …" for one that is not valid TOML — and
// those describe a damaged machine, not an unsupported one. A Claude-only
// install with a truncated or briefly-unreadable document starts today, fails
// its ticks, and reaches the wedge/restart machinery; widening this check back
// to "any error" would make it exit at start instead, with no status
// published, which is the Claude-path behaviour change this narrowing exists
// to prevent.
func TestRunStartsOnAStoreItCannotParse(t *testing.T) {
	root := isolate(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := "version = 2\n\n[[accounts]\nthis is not valid toml {{{\n"
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Run(ctx, Options{}); err != nil {
		t.Fatalf("Run() = %v, want nil — a damaged document must not refuse startup", err)
	}
}

// See TestRunStartsOnAStoreItCannotParse: the same narrowing, for the other
// error load() can return — a document this process cannot open at all.
func TestRunStartsOnAStoreItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 only sets the read-only attribute on Windows and does not block reads, so the unreadable-document branch cannot be reached here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not deny root")
	}
	root := isolate(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "accounts.toml")
	if err := os.WriteFile(p, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Run(ctx, Options{}); err != nil {
		t.Fatalf("Run() = %v, want nil — an unreadable document must not refuse startup", err)
	}
}
