package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// manualFleet is one readable account, which is all these tests need: the
// question is whether the MODE is named, not what the table says.
func manualFleet(t *testing.T) {
	t.Helper()
	isolate(t)
	freezeClock(t, statusNow)
	seedAccount(t, "uuid-a", "work@example.com")
	seedUsageEntry(t, "uuid-a", usage.Entry{
		FetchedAt: statusNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, statusNow.Add(30*time.Minute)),
			SevenDay: window(62, statusNow.Add(2*time.Hour+14*time.Minute)),
		},
	})
}

// ---- the command ------------------------------------------------------------

func TestManualOnWritesTheKeyAndOffTakesItBack(t *testing.T) {
	manualFleet(t)

	if code, _, _, _ := runRoot(t, "manual", "on"); code != ExitOK {
		t.Fatalf("manual on exit %d, want 0", code)
	}
	code, stdout, _, _ := runRoot(t, "config", "get", "manual")
	if code != ExitOK || strings.TrimSpace(stdout) != "true" {
		t.Fatalf("config get manual = %q (exit %d), want true", stdout, code)
	}

	if code, _, _, _ := runRoot(t, "manual", "off"); code != ExitOK {
		t.Fatalf("manual off exit %d, want 0", code)
	}
	code, stdout, _, _ = runRoot(t, "config", "get", "manual")
	if code != ExitOK || strings.TrimSpace(stdout) != "false" {
		t.Fatalf("config get manual = %q (exit %d), want false", stdout, code)
	}
}

// The exit contract: 3 is "the world is already how you asked for it", and it is
// what makes `ccdad manual on` idempotent in a provisioning script.
func TestManualOnTwiceIsNothingToDo(t *testing.T) {
	manualFleet(t)

	if code, _, _, _ := runRoot(t, "manual", "on"); code != ExitOK {
		t.Fatalf("first manual on exit %d, want 0", code)
	}
	if code, _, _, _ := runRoot(t, "manual", "on"); code != ExitNothingToDo {
		t.Errorf("second manual on exit %d, want %d", code, ExitNothingToDo)
	}
}

// status is a probe, so the mode being off is a negative answer rather than a
// failure — which is what makes `ccdad manual status >/dev/null || ccdad manual
// on` correct.
func TestManualStatusProbesRatherThanFails(t *testing.T) {
	manualFleet(t)

	code, stdout, _, _ := runRoot(t, "manual", "status")
	if code != ExitProbeNegative {
		t.Errorf("manual status with the mode off exit %d, want %d", code, ExitProbeNegative)
	}
	if !strings.Contains(stdout, "off") {
		t.Errorf("manual status does not say the mode is off:\n%s", stdout)
	}

	runRoot(t, "manual", "on")
	code, stdout, _, _ = runRoot(t, "manual", "status")
	if code != ExitOK {
		t.Errorf("manual status with the mode on exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "on") {
		t.Errorf("manual status does not say the mode is on:\n%s", stdout)
	}
}

func TestManualRefusesAVerbItDoesNotHave(t *testing.T) {
	manualFleet(t)
	if code, _, _, _ := runRoot(t, "manual", "sometimes"); code != ExitUsage {
		t.Errorf("manual sometimes exit %d, want %d", code, ExitUsage)
	}
}

// ---- the surfaces name it ---------------------------------------------------

// A mode that suppresses the program's whole purpose has to be visible. Every
// other line of the status block reads exactly as it would with the mode off,
// which is precisely why this one has to be there.
func TestStatusNamesManualModeOnlyWhenItIsOn(t *testing.T) {
	manualFleet(t)

	_, stdout, _, _ := runRoot(t, "status")
	if strings.Contains(stdout, "Manual:") {
		t.Errorf("the status block names manual mode while it is off:\n%s", stdout)
	}

	runRoot(t, "manual", "on")
	_, stdout, _, _ = runRoot(t, "status")
	if !strings.Contains(stdout, "Manual:") {
		t.Fatalf("the status block does not name manual mode:\n%s", stdout)
	}
	// The sentence a reader needs is not "ccdad has stopped" but "ccdad has
	// stopped and you have not".
	if !strings.Contains(stdout, "ccdad switch") {
		t.Errorf("the Manual: line does not say that ccdad switch still works:\n%s", stdout)
	}
}

func TestListNotesManualModeOnlyWhenItIsOn(t *testing.T) {
	manualFleet(t)

	_, _, stderr, _ := runRoot(t, "list")
	if strings.Contains(stderr, "manual mode") {
		t.Errorf("list notes manual mode while it is off:\n%s", stderr)
	}

	runRoot(t, "manual", "on")
	_, _, stderr, _ = runRoot(t, "list")
	if !strings.Contains(stderr, "manual mode is on") {
		t.Errorf("list does not note manual mode:\n%s", stderr)
	}
}

// doctor is where a person looks when a fleet has stopped switching, and every
// other row there reads ok in this mode.
func TestDoctorCarriesAManualModeRow(t *testing.T) {
	manualFleet(t)

	runRoot(t, "manual", "on")
	code, stdout, _, _ := runRoot(t, "doctor")
	if !strings.Contains(stdout, "manual-mode") {
		t.Fatalf("doctor has no manual-mode row:\n%s", stdout)
	}
	if !strings.Contains(stdout, "will NOT move the live login") {
		t.Errorf("the manual-mode row does not say what the mode does:\n%s", stdout)
	}
	// warn and never fail: the user asked for this, and fail is the only level
	// that changes doctor's exit code.
	if code == ExitFailure {
		t.Errorf("doctor exit %d — an intentional mode must not turn a health check red", code)
	}
}

// ---- and the engine actually obeys it ---------------------------------------

// The end-to-end wire: the key in config.toml reaches Decide. This is
// TestAutoOnceSwitchesToTheAccountWithRoom's own fixture, which is the point --
// the pool is one that demonstrably switches, so the only thing that changed is
// the mode.
func TestAutoOnceStaysPutInManualMode(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	before := liveUUIDOf(t)

	runRoot(t, "manual", "on")
	code, stdout, stderr, top := runRoot(t, "auto", "--once")
	if code != ExitNothingToDo {
		t.Fatalf("auto --once exit %d, want %d (staying put, not blocked)\n%s\n%s\n%s",
			code, ExitNothingToDo, stdout, stderr, top)
	}
	if !strings.Contains(stdout+stderr, "manual mode") {
		t.Errorf("auto --once does not say manual mode is why:\n%s\n%s", stdout, stderr)
	}
	if got := liveUUIDOf(t); got != before {
		t.Errorf("live account moved to %q; manual mode must not switch", got)
	}
}
