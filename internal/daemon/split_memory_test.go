package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
)

// The identity oracle's memory, and the line said once per login store, are
// kept for the logins that can still matter -- the ones a store holds, and the
// ones a pass saw there recently -- and nothing else. They used to be wiped
// whole once they reached 64 logins, and a login that never left its store was
// then asked about again through the profile endpoint, and described again in
// daemon.log, because others had come and gone beside it.

// A login that stays in the keychain item -- one that belongs to no account
// here -- is asked about once and described once, while the credentials file
// beside it holds a different older login of u-2 on every pass: the churn that
// used to fill the memory.
func TestALoginStillInAStoreIsNotAskedAboutOrDescribedAgain(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(8*time.Hour))
	seedAccountExpiring(t, "u-2", "org-2", tickEpoch.Add(8*time.Hour))
	cclinktest.Install(t, cclinktest.NewKeychain(blobExpiring("RT-stranger", tickEpoch.Add(8*time.Hour))))
	recordActive(t, "u-1")
	// Both disabled, so no plan moves the login and the stores stay as each
	// pass sets them.
	for _, uuid := range []string{"u-1", "u-2"} {
		if _, err := openEngineStore(t).SetDisabled(uuid, true); err != nil {
			t.Fatal(err)
		}
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	asked := map[string]int{}
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		asked[token]++
		if token == "AT-RT-stranger" {
			return "u-somebody-else", nil
		}
		return "u-2", nil
	}
	var log logLines
	log.attach(e)

	const passes = 100
	for i := 0; i < passes; i++ {
		writeLiveLoginRT(t, fmt.Sprintf("RT-u-2-older-%d", i), tickEpoch.Add(time.Hour))
		tick(t, e)
		now = now.Add(resolveMinInterval)
	}

	if len(asked) < passes/2 {
		t.Fatalf("precondition: the oracle was asked about %d logins in %d passes; the churn did not happen", len(asked), passes)
	}
	if n := asked["AT-RT-stranger"]; n != 1 {
		t.Errorf("the oracle was asked %d times about the login that never left the keychain item, want 1", n)
	}
	said := 0
	for _, line := range log.lines {
		if strings.Contains(line, "the keychain item holds a login that belongs to no account") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the line about the keychain item's login was said %d times, want 1", said)
	}
	// And what is remembered is bounded by what can still matter: the two
	// logins in the stores, and those a pass saw there within the grace -- one
	// a pass, a pass a resolveMinInterval apart.
	bound := 2 + int(loginMemoryGrace/resolveMinInterval)
	if n := len(e.owners); n > bound {
		t.Errorf("the oracle's memory holds %d logins after %d passes, want at most %d", n, passes, bound)
	}
	if n := len(e.saidStore); n > bound {
		t.Errorf("the said-once memory holds %d logins after %d passes, want at most %d", n, passes, bound)
	}
}
