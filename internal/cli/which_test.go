package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An unmanaged login is a negative probe answer, not a failure: exit 5, with
// the explanation on stderr and nothing on stdout.
func TestWhichUnattributedIsExitFive(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, errOut, top := runRoot(t, "which")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d", code, ExitProbeNegative)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: the notice belongs on stderr", out)
	}
	if !strings.Contains(errOut, "not one ccdad manages") {
		t.Fatalf("stderr = %q, want the explanation", errOut)
	}
	if top != "" {
		t.Fatalf("ExecuteWith also printed %q; a silent error must not be re-reported", top)
	}
}

// --json changes the representation, never the answer: a supervisor doing
// `ccdad which --json >/dev/null || restart` must get the same verdict.
func TestWhichJSONKeepsTheSameExitCode(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "which", "--json")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d — the same as without --json", code, ExitProbeNegative)
	}
	if top != "" {
		t.Fatalf("ExecuteWith printed %q on a machine-readable run", top)
	}
	var payload struct {
		SchemaVersion int  `json:"schemaVersion"`
		Attributed    bool `json:"attributed"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if payload.Attributed {
		t.Fatal("attributed = true, want false")
	}
	if payload.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", payload.SchemaVersion)
	}
}

// A token account can never be the live credentials-file login, so attributing
// it means looking where Claude Code actually reads it from. The bundle is
// explicit that CLAUDE_CODE_OAUTH_TOKEN takes precedence over the stored
// claudeAiOauth, and a headless machine is where `which` matters most.
func TestWhichAttributesTheEnvironmentSetupToken(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "headless@example.com"))
	})
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-LIVE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-LIVE")

	code, out, _, top := runRoot(t, "which")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if !strings.Contains(out, "headless@example.com") {
		t.Fatalf("stdout = %q, want the token account named", out)
	}
}

// An environment token ccdad does not manage is still unattributed, and must
// not fall back to matching the credentials file: the file is not what Claude
// Code is using.
func TestWhichDoesNotFallBackWhenTheEnvTokenIsUnmanaged(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-SOMEONE-ELSES")

	code, _, errOut, _ := runRoot(t, "which")
	if code != ExitProbeNegative {
		t.Fatalf("exit = %d, want %d: the env token is what Claude Code uses, and it is not managed", code, ExitProbeNegative)
	}
	if !strings.Contains(errOut, "not one ccdad manages") {
		t.Fatalf("stderr = %q", errOut)
	}
}
