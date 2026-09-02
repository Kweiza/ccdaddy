package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// writeAccountsDocument replaces accounts.toml with a hand-written fixture,
// after Open has made the directory. Every version rule below is about a
// document ccdad did not write in this process.
func writeAccountsDocument(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, accountsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A version-1 document has no provider key anywhere, because every account in
// it predates Codex support. Every row reads as Claude, which is what it is.
func TestAVersionOneDocumentReadsEveryRowAsClaude(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	writeAccountsDocument(t, root, "version = 1\n\n[[accounts]]\nuuid = \"u-1\"\n"+
		"email = \"a@example.com\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")

	s, err := Open()
	if err != nil {
		t.Fatalf("Open() = %v, want it to read a version-1 document", err)
	}
	if got := s.Accounts()[0].Provider; got != provider.Claude {
		t.Fatalf("Provider = %q, want claude for a row from before the field existed", got)
	}
}

// The pre-v2 rewrite. A build that does not know the field reads a version-2
// document, drops the key it cannot name, and writes the whole thing back --
// leaving a version-2 header over rows with no provider. That is the state
// this error exists for: the rows may be Codex accounts, and reading them as
// Claude would hand a Codex login to the Claude switch path.
func TestAVersionTwoRowWithoutAProviderIsRefused(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	writeAccountsDocument(t, root, "version = 2\n\n[[accounts]]\nuuid = \"u-1\"\n"+
		"email = \"a@example.com\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")

	_, err := Open()
	if !errors.Is(err, ErrProviderMissing) {
		t.Fatalf("Open() = %v, want ErrProviderMissing", err)
	}
	if !strings.Contains(err.Error(), "u-1") {
		t.Errorf("Open() error = %q, want it to name the row it could not read", err)
	}
}

// A provider this build does not know is the same refusal and for the same
// reason: guessing would put an account of unknown provenance in the rotation.
func TestARowWithAnUnknownProviderIsRefused(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	writeAccountsDocument(t, root, "version = 2\n\n[[accounts]]\nuuid = \"u-1\"\n"+
		"provider = \"gemini\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")

	if _, err := Open(); !errors.Is(err, ErrProviderMissing) {
		t.Fatalf("Open() = %v, want ErrProviderMissing for an unknown provider", err)
	}
}

// A version-1 document carrying an explicit provider is a downgrade artefact:
// a build that writes the key wrote version 1 because no row was Codex, and an
// older build then read it. The key is still honoured.
func TestAVersionOneDocumentStillHonoursAnExplicitProvider(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	writeAccountsDocument(t, root, "version = 1\n\n[[accounts]]\nuuid = \"u-1\"\n"+
		"provider = \"claude\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Accounts()[0].Provider; got != provider.Claude {
		t.Fatalf("Provider = %q, want claude", got)
	}
}

func TestAVersionTwoDocumentReadsBothProviders(t *testing.T) {
	root := withStore(t)
	if _, err := Open(); err != nil {
		t.Fatal(err)
	}
	writeAccountsDocument(t, root, "version = 2\n\n[[accounts]]\nuuid = \"u-1\"\n"+
		"provider = \"claude\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n\n"+
		"[[accounts]]\nuuid = \"u-2\"\nprovider = \"codex\"\nkind = \"subscription\"\nidx = 2\n"+
		"added_at = 2026-08-21T00:00:00Z\n")

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got := s.Accounts()
	if len(got) != 2 {
		t.Fatalf("Accounts() = %d rows, want 2", len(got))
	}
	if got[0].Provider != provider.Claude || got[1].Provider != provider.Codex {
		t.Fatalf("providers = %q, %q; want claude, codex", got[0].Provider, got[1].Provider)
	}
}

// CheckVersionAt is what the daemon runs at start and what `ccdad uninstall`
// must be able to survive. It creates nothing: a store that is not there is
// not a store with a bad version.
func TestCheckVersionAt(t *testing.T) {
	t.Run("a store that is not there", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "never-created")
		if err := CheckVersionAt(missing); err != nil {
			t.Fatalf("CheckVersionAt(missing) = %v, want nil", err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("CheckVersionAt created the directory it was asked about")
		}
	})
	t.Run("a readable store", func(t *testing.T) {
		root := withStore(t)
		if _, err := Open(); err != nil {
			t.Fatal(err)
		}
		writeAccountsDocument(t, root, "version = 2\n\n[[accounts]]\nuuid = \"u-1\"\n"+
			"provider = \"codex\"\nkind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")
		if err := CheckVersionAt(root); err != nil {
			t.Fatalf("CheckVersionAt() = %v, want nil", err)
		}
	})
	t.Run("a rewritten store", func(t *testing.T) {
		root := withStore(t)
		if _, err := Open(); err != nil {
			t.Fatal(err)
		}
		writeAccountsDocument(t, root, "version = 2\n\n[[accounts]]\nuuid = \"u-1\"\n"+
			"kind = \"subscription\"\nidx = 1\nadded_at = 2026-08-21T00:00:00Z\n")
		if err := CheckVersionAt(root); !errors.Is(err, ErrProviderMissing) {
			t.Fatalf("CheckVersionAt() = %v, want ErrProviderMissing", err)
		}
	})
}

// An account needs a relogin exactly while the mark names the token it still
// holds. A later `ccdad codex add` stores a new token and the mark stops
// matching, so nothing has to clear it and no stale mark can survive a
// re-login.
func TestNeedsRelogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mark    string
		current string
		want    bool
	}{
		{"no mark", "", "abc123", false},
		{"mark names the token it still holds", "abc123", "abc123", true},
		{"a re-login moved the token past the mark", "abc123", "def456", false},
		{"a mark with no token to compare", "abc123", "", false},
		{"neither", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Account{Provider: provider.Codex, CodexReloginFor: tc.mark}
			if got := a.NeedsRelogin(tc.current); got != tc.want {
				t.Errorf("NeedsRelogin(%q) with mark %q = %v, want %v", tc.current, tc.mark, got, tc.want)
			}
		})
	}
}

// The field is written with NO omitempty, so every row carries it. A row
// written without it is exactly the pre-v2 rewrite this file refuses, and a
// document that omitted the key for Claude rows would be indistinguishable
// from one.
func TestEveryRowIsWrittenWithItsProviderKey(t *testing.T) {
	root := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-1", Provider: provider.Claude}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, accountsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "provider = 'claude'") {
		t.Fatalf("accounts.toml does not carry the provider key:\n%s", raw)
	}
}
