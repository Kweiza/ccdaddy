package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// Skipped, not ok: "this check does not apply here" is an absence rather than a
// pass, and painting it ok would tell a user their codex accounts were checked
// and fine when there are none.
func TestTheCodexRowsAreSkippedWithNoCodexAccounts(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	for _, name := range []string{"codex-relogin", "codex-proxy"} {
		if got := r.level(t, name); got != "skipped" {
			t.Errorf("%s = %s with no codex accounts, want skipped: %s", name, got, r.detail(t, name))
		}
	}
}

func TestTheCodexReloginRowIsOKWithALiveGrant(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-relogin"); got != "ok" {
		t.Fatalf("codex-relogin = %s with a live grant: %s", got, r.detail(t, "codex-relogin"))
	}
}

// A dead grant is a warning and not a failure, on this file's own taxonomy: the
// machine works, the other accounts rotate, and one account is out until a
// person runs a command. The row has to NAME the command, because nothing else
// on the machine will.
func TestTheCodexReloginRowNamesTheAccountAndTheRemedy(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	markCodexReloginNeeded(t, "cx-1")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-relogin"); got != "warn" {
		t.Fatalf("codex-relogin = %s with a dead grant, want warn", got)
	}
	detail := r.detail(t, "codex-relogin")
	if !strings.Contains(detail, "codex@example.com") || !strings.Contains(detail, "ccdad codex add") {
		t.Fatalf("codex-relogin does not name the account and the remedy: %s", detail)
	}
}

// A mark that no longer names the stored token is a mark from before a
// re-login, and reporting it would send a user to re-run a command they already
// ran.
func TestTheCodexReloginRowIgnoresAStaleMark(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	markCodexReloginFor(t, "cx-1", codexauth.RefreshTokenHash("a-token-this-account-no-longer-holds"))

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-relogin"); got != "ok" {
		t.Fatalf("codex-relogin = %s on a mark that predates a re-login: %s", got, r.detail(t, "codex-relogin"))
	}
}

// The proxy is what makes a codex account usable at all, so its absence is a
// warning on a machine that has codex accounts -- and it says which of the two
// reasons it is: no daemon, or a daemon that published no port.
func TestTheCodexProxyRowWarnsWithNoDaemon(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubDaemon(t, daemon.Report{State: daemon.DaemonStopped}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-proxy"); got != "warn" {
		t.Fatalf("codex-proxy = %s with no daemon, want warn", got)
	}
	if !strings.Contains(r.detail(t, "codex-proxy"), "daemon") {
		t.Fatalf("codex-proxy does not say the daemon is not running: %s", r.detail(t, "codex-proxy"))
	}
}

// A running daemon that has published no port is SKIPPED, not warned about.
// Nothing in the status document says whether the listener failed to come up or
// whether this daemon has no listener to publish at all, and while the proxy
// half is not in the build the second is every machine with a codex account on
// it. A row that is yellow on every machine for a whole release is a row a
// reader learns to skip before it has ever had something to say.
func TestTheCodexProxyRowIsSkippedUntilAPortIsPublished(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubDaemon(t, daemon.Report{State: daemon.DaemonRunning, HasStatus: true}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-proxy"); got != "skipped" {
		t.Fatalf("codex-proxy = %s with a running daemon and no published port, want skipped: %s",
			got, r.detail(t, "codex-proxy"))
	}
	if !strings.Contains(r.detail(t, "codex-proxy"), "no codex proxy port") {
		t.Fatalf("codex-proxy does not say what is missing: %s", r.detail(t, "codex-proxy"))
	}
}

func TestTheCodexProxyRowNamesThePort(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status:    daemon.Status{CodexProxyPort: 24601},
	}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-proxy"); got != "ok" {
		t.Fatalf("codex-proxy = %s with a listening proxy: %s", got, r.detail(t, "codex-proxy"))
	}
	if !strings.Contains(r.detail(t, "codex-proxy"), "24601") {
		t.Fatalf("codex-proxy does not name the port: %s", r.detail(t, "codex-proxy"))
	}
}

// A fallback port is the one state a user has to act on: every codex session
// started before this daemon is talking to a port nothing is listening on, and
// codex's own symptom for that is an endless "Reconnecting" with no error.
func TestTheCodexProxyRowWarnsWhenThePortMoved(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status:    daemon.Status{CodexProxyPort: 24601, CodexProxyFellBack: true},
	}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-proxy"); got != "warn" {
		t.Fatalf("codex-proxy = %s after a port fallback, want warn", got)
	}
	if !strings.Contains(r.detail(t, "codex-proxy"), "relaunch") {
		t.Fatalf("codex-proxy does not say what to do: %s", r.detail(t, "codex-proxy"))
	}
}

// The fourth arm of checkCodexProxy: a proxy that is listening on the port it
// resolved, but some codex sessions had to launch outside it anyway. That
// state is a warning because a session ccdad never routed is a session ccdad
// cannot rotate, and its cost lands on whatever ~/.codex holds without ccdad
// having chosen that or being able to see it.
func TestTheCodexProxyRowWarnsAboutUnroutedLaunches(t *testing.T) {
	isolate(t)
	seedCodexAccount(t, "cx-1", "codex@example.com")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status:    daemon.Status{CodexProxyPort: 24601, CodexUnroutedLaunches: 3},
	}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-proxy"); got != "warn" {
		t.Fatalf("codex-proxy = %s with unrouted launches, want warn", got)
	}
	detail := r.detail(t, "codex-proxy")
	if !strings.Contains(detail, "3") {
		t.Fatalf("codex-proxy does not name the count of unrouted launches: %s", detail)
	}
	if !strings.Contains(detail, "24601") {
		t.Fatalf("codex-proxy does not still name the port it is listening on: %s", detail)
	}
}

func markCodexReloginNeeded(t *testing.T, uuid string) {
	t.Helper()
	markCodexReloginFor(t, uuid, codexauth.RefreshTokenHash("RT-"+uuid))
}

func markCodexReloginFor(t *testing.T, uuid, mark string) {
	t.Helper()
	if err := store.WithStore(func(s *store.Store) error {
		creds, err := s.Credentials(uuid)
		if err != nil {
			return err
		}
		c, _, err := codexauth.FromBlob(creds)
		if err != nil {
			return err
		}
		return s.SetCodexReloginFor(uuid, codexauth.RefreshTokenHash(c.RefreshToken), mark)
	}); err != nil {
		t.Fatal(err)
	}
}
