package codexproxy_test

import (
	"os/exec"
	"strings"
	"testing"
)

// Codex accounts rotate only among Codex accounts. The proxy is where that
// could be broken by accident -- it is the one component holding credentials,
// an account chooser and a switch-shaped decision in one place -- so the rule is
// asserted against the build graph rather than against a habit.
//
// internal/switcher executes a Claude switch; internal/tokens spends Claude
// grants; internal/credhome claims the Claude credential home. None of the
// three is anywhere in these packages' closures, and none of them has anything
// to say about a Codex request.
//
// internal/oauth is the one that needs a weaker rule, and the reason is
// measured rather than assumed: it is already inside the closure of
// internal/store and internal/usage, which this package legitimately uses. So
// what is asserted about it is that nothing here IMPORTS it -- reaching for
// Claude's token machinery by name is the mistake worth catching, and it is the
// one a closure rule could never have caught here anyway.
func TestTheProxyNeverReachesTheClaudeCredentialPath(t *testing.T) {
	closureForbidden := []string{
		"github.com/Kweiza/ccdaddy/internal/switcher",
		"github.com/Kweiza/ccdaddy/internal/tokens",
		"github.com/Kweiza/ccdaddy/internal/credhome",
	}
	const directForbidden = "github.com/Kweiza/ccdaddy/internal/oauth"

	for _, pkg := range []string{
		"github.com/Kweiza/ccdaddy/internal/codexproxy",
		"github.com/Kweiza/ccdaddy/internal/codexlaunch",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			for _, bad := range closureForbidden {
				if dep == bad {
					t.Errorf("%s depends on %s", pkg, bad)
				}
			}
		}

		out, err = exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).Output()
		if err != nil {
			t.Fatalf("go list -f Imports %s: %v", pkg, err)
		}
		for _, imp := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if imp == directForbidden {
				t.Errorf("%s imports %s directly", pkg, imp)
			}
		}
	}
}
