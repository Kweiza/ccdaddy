package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// stubStdoutTTY describes a stdout the test is not attached to. --full's
// refusal is the only thing standing between a payload of live refresh tokens
// and a scrollback buffer, so the refusal has to be exercised.
func stubStdoutTTY(t *testing.T, tty bool) {
	t.Helper()
	saved := stdoutIsTTY
	t.Cleanup(func() { stdoutIsTTY = saved })
	stdoutIsTTY = func() bool { return tty }
}

func decodeExport(t *testing.T, raw string) exportPayload {
	t.Helper()
	var payload exportPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the export is not valid JSON (%v):\n%s", err, raw)
	}
	return payload
}

// The default export is the one people mail to themselves. It must carry no
// token of any kind — not a refresh token, not an access token, not an API key.
func TestExportDefaultCarriesNoCredentials(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")

	code, stdout, _, top := runRoot(t, "export")
	if code != ExitOK {
		t.Fatalf("export = %d (%s), want 0", code, top)
	}
	if strings.Contains(stdout, "RT-u-1") || strings.Contains(stdout, "credentials") {
		t.Fatalf("the default export carries credentials:\n%s", stdout)
	}

	payload := decodeExport(t, stdout)
	if payload.Full {
		t.Error(`the default export is marked "full"`)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].UUID != "u-1" {
		t.Fatalf("accounts = %+v, want the one seeded account", payload.Accounts)
	}
	if payload.Accounts[0].Credentials != nil {
		t.Error("an account in the default export carries a credential snapshot")
	}
}

// idx recompacts on every removal, so an export carrying it would reproduce a
// stale ordinal on the machine it is imported into.
func TestExportOmitsIdx(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	_, stdout, _, _ := runRoot(t, "export")
	if strings.Contains(stdout, `"idx"`) {
		t.Fatalf("the export carries idx:\n%s", stdout)
	}
}

// The unknown-key drift probe belongs in the artifact, not only on stderr —
// the artifact is what outlives the terminal it was taken in.
func TestExportSurfacesUnknownCredentialKeys(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	addLiveKey(t, "somethingNew", `{"a":1}`)

	_, stdout, _, _ := runRoot(t, "export")
	payload := decodeExport(t, stdout)
	if len(payload.UnknownKeys) != 1 || payload.UnknownKeys[0] != "somethingNew" {
		t.Fatalf("unknownKeys = %v, want [somethingNew]", payload.UnknownKeys)
	}
}

// Three conditions ride together on --include-mcp: it needs --full, it warns
// loudly on stderr, and it is the only path by which mcpOAuth leaves the
// machine. The flag on its own is a usage error rather than a silent upgrade
// to --full: the difference between the two payloads is every MCP client
// secret on the machine.
func TestIncludeMCPRequiresFull(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")

	code, stdout, _, top := runRoot(t, "export", "--include-mcp")
	if code != ExitUsage {
		t.Fatalf("export --include-mcp = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "--full") {
		t.Errorf("the refusal %q does not name --full", top)
	}
	if strings.Contains(stdout, "schemaVersion") {
		t.Error("a payload was written despite the refusal")
	}
}

func TestIncludeMCPWarnsLoudlyAndCarriesBothHalves(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	addLiveKey(t, "mcpOAuth", `{"server":{"accessToken":"MCP-AT"}}`)
	addLiveKey(t, "mcpOAuthClientConfig", `{"server":{"clientSecret":"MCP-SECRET"}}`)

	code, stdout, stderr, top := runRoot(t, "export", "--full", "--include-mcp")
	if code != ExitOK {
		t.Fatalf("export --full --include-mcp = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("stderr = %q, want a loud warning", stderr)
	}

	payload := decodeExport(t, stdout)
	if payload.Machine == nil {
		t.Fatal("the payload carries no machine block")
	}
	// mcpOAuthClientConfig is the client-secret half. Carrying only the token
	// half leaves MCP logins that cannot refresh.
	if !strings.Contains(string(payload.Machine.MCPOAuth), "MCP-AT") {
		t.Error("mcpOAuth is missing from the machine block")
	}
	if !strings.Contains(string(payload.Machine.MCPOAuthClientConfig), "MCP-SECRET") {
		t.Error("mcpOAuthClientConfig is missing; the exported MCP logins could not refresh")
	}
}

func TestExportWithoutIncludeMCPCarriesNoMachineKeys(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	addLiveKey(t, "mcpOAuth", `{"server":{"accessToken":"MCP-AT"}}`)

	_, stdout, _, _ := runRoot(t, "export", "--full")
	if strings.Contains(stdout, "MCP-AT") {
		t.Fatalf("a --full export leaked the MCP login without --include-mcp:\n%s", stdout)
	}
}

// A payload of live refresh tokens printed at a terminal is one screen share
// and one `> backup.json` at the shell's umask from being world-readable.
func TestFullExportToATerminalIsRefused(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	stubStdoutTTY(t, true)

	code, stdout, _, top := runRoot(t, "export", "--full")
	if code != ExitUsage {
		t.Fatalf("export --full to a terminal = %d, want %d", code, ExitUsage)
	}
	if strings.Contains(stdout, "RT-u-1") {
		t.Fatal("the refusal printed the tokens anyway")
	}
	if !strings.Contains(top, "--out") {
		t.Errorf("the refusal %q does not say how to write the file", top)
	}

	// The default export holds nothing secret, so a terminal is fine for it.
	if code, _, _, _ := runRoot(t, "export"); code != ExitOK {
		t.Errorf("a credential-free export to a terminal = %d, want 0", code)
	}
}

func TestExportOutWritesMode0600(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, true)
	seedAccount(t, "u-1", "one@example.com")

	path := filepath.Join(t.TempDir(), "backup.json")
	code, _, stderr, top := runRoot(t, "export", "--full", "--out", path)
	if code != ExitOK {
		t.Fatalf("export --out = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("stderr = %q, want it to name the file it wrote", stderr)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %04o, want 0600 — a shell redirect at the umask is what --out exists to avoid", path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeExport(t, string(raw))
	if !payload.Full || payload.Accounts[0].Credentials == nil {
		t.Fatal("a --full export to a file carries no credentials")
	}
}

// primary decides whether an account is ranked beside the subscriptions and
// whether the credit ceiling gates it, so an export that dropped it would
// restore a machine that behaves differently from the one it was taken from —
// silently, and only on the day the main pool runs out.
func TestExportCarriesThePrimaryFlag(t *testing.T) {
	isolate(t)
	seedPrimaryCreditAccount(t, "u-1", "seat@example.com")
	seedAccount(t, "u-2", "plain@example.com")

	path := exportTo(t, "backup.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range decodeExport(t, string(raw)).Accounts {
		if row.UUID == "u-1" && !row.Primary {
			t.Error("the export does not carry the primary flag")
		}
		// Omitted rather than false for an ordinary account, which is the shape
		// `disabled` already has in this payload.
		if row.UUID == "u-2" && row.Primary {
			t.Error("the export marked an ordinary account primary")
		}
	}
}

// --base64 is the form that survives a GitHub secret, a `.env` line, and every
// other transport that carries one string and no newlines. The invariant that
// makes it safe to reason about: what comes back out of the decoder is the
// document the plain export would have written, to the byte.
//
// exportedAt is the one field that cannot match, because the two runs happen at
// two times. It is dropped from both sides rather than stubbed: a clock seam
// added for one assertion is a seam every later change has to keep honest.
func TestExportBase64DecodesToThePlainDocument(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	seedAccount(t, "u-1", "one@example.com")
	seedAccount(t, "u-2", "two@example.com")

	_, plain, _, _ := runRoot(t, "export")
	code, encoded, _, top := runRoot(t, "export", "--base64")
	if code != ExitOK {
		t.Fatalf("export --base64 = %d (%s), want 0", code, top)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("the --base64 output is not standard base64 (%v):\n%s", err, encoded)
	}
	if got, want := withoutExportedAt(t, string(decoded)), withoutExportedAt(t, plain); got != want {
		t.Fatalf("decoding --base64 does not reproduce the plain export:\n got %s\nwant %s", got, want)
	}
	// The framing travels too, so `ccdad export --base64 | base64 -d > f.json`
	// and `ccdad export --out f.json` produce the same file.
	if !strings.HasSuffix(string(decoded), "\n") {
		t.Error("the decoded document does not end in a newline, but the plain one does")
	}
}

// exportedAtField matches the one field two runs of `export` cannot agree on.
var exportedAtField = regexp.MustCompile(`"exportedAt": "[^"]*"`)

// withoutExportedAt blanks that field TEXTUALLY, leaving every other byte where
// it was: indentation, key order and the trailing newline all survive, which is
// what makes the caller's comparison the byte-for-byte one it claims to be.
// Round-tripping through map[string]any instead would compare JSON values and
// silently stop defending the framing.
func withoutExportedAt(t *testing.T, document string) string {
	t.Helper()
	blanked := exportedAtField.ReplaceAllString(document, `"exportedAt": ""`)
	if blanked == document {
		t.Fatalf("nothing matched exportedAt, so this comparison is not doing what it claims:\n%s", document)
	}
	return blanked
}

// `base64` wraps at 76 columns unless it is given -w0, and a wrapped blob
// breaks the `.env` line it was pasted into — at the far end of a deployment,
// where nobody reruns it by hand. ccdad never wraps.
func TestExportBase64IsOneUnwrappedLine(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, false)
	for i := 0; i < 12; i++ {
		seedAccount(t, fmt.Sprintf("u-%d", i), fmt.Sprintf("%d@example.com", i))
	}

	_, stdout, _, _ := runRoot(t, "export", "--base64")
	if len(stdout) < 400 {
		t.Fatalf("the payload is too short to say anything about wrapping:\n%s", stdout)
	}
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("--base64 wrote %d newline(s), want exactly one and at the end",
			strings.Count(stdout, "\n"))
	}
}

// The refusal has to survive --base64, and it has to say why. base64 is
// exactly what somebody reaches for when they want to print a secret and feel
// safe about it, so a guard that let the flag through would be worse than no
// guard: it would look like the answer to the warning it silenced.
func TestFullBase64ExportToATerminalIsRefused(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "one@example.com")
	stubStdoutTTY(t, true)

	code, stdout, _, top := runRoot(t, "export", "--full", "--base64")
	if code != ExitUsage {
		t.Fatalf("export --full --base64 to a terminal = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("the refusal wrote a payload anyway:\n%s", stdout)
	}
	if !strings.Contains(top, "not encryption") {
		t.Errorf("the refusal %q does not say that base64 is not encryption", top)
	}
	if !strings.Contains(top, "--out") {
		t.Errorf("the refusal %q does not say how to write the file", top)
	}
}

func TestExportBase64OutWritesOneLineAt0600(t *testing.T) {
	isolate(t)
	stubStdoutTTY(t, true)
	seedAccount(t, "u-1", "one@example.com")

	path := filepath.Join(t.TempDir(), "backup.b64")
	code, _, _, top := runRoot(t, "export", "--full", "--base64", "--out", path)
	if code != ExitOK {
		t.Fatalf("export --full --base64 --out = %d (%s), want 0", code, top)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %04o, want 0600 — base64 is not encryption, so the mode still carries the secret",
			path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 || !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("the file holds %d newline(s), want exactly one and at the end",
			strings.Count(string(raw), "\n"))
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("the file is not base64: %v", err)
	}
	payload := decodeExport(t, string(decoded))
	if !payload.Full || payload.Accounts[0].Credentials == nil {
		t.Fatal("a --full --base64 export carries no credentials")
	}
}
