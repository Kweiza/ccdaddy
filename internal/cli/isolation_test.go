package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A re-executed child of this test binary -- the `status --json` and
// `auto --once --json` runs of the closed-reader contract, the MCP server --
// runs its role under the same keychain isolation as the tests: the empty
// fake, and a panic on any spawn of the real /usr/bin/security. Isolated only
// after the role dispatch, such a child would read the developer's real
// keychain.
func TestAReExecutedChildRunsWithTheKeychainIsolated(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), isolationRoleEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the child failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Fatalf("keychain isolated in a re-executed child = %q, want true", got)
	}
}
