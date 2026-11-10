package tokens

import (
	"os"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// TestMain keeps every test in this package off the developer's own keychain:
// an empty fake item store is the default, and a spawn of the real
// /usr/bin/security panics. A test that needs an item installs its own
// FakeKeychain.
func TestMain(m *testing.M) {
	cclink.IsolateKeychainForTests()
	os.Exit(m.Run())
}
