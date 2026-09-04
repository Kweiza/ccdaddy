// The stable port: resolved once per start, and recorded as what was bound.
//
// This comment describes the FILE and not the package. `internal/codexproxy`
// already has a package comment -- Part 3 landed `limitbook.go` here first and
// it carries one -- and a second one in this file would be a second package
// doc for the same package.
package codexproxy

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
)

const (
	// derivedBase and derivedSpan bound the band a port is derived into:
	// 20000-31999. It sits below every operating system's ephemeral range --
	// Linux starts at 32768, the BSDs and Windows at 49152 -- so a derived port
	// cannot be handed out by the kernel to somebody else's outbound socket
	// while the daemon is down and then be occupied when it comes back.
	derivedBase = 20000
	derivedSpan = 12000

	portPerm    = 0o600
	portDirPerm = 0o700
)

// PortPath is where the live port is recorded.
func PortPath(root string) string { return filepath.Join(root, "codex", "port") }

// ResolvePort decides which port to try, and says where the answer came from.
//
// The source is not decoration: a bind failure on a port the USER configured is
// a refusal the daemon must fail on, and a bind failure on a port ccdad chose
// for itself is a fallback.
func ResolvePort(root string, configured int) (int, string, error) {
	if configured != 0 {
		if configured < 1 || configured > 65535 {
			return 0, "", fmt.Errorf("the configured codex proxy port %d is not a port number", configured)
		}
		return configured, "config", nil
	}
	if p, ok := recordedPort(root); ok {
		return p, "recorded", nil
	}
	return derivedPort(root), "derived", nil
}

// RecordPort writes the port the daemon actually bound.
//
// It refuses anything outside 1-65535, and 0 above all: 0 is what a caller
// passes to ASK for a port, and a store that remembered it would ask the kernel
// for a fresh random port on every restart -- which strands every codex session
// started before the restart on a port nothing answers.
func RecordPort(root string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("refusing to record %d as the codex proxy port", port)
	}
	if err := os.MkdirAll(filepath.Dir(PortPath(root)), portDirPerm); err != nil {
		return fmt.Errorf("creating the codex directory: %w", err)
	}
	return atomicfile.WriteFile(PortPath(root), []byte(strconv.Itoa(port)+"\n"), portPerm)
}

// recordedPort reads the last port the daemon bound. Anything it cannot read as
// a port number reads as absent, so a truncated or hand-edited record costs a
// fallback rather than a failure to start.
func recordedPort(root string) (int, bool) {
	data, err := os.ReadFile(PortPath(root))
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// derivedPort is the port a store always agrees with itself about and two
// stores on one machine are unlikely to share.
func derivedPort(root string) int {
	h := fnv.New32a()
	// Hash.Write never returns an error.
	_, _ = h.Write([]byte(canonicalRoot(root)))
	return derivedBase + int(h.Sum32()%derivedSpan)
}

// canonicalRoot is the spelling the derivation hashes.
//
// Clean and ToSlash only, deliberately. Resolving symlinks would make the
// answer depend on whether the directory exists yet, so the first run and every
// run after it would derive different ports -- the one property this derivation
// exists to have.
func canonicalRoot(root string) string {
	return strings.TrimSuffix(filepath.ToSlash(filepath.Clean(root)), "/")
}
