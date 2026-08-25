package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// A command whose entire job is to make a machine correct has to be safe to run
// twice. Claude Code's own `mcp add` errors on a duplicate; this one reports
// that there was nothing to do and leaves exactly one entry.
func TestInstallingTwiceLeavesOneEntryAndSaysThereWasNothingToDo(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)

	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("first install = %d (%s%s), want 0", code, stderr, top)
	}
	code, _, stderr, top := runRoot(t, "mcp", "install")
	if code != ExitNothingToDo {
		t.Fatalf("second install = %d (%s%s), want %d", code, stderr, top, ExitNothingToDo)
	}
	if !strings.Contains(stderr, "already registered") {
		t.Errorf("stderr = %q, want it to say the entry was already there", stderr)
	}
	if got := countCcdadServers(t); got != 1 {
		t.Errorf("%d entries named ccdad, want exactly 1", got)
	}
}

// The file belongs to the user. It is 195 KB on a real machine with dozens of
// project entries, people keep it in dotfile repositories, and a writer that
// re-encoded it from a map would turn a one-key edit into a whole-file diff --
// or lose whatever it did not understand.
func TestInstallingPreservesEveryOtherKeyAndTheirOrder(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	writeGlobalConfigFixture(t,
		`{"numStartups":41,"projects":{"/w":{"mcpServers":{"other":{"type":"stdio","command":"x"}}}},"tipsHistory":{"a":1}}`)

	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}

	raw := readGlobalConfigFixture(t)
	for _, want := range []string{`"numStartups":41`, `"other"`, `"tipsHistory"`} {
		if !strings.Contains(compactJSON(t, raw), want) {
			t.Errorf("%s did not survive the write:\n%s", want, raw)
		}
	}
	if strings.Index(raw, "numStartups") > strings.Index(raw, "mcpServers") {
		t.Error("the existing keys were reordered; the writer must preserve file order")
	}
}

// The endpoint is what Claude Code de-duplicates on. A bare `ccdad` collapses
// with the plugin's identical entry into ONE server; an absolute path does not,
// and two servers means two full tool sets and two credential primitives.
func TestTheEntryNamesTheBareCommandAndNotThisBinarysPath(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}

	entry := userScopeEntry(t)
	if got := entry["command"]; got != "ccdad" {
		t.Errorf("command = %v, want the bare string \"ccdad\"", got)
	}
	if _, ok := entry["env"]; !ok {
		t.Error("env is missing; it is written even when empty, so a user diffing the file sees nothing unusual")
	}
	if got := entry["type"]; got != "stdio" {
		t.Errorf("type = %v, want \"stdio\"", got)
	}
	args, _ := entry["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args = %v, want [\"mcp\"]", entry["args"])
	}
}

// --print-config mutates nothing, and emits the WHOLE-FILE shape rather than
// the bare entry: that shape is what a project config and the --mcp-config flag
// both consume.
func TestPrintConfigWritesTheWrapperAndTouchesNothing(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, false)

	code, stdout, stderr, top := runRoot(t, "mcp", "install", "--print-config")
	if code != ExitOK {
		t.Fatalf("print-config = %d (%s%s)", code, stderr, top)
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not the wrapper shape: %v\n%s", err, stdout)
	}
	if _, ok := doc.MCPServers["ccdad"]; !ok {
		t.Errorf("no ccdad server in the printed config:\n%s", stdout)
	}
	if _, err := os.Stat(mustPath(ccpath.GlobalConfigPath())); !os.IsNotExist(err) {
		t.Error("--print-config wrote the config file")
	}
	// It warns about nothing: the output is a document somebody is about to
	// paste somewhere, and a PATH diagnostic on stderr beside it is noise
	// about a machine this invocation did not touch.
	if stderr != "" {
		t.Errorf("--print-config wrote %q on stderr; it changed nothing to warn about", stderr)
	}

	// It reads nothing either, which is not a detail: an unparseable
	// .claude.json is one of the two situations somebody reaches for this
	// flag in, and a version that loaded the file first would fail exactly
	// there.
	writeGlobalConfigFixture(t, "{ this is not json")
	code, stdout, _, top = runRoot(t, "mcp", "install", "--print-config")
	if code != ExitOK {
		t.Fatalf("print-config over an unparseable config = %d (%s), want 0", code, top)
	}
	if !strings.Contains(stdout, `"mcpServers"`) {
		t.Errorf("stdout = %q, want the config it prints without reading anything", stdout)
	}
}

// An entry that is there with a DIFFERENT endpoint is rewritten, and both the
// old and the new are printed: silently replacing somebody's deliberate
// override is worse than either refusing or telling them.
func TestAnEntryWithADifferentEndpointIsRewrittenAndBothArePrinted(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	writeGlobalConfigFixture(t,
		`{"mcpServers":{"ccdad":{"type":"stdio","command":"/opt/ccdad/bin/ccdad","args":["mcp"],"env":{}}}}`)

	code, _, stderr, top := runRoot(t, "mcp", "install")
	if code != ExitOK {
		t.Fatalf("install over a different endpoint = %d (%s%s), want 0", code, stderr, top)
	}
	for _, want := range []string{"/opt/ccdad/bin/ccdad", `"command":"ccdad"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not print %s:\n%s", want, stderr)
		}
	}
	if got := userScopeEntry(t)["command"]; got != "ccdad" {
		t.Errorf("command = %v, want the entry to have been rewritten", got)
	}
}

// The record is what unwireMCP reads instead of guessing.
func TestInstallRecordsTheScopeItChose(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}

	rec, ok, err := loadMCPRecord()
	if err != nil || !ok {
		t.Fatalf("no install record: ok=%v err=%v", ok, err)
	}
	if rec.Scope != string(scopeUser) || rec.Name != "ccdad" || rec.SchemaVersion != 1 {
		t.Errorf("record = %+v, want scope user, name ccdad, schema 1", rec)
	}
	if rec.Path != mustPath(ccpath.GlobalConfigPath()) {
		t.Errorf("record path = %q, want the file that was actually written", rec.Path)
	}
	if rec.InstalledAt == "" {
		t.Error("the record carries no timestamp")
	}
}

// Uninstall removes exactly the entry it wrote and nothing else, and reports 3
// when there was nothing to remove.
func TestUninstallRemovesOnlyItsOwnEntryAndIsSafeToRunTwice(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	writeGlobalConfigFixture(t,
		`{"numStartups":41,"mcpServers":{"other":{"type":"stdio","command":"x"}}}`)
	if code, _, stderr, top := runRoot(t, "mcp", "install"); code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}

	code, _, stderr, top := runRoot(t, "mcp", "uninstall")
	if code != ExitOK {
		t.Fatalf("uninstall = %d (%s%s), want 0", code, stderr, top)
	}
	if got := countCcdadServers(t); got != 0 {
		t.Errorf("%d entries named ccdad survived the uninstall", got)
	}
	raw := compactJSON(t, readGlobalConfigFixture(t))
	for _, want := range []string{`"numStartups":41`, `"other"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("uninstall removed %s as well:\n%s", want, raw)
		}
	}

	code, _, stderr, top = runRoot(t, "mcp", "uninstall")
	if code != ExitNothingToDo {
		t.Fatalf("second uninstall = %d (%s%s), want %d", code, stderr, top, ExitNothingToDo)
	}
	if _, ok, _ := loadMCPRecord(); ok {
		t.Error("the install record survived an uninstall, so unwireMCP would go looking for an entry that is gone")
	}
}

// The default is user, and it is NOT Claude Code's own default. The help says
// so, because a user who has read Anthropic's documentation assumes they match.
func TestTheHelpSaysTheDefaultScopeDiffersFromClaudeCodesOwn(t *testing.T) {
	isolate(t)
	_, stdout, _, _ := runRoot(t, "mcp", "install", "--help")
	if !strings.Contains(stdout, "local") {
		t.Errorf("help = %q, want it to name Claude Code's own default of local", stdout)
	}
}

// Settings is not the config. It holds approval lists and no servers at all, so
// a command that wrote it would be putting an entry where nothing reads one.
func TestInstallNeverWritesClaudeCodesSettingsFile(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	settings := filepath.Join(mustPath(ccpath.ConfigHome()), "settings.json")

	for _, scope := range []string{"user", "local"} {
		if code, _, stderr, top := runRoot(t, "mcp", "install", "--scope", scope); code != ExitOK {
			t.Fatalf("install --scope %s = %d (%s%s)", scope, code, stderr, top)
		}
		if _, err := os.Stat(settings); !os.IsNotExist(err) {
			t.Fatalf("install --scope %s wrote %s", scope, settings)
		}
	}
}

// The entry is correct the moment it is written -- `command` is deliberately
// the bare word -- but nothing before this checked that the bare word
// RESOLVES. On this project that is not hypothetical: install.sh puts the
// binary in ~/.local/bin and tells the user to run `ccdad setup-path` next, so
// "not on PATH yet" is the ordinary state of a freshly installed machine.
// Without the warning the failure is invisible from ccdad's side and the first
// anyone hears of it is `claude mcp list` printing a connection error.
func TestAWarningNamesThisBinarysDirectoryWhenCcdadDoesNotResolve(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, false)

	code, _, stderr, top := runRoot(t, "mcp", "install")

	// Still 0. The entry is correct even though PATH is not set up yet, and
	// this is a diagnostic rather than a reason to fail the command.
	if code != ExitOK {
		t.Fatalf("install = %d (%s%s), want 0: a PATH that is not set up yet does not make the entry wrong",
			code, stderr, top)
	}
	if !strings.Contains(stderr, "setup-path") {
		t.Errorf("stderr does not point at the command that fixes it:\n%s", stderr)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, filepath.Dir(self)) {
		t.Errorf("stderr does not name the directory this binary is actually in (%s):\n%s",
			filepath.Dir(self), stderr)
	}
	// And it never says so when the word resolves.
	isolate(t)
	stubCcdadOnPath(t, true)
	if _, _, stderr, _ := runRoot(t, "mcp", "install"); strings.Contains(stderr, "setup-path") {
		t.Errorf("the warning fired on a machine where `ccdad` resolves:\n%s", stderr)
	}
}

// The local scope reaches two levels deeper than internal/cclink's writer goes,
// and the depth is where the damage would be: `projects` holds one entry per
// directory Claude Code has ever run in -- dozens on a real machine -- and a Go
// map's encoder sorts. Decoding it into one and re-encoding would turn adding
// one server into a whole-file diff of somebody's dotfile.
func TestTheLocalScopeWritesUnderThisDirectoryAndReordersNothing(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT in sorted order, and deliberately with this directory
	// last: a map round trip would bring "aaa" to the front and move "zzz".
	here, err := json.Marshal(cwd)
	if err != nil {
		t.Fatal(err)
	}
	writeGlobalConfigFixture(t, `{"projects":{"zzz":{"hasTrustDialogAccepted":true},`+
		`"aaa":{"hasTrustDialogAccepted":true},`+
		string(here)+`:{"hasTrustDialogAccepted":true}}}`)

	if code, _, stderr, top := runRoot(t, "mcp", "install", "--scope", "local"); code != ExitOK {
		t.Fatalf("install --scope local = %d (%s%s)", code, stderr, top)
	}

	raw := compactJSON(t, readGlobalConfigFixture(t))
	if strings.Index(raw, `"zzz"`) > strings.Index(raw, `"aaa"`) {
		t.Errorf("the projects object was reordered:\n%s", raw)
	}
	if !strings.Contains(raw, `"hasTrustDialogAccepted":true`) {
		t.Errorf("this directory's other keys did not survive:\n%s", raw)
	}
	if got := countCcdadServers(t); got != 1 {
		t.Errorf("%d entries named ccdad, want exactly 1 -- under this directory's project", got)
	}
	// The top-level mcpServers is NOT where local goes.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["mcpServers"]; ok {
		t.Error("--scope local wrote the top-level mcpServers, which is the user scope")
	}
}

// The project scope is a different FILE, and it is the one shape of this
// command that commits ccdad into somebody's repository -- which is why it is
// not the default even though Claude Code's own `mcp add` defaults nearby.
func TestTheProjectScopeWritesItsOwnFileAndNotTheGlobalConfig(t *testing.T) {
	isolate(t)
	stubCcdadOnPath(t, true)
	dir := t.TempDir()
	t.Chdir(dir)

	if code, _, stderr, top := runRoot(t, "mcp", "install", "--scope", "project"); code != ExitOK {
		t.Fatalf("install --scope project = %d (%s%s)", code, stderr, top)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("no .mcp.json in the project directory: %v", err)
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf(".mcp.json is not the wrapper shape: %v\n%s", err, raw)
	}
	if _, ok := doc.MCPServers["ccdad"]; !ok {
		t.Errorf("no ccdad server in .mcp.json:\n%s", raw)
	}
	if _, err := os.Stat(mustPath(ccpath.GlobalConfigPath())); !os.IsNotExist(err) {
		t.Error("--scope project wrote Claude Code's global config as well")
	}

	// And it is safe to run twice, in the file it owns as much as in the one
	// it shares.
	if code, _, _, _ := runRoot(t, "mcp", "install", "--scope", "project"); code != ExitNothingToDo {
		t.Errorf("second install --scope project = %d, want %d", code, ExitNothingToDo)
	}
}

// One artifact, three uses: the plugin's server config, --print-config's
// output, and the entry written into all three scopes. If the wrapper ever
// stops containing the entry byte for byte, a plugin registration and a direct
// one stop de-duplicating and the machine runs two full tool sets.
func TestTheWrapperContainsTheEntryByteForByte(t *testing.T) {
	if !strings.Contains(string(mcpConfigJSON()), string(mcpEntryJSON())) {
		t.Errorf("the wrapper does not contain the entry verbatim:\n%s\n%s", mcpConfigJSON(), mcpEntryJSON())
	}
	var entry map[string]any
	if err := json.Unmarshal(mcpEntryJSON(), &entry); err != nil {
		t.Fatalf("the entry is not JSON: %v", err)
	}
}

// stubCcdadOnPath describes a machine the test is not running on: whether the
// bare word `ccdad` resolves is a property of the developer's PATH, and both
// answers have to be exercised.
func stubCcdadOnPath(t *testing.T, found bool) {
	t.Helper()
	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(file string) (string, error) {
		if found {
			return filepath.Join(t.TempDir(), file), nil
		}
		return "", os.ErrNotExist
	}
}

func writeGlobalConfigFixture(t *testing.T, raw string) {
	t.Helper()
	path := mustPath(ccpath.GlobalConfigPath())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGlobalConfigFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.GlobalConfigPath()))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// compactJSON strips the formatting so an assertion is about content rather
// than about how the writer indented it.
func compactJSON(t *testing.T, raw string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(raw)); err != nil {
		t.Fatalf("compacting the config: %v\n%s", err, raw)
	}
	return out.String()
}

// userScopeEntry is the ccdad entry from the top-level mcpServers object.
func userScopeEntry(t *testing.T) map[string]any {
	t.Helper()
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readGlobalConfigFixture(t)), &doc); err != nil {
		t.Fatal(err)
	}
	entry, ok := doc.MCPServers["ccdad"]
	if !ok {
		t.Fatalf("no ccdad entry in the top-level mcpServers:\n%s", readGlobalConfigFixture(t))
	}
	return entry
}

// countCcdadServers counts entries named ccdad ANYWHERE in the config -- the
// top-level object and every project's -- because "exactly one" is a claim
// about the file rather than about one scope, and two scopes each holding one
// is the duplicate this command exists to avoid.
func countCcdadServers(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.GlobalConfigPath()))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the config is not readable after the write: %v\n%s", err, raw)
	}
	n := 0
	if _, ok := doc.MCPServers["ccdad"]; ok {
		n++
	}
	for _, p := range doc.Projects {
		if _, ok := p.MCPServers["ccdad"]; ok {
			n++
		}
	}
	return n
}
