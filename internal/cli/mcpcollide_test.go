package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInstalledPlugins puts Claude Code's own plugin registry where
// ccpath.ConfigHome() will find it. The shape is the one Claude Code writes: a
// version, then a map of "<plugin>@<marketplace>" to a LIST of records.
func writeInstalledPlugins(t *testing.T, claude, body string) {
	t.Helper()
	dir := filepath.Join(claude, "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestThePluginIsFoundUnderAMarketplaceNameNobodyPredicted(t *testing.T) {
	claude := isolate(t)
	writeInstalledPlugins(t, claude,
		`{"version":2,"plugins":{"ccdad@my-own-mirror":[{"scope":"user","installPath":"/x"}]}}`)

	got := installedCcdadPlugins()
	if len(got) != 1 {
		t.Fatalf("found %d installs, want 1; a marketplace can be added under any name, so a check "+
			"keyed on ccdad@ccdaddy reports a clean machine on a machine that is not", len(got))
	}
	if got[0].Key != "ccdad@my-own-mirror" {
		t.Errorf("Key = %q, want the key verbatim so the warning can name it", got[0].Key)
	}
	if got[0].Scope != "user" {
		t.Errorf("Scope = %q, want user", got[0].Scope)
	}
}

func TestAPluginRegistryThatCannotBeReadIsNotAnError(t *testing.T) {
	claude := isolate(t)
	// No file at all.
	if got := installedCcdadPlugins(); len(got) != 0 {
		t.Fatalf("found %d installs with no registry present", len(got))
	}
	writeInstalledPlugins(t, claude, "{ this is not json")
	if got := installedCcdadPlugins(); len(got) != 0 {
		t.Fatalf("found %d installs in a file that does not parse; this check is advisory and must "+
			"never be a reason ccdad mcp install fails", len(got))
	}
}

// CLAUDE_CONFIG_DIR moves the whole config home, and that is exactly what
// `ccdad run --full-profile` does. A hard-coded ~/.claude would answer this
// question against one account's profile rather than against the machine.
func TestTheRegistryIsReadFromTheConfigHomeAndNotFromTheHomeDirectory(t *testing.T) {
	claude := isolate(t)
	writeInstalledPlugins(t, claude,
		`{"version":2,"plugins":{"ccdad@ccdaddy":[{"scope":"user"}]}}`)

	elsewhere := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", elsewhere)
	if got := installedCcdadPlugins(); len(got) != 0 {
		t.Fatalf("found %d installs after the config home moved; the registry moves with it", len(got))
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	if got := installedCcdadPlugins(); len(got) != 1 {
		t.Fatalf("found %d installs with the config home back, want 1", len(got))
	}
}

func TestAnotherPluginsInstallIsNotCcdads(t *testing.T) {
	claude := isolate(t)
	writeInstalledPlugins(t, claude,
		`{"version":2,"plugins":{"flightdeck@somewhere":[{"scope":"user"}],"ccdadx@m":[{"scope":"user"}]}}`)
	if got := installedCcdadPlugins(); len(got) != 0 {
		t.Fatalf("found %d installs, want 0 -- the prefix is ccdad@ and ccdadx is another plugin", len(got))
	}
}

// The warning's job is to stop a permission rule from silently never firing
// again, so both spellings have to be in it verbatim. A user greps for the one
// they already wrote.
func TestTheCollisionWarningNamesBothToolSpellingsAndTheWayBack(t *testing.T) {
	var b bytes.Buffer
	warnPluginCollision(&b, []pluginInstall{{Key: "ccdad@ccdaddy", Scope: "user"}}, "user")
	got := b.String()
	for _, want := range []string{
		"ccdad@ccdaddy",
		"mcp__ccdad__",
		"mcp__plugin_ccdad_ccdad__",
		"stop firing",
		"ccdad mcp uninstall",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not contain %q:\n%s", want, got)
		}
	}
}

func TestNoWarningIsPrintedWhenNoPluginIsInstalled(t *testing.T) {
	var b bytes.Buffer
	warnPluginCollision(&b, nil, "user")
	if b.Len() != 0 {
		t.Errorf("wrote a warning with nothing to warn about:\n%s", b.String())
	}
}

// The warning is printed by the command that CREATES the collision, on the
// branch that wrote a file -- and by nothing else.
//
// Not on --print-config, which mutates nothing: the warning's second line says
// the entry was registered anyway, and printing that after a command that
// registered nothing is a false statement about the user's machine.
func TestOnlyTheMutatingPathWarnsAboutTheCollision(t *testing.T) {
	claude := isolate(t)
	stubCcdadOnPath(t, true)
	writeInstalledPlugins(t, claude,
		`{"version":2,"plugins":{"ccdad@ccdaddy":[{"scope":"user"}]}}`)

	if _, _, stderr, _ := runRoot(t, "mcp", "install", "--print-config"); stderr != "" {
		t.Errorf("--print-config warned about a collision it did not create:\n%s", stderr)
	}

	code, _, stderr, top := runRoot(t, "mcp", "install")
	if code != ExitOK {
		t.Fatalf("install = %d (%s%s)", code, stderr, top)
	}
	if !strings.Contains(stderr, "mcp__plugin_ccdad_ccdad__") {
		t.Errorf("the install that created the collision said nothing about it:\n%s", stderr)
	}
	if !strings.Contains(stderr, "user scope") {
		t.Errorf("the warning does not name the scope that was written:\n%s", stderr)
	}

	// And a second install, which changes nothing, does not repeat it: the
	// warning is about a change, and exit 3 means there was none.
	code, _, stderr, top = runRoot(t, "mcp", "install")
	if code != ExitNothingToDo {
		t.Fatalf("second install = %d (%s%s), want %d", code, stderr, top, ExitNothingToDo)
	}
	if strings.Contains(stderr, "mcp__plugin_ccdad_ccdad__") {
		t.Errorf("a no-op install repeated the collision warning:\n%s", stderr)
	}
}
