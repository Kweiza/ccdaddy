package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The three top-level names the Codex work adds are markers of a ccdad store.
//
// Without them, a machine whose only ccdad state is a shim and a proxy port --
// every account removed, nothing else left -- is refused as "not a ccdad
// store" and the directory is left behind for nobody to clean up. That is the
// same reason the sessions, profiles and MCP-record names are already here.
func TestStoreMarkersNameTheCodexPaths(t *testing.T) {
	got := storeMarkers()
	for _, want := range []string{"bin", "codex", "codex-shim.json"} {
		if !slices.Contains(got, want) {
			t.Errorf("storeMarkers() does not name %q: %v", want, got)
		}
	}
}

// `ccdad uninstall` must work on a store this build cannot parse.
//
// The document it cannot parse is the one this build's own version rules
// refuse -- a version-2 header over rows with no provider, which is what an
// older ccdad leaves behind. Refusing to uninstall then would mean the one
// command that repairs the machine is the one the damage disables.
func TestUninstallSurvivesADocumentItCannotParse(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	seedCodexAccount(t, "u-x", "c@example.com")

	// Exactly what a build that does not know the provider field writes back.
	rewritten := "version = 2\n\n[[accounts]]\nuuid = \"u-x\"\nemail = \"c@example.com\"\n" +
		"kind = \"subscription\"\nidx = 1\nadded_at = 2026-09-02T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := inspectStore(root)
	if err != nil {
		t.Fatalf("inspectStore() = %v, want nil for a store whose document this build cannot read", err)
	}
	if !got.present {
		t.Fatal("inspectStore() reported no store, so uninstall would refuse to remove the directory")
	}
}

// The tolerance inspectStore grants is narrow on purpose: ONLY
// ErrProviderMissing, the one shape a pre-Codex build's rewrite leaves behind.
// A document broken some other way must still refuse, the same way it did
// before this build learned to survive that one specific error. Without a
// test pinning this, the gate can drift from `errors.Is(err,
// store.ErrProviderMissing)` back to a bare `err != nil` -- which reads as
// "delete a store I could not read" instead of "refuse and say why" -- and
// nothing in the suite would notice.
func TestUninstallRefusesAMalformedDocument(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	seedCodexAccount(t, "u-x", "c@example.com")

	// Broken syntax, not the version-2-without-provider shape the tolerant
	// branch exists for.
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("version = 2\n\n[[accounts\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := inspectStore(root)
	if err == nil {
		t.Fatalf("inspectStore() = nil, present=%v, want a parse error for malformed TOML", got.present)
	}
	if got.present {
		t.Errorf("inspectStore() reported present=true on a parse error; uninstall would proceed to delete a store it could not read")
	}
	if !strings.Contains(err.Error(), "parsing accounts.toml") {
		t.Errorf("err = %v, want it to name accounts.toml as what failed to parse", err)
	}
}

// Same narrowing, the other way a document can be unreadable: not broken
// syntax but no permission to read it at all. See
// TestUninstallRefusesAMalformedDocument for why this is pinned.
func TestUninstallRefusesAnUnreadableDocument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	seedCodexAccount(t, "u-x", "c@example.com")

	p := filepath.Join(root, "accounts.toml")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restored before t.TempDir's own cleanup, which cannot remove a file it
	// may not read.
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	got, err := inspectStore(root)
	if err == nil {
		t.Fatalf("inspectStore() = nil, present=%v, want a permission error", got.present)
	}
	if got.present {
		t.Errorf("inspectStore() reported present=true on a permission error; uninstall would proceed to delete a store it could not read")
	}
}

// The sentence enumerate prints for an unreadable store must not be the one a
// genuinely empty store gets: they would otherwise be byte-identical, and a
// user about to run a destructive command could not tell "empty" from "I
// could not read it". Both sentences are pinned here, not just the new one --
// a test that checked only the new text would let the empty-store text drift
// without anyone noticing the two had become the same again.
func TestUninstallSaysWhenTheDocumentCouldNotBeRead(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())

	cases := []struct {
		name string
		s    storeInspection
		want string
	}{
		{
			name: "genuinely empty",
			s:    storeInspection{root: root, present: true},
			want: "This will delete " + root + ", which holds no accounts.\n",
		},
		{
			name: "unreadable",
			s:    storeInspection{root: root, present: true, unreadable: true},
			want: "This will delete " + root + ", which holds accounts this build cannot read.\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			enumerate(&out, c.s, "/bin/ccdad", nil, "", store.Account{}, false)
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("enumerate() = %q, want it to contain %q", out.String(), c.want)
			}
		})
	}
}
