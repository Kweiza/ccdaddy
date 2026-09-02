package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The export carries the provider on every row, with no omitempty.
//
// Written unconditionally for the reason accountJSON writes it
// unconditionally: an absent key would have to mean "this document was written
// before ccdad knew about providers", and a Claude row would then be
// indistinguishable from an unknown one. The importer's derivation exists for
// documents that really do predate the field, and it must not have to guess
// about documents that do not.
func TestExportCarriesEveryRowsProvider(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedCodexAccount(t, "u-x", "c@example.com")

	_, out, _, top := runRoot(t, "export")
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Accounts      []struct {
			UUID     string `json:"uuid"`
			Provider string `json:"provider"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("export is not valid JSON: %v (%s)\n%s", err, top, out)
	}
	// The bump is what makes an older ccdad print its existing "newer ccdad"
	// note instead of importing a document whose rows it cannot classify.
	if payload.SchemaVersion != 2 {
		t.Fatalf("schemaVersion = %d, want 2", payload.SchemaVersion)
	}
	want := map[string]string{"u-1": "claude", "u-x": "codex"}
	if len(payload.Accounts) != 2 {
		t.Fatalf("export carries %d accounts, want 2:\n%s", len(payload.Accounts), out)
	}
	for _, a := range payload.Accounts {
		if a.Provider != want[a.UUID] {
			t.Errorf("%s: provider = %q, want %q", a.UUID, a.Provider, want[a.UUID])
		}
	}
}

// TestEveryExportedAccountCarriesTheProviderKey pins that "provider" is a KEY
// present on every row, not merely a field that happens to decode correctly.
//
// The two tests above decode the document into a Go struct with a `string`
// field for Provider, and a `string` cannot tell "the key is absent" from
// "the key is present and holds the empty string" — so neither test would
// notice if the key stopped being written at all. That distinction matters
// here specifically: the importer recognizes a document written before ccdad
// knew about providers by the ABSENCE of the key on a row, and treats such a
// row as Claude.
//
// The hazard is the key going missing for any reason, a dropped assignment in
// exportAccount being the likely one — NOT `omitempty`: a stored account's
// provider is always "claude" or "codex", so the field is never the empty
// string omitempty elides, and adding the tag back would change nothing a
// real export produces. If the key stops being written some other way, a
// freshly-exported Claude row becomes indistinguishable from a document that
// predates the field, and the importer's derivation starts guessing instead
// of reading a fact — which is why presence, not merely correctness, is what
// a later import needs. This test decodes into the untyped JSON shape
// instead, so it can ask the one question that matters: is "provider" a key
// of this object at all.
func TestEveryExportedAccountCarriesTheProviderKey(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedCodexAccount(t, "u-x", "c@example.com")

	_, out, _, top := runRoot(t, "export")
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("export is not valid JSON: %v (%s)\n%s", err, top, out)
	}
	accounts, ok := payload["accounts"].([]any)
	if !ok {
		t.Fatalf("export has no \"accounts\" array:\n%s", out)
	}
	if len(accounts) != 2 {
		t.Fatalf("export carries %d accounts, want 2:\n%s", len(accounts), out)
	}
	for _, raw := range accounts {
		account, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("an account row is not a JSON object: %#v", raw)
		}
		uuid, _ := account["uuid"].(string)
		if _, present := account["provider"]; !present {
			t.Errorf("account %s has no \"provider\" key at all:\n%s", uuid, out)
		}
	}
}

// The credential snapshot of a Codex account travels in a --full export, or a
// backup of a machine with Codex accounts on it restores nothing usable.
func TestAFullExportCarriesTheCodexCredential(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "u-x", "c@example.com")

	path := filepath.Join(t.TempDir(), "export.json")
	if code, _, _, top := runRoot(t, "export", "--full", "--out", path); code != ExitOK {
		t.Fatalf("export --full = %d (%s)", code, top)
	}
	raw := readFileForTest(t, path)
	var payload struct {
		Accounts []struct {
			UUID        string                     `json:"uuid"`
			Provider    string                     `json:"provider"`
			Credentials map[string]json.RawMessage `json:"credentials"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("export is not valid JSON: %v\n%s", err, raw)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("export carries %d accounts, want 1", len(payload.Accounts))
	}
	if _, ok := payload.Accounts[0].Credentials["codexOAuth"]; !ok {
		t.Fatalf("the Codex account is exported with no credential:\n%s", raw)
	}
}
