package tokens

import (
	"os"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
)

// TestMain keeps every test in this package off the developer's own keychain:
// an empty fake item store is the default, and a spawn of the real
// /usr/bin/security panics. A test that needs an item installs its own
// cclinktest.Keychain.
func TestMain(m *testing.M) {
	cclinktest.Isolate()
	os.Exit(m.Run())
}
