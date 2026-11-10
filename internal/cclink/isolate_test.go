package cclink

import (
	"context"
	"testing"
)

// IsolateKeychainForTests makes a spawn of the real binary FAIL LOUDLY: the
// other packages' test mains rely on it to catch a test that would otherwise
// read the developer's own keychain.
func TestASpawnOfTheRealSecurityPanicsOnceForbidden(t *testing.T) {
	fakeSecurity{}.install(t)
	realSecurityForbidden.Store(true)
	t.Cleanup(func() { realSecurityForbidden.Store(false) })

	defer func() {
		if recover() == nil {
			t.Fatal("a forbidden spawn of security did not panic")
		}
	}()
	_, _ = spawnSecurity(context.Background(), keychainReadTimeout, "find-generic-password", "-s", "x")
}
