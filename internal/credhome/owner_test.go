package credhome

import (
	"os"
	"path/filepath"
	"testing"
)

// The owner document's states are the whole of "held by somebody I cannot
// name", and the split between them decides what a caller does: a state that is
// merely NOT WRITTEN YET is transient and silent, while a document that exists
// and does not make sense is something `ccdad doctor` has to print.
//
// The two are deliberately not the same answer, and every row here pins which
// side it falls on. Folding them together — the obvious simplification — makes
// the transient window print an error on every probe, or makes real corruption
// invisible forever.
func TestOwnerDocumentStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		absent  bool
		named   bool
		wantErr bool
	}{{
		name:   "absent is nothing to read",
		absent: true,
	}, {
		name: "empty is the truncate half of a write in flight",
		body: "",
	}, {
		name: "no commit marker is the half-written prefix of one",
		body: `{"schemaVersion":1,"store":"/s","pid":7}`,
	}, {
		name:    "committed and unparseable is corruption",
		body:    "not json at all\n",
		wantErr: true,
	}, {
		name:    "schema version zero was written by nothing here",
		body:    `{"store":"/s","pid":7}` + "\n",
		wantErr: true,
	}, {
		// The additive contract, and the row that keeps it honest. A NEWER
		// document must still name its holder: the fields this binary knows
		// mean what they meant, and refusing one would make a ccdad upgrade
		// look like two engines fighting.
		name:  "a newer schema version is still readable",
		body:  `{"schemaVersion":99,"store":"/s","pid":7,"unknownField":true}` + "\n",
		named: true,
	}, {
		name:    "a document naming no store cannot identify anybody",
		body:    `{"schemaVersion":1,"pid":7}` + "\n",
		wantErr: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), OwnerFileName)
			if !tc.absent {
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			o, named, err := readOwner(path)
			if named != tc.named {
				t.Errorf("readOwner named = %v, want %v", named, tc.named)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("readOwner err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.named && o.Store != "/s" {
				t.Errorf("readOwner store = %q, want %q", o.Store, "/s")
			}
			// Nothing here may be BOTH named and an error: a caller that used
			// the owner while an error was set would be reading a document the
			// reader had already rejected.
			if named && err != nil {
				t.Errorf("readOwner returned a name and an error together (%v)", err)
			}
		})
	}
}

// A read must not create the file it reads. readOwner is reached from the probe
// path, which doctor and the auto-start hook both run.
func TestReadOwnerCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), OwnerFileName)
	if _, _, err := readOwner(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("readOwner created %s", path)
	}
}

// clearOwner empties; it never unlinks. An absent document beside a lock file
// that exists is a state nothing else produces, and forging it tells the next
// reader something untrue.
func TestClearOwnerEmptiesWithoutRemoving(t *testing.T) {
	path := filepath.Join(t.TempDir(), OwnerFileName)
	if err := writeOwner(path, Owner{SchemaVersion: OwnerSchemaVersion, Store: "/s", PID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := clearOwner(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("clearOwner removed the document: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("clearOwner left %d bytes, want 0", info.Size())
	}
	// A missing document is not an error to clear: uninstall runs this on a
	// machine where a claim was never written.
	if err := clearOwner(filepath.Join(t.TempDir(), "never-written")); err != nil {
		t.Errorf("clearOwner on an absent document = %v, want nil", err)
	}
}
