package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// doctorReport is one run of the command, parsed out of --json.
type doctorReport struct {
	payload map[string]any
	checks  map[string]map[string]any
}

func runDoctor(t *testing.T) (ExitCode, doctorReport, string) {
	t.Helper()
	code, stdout, stderr, _ := runRoot(t, "doctor", "--json")
	payload := statusJSON(t, stdout)
	out := doctorReport{payload: payload, checks: map[string]map[string]any{}}
	rows, ok := payload["checks"].([]any)
	if !ok {
		t.Fatalf("no checks array: %v", payload)
	}
	for _, r := range rows {
		row := r.(map[string]any)
		out.checks[row["name"].(string)] = row
	}
	return code, out, stderr
}

func (r doctorReport) check(t *testing.T, name string) map[string]any {
	t.Helper()
	c, ok := r.checks[name]
	if !ok {
		names := make([]string, 0, len(r.checks))
		for n := range r.checks {
			names = append(names, n)
		}
		t.Fatalf("no check named %q; there are %v", name, names)
	}
	return c
}

func (r doctorReport) level(t *testing.T, name string) string {
	t.Helper()
	return r.check(t, name)["level"].(string)
}

func (r doctorReport) detail(t *testing.T, name string) string {
	t.Helper()
	d, _ := r.check(t, name)["detail"].(string)
	return d
}

// seedHealthyMachine leaves the store and the live credentials file in the
// state a working install has, so a test can assert on one departure from it.
func seedHealthyMachine(t *testing.T) {
	t.Helper()
	seedAccount(t, "uuid-a", "work@example.com")
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-uuid-a"}}`)
}

// §8.2's rule, one layer up: creating the lock file while checking for it
// destroys the one piece of genuine evidence that no daemon ever started here.
// store.Open would create the store directory too, which is why doctor does not
// use it — a diagnostic that manufactures what it reports on is worthless.
func TestDoctorCreatesNothing(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	t.Setenv("CCDAD_HOME", missing)

	runDoctor(t)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("doctor created the store directory it was asked to report on: %v", err)
	}
	if _, err := os.Stat(mustPath(daemon.LockPath())); !os.IsNotExist(err) {
		t.Error("doctor created the daemon lock file, destroying the evidence that no daemon ever started here")
	}
}

// §8.2 names this as doctor's job: a store that points at nothing is a
// configuration question, and the singleton answers "not running" for it on
// purpose rather than "cannot determine".
//
// It is a warning and not a failure because the two readings are
// indistinguishable from here — a fresh install and a mistyped CCDAD_HOME
// produce exactly the same evidence — so the report has to name both rather
// than pick one and make a new machine look broken.
func TestDoctorNamesAStoreThatPointsAtNothing(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	t.Setenv("CCDAD_HOME", missing)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "store"); got != "warn" {
		t.Errorf("store check = %q, want warn: %s", got, r.detail(t, "store"))
	}
	detail := r.detail(t, "store")
	if !strings.Contains(detail, missing) {
		t.Errorf("the store path is not named: %s", detail)
	}
	if !strings.Contains(detail, "CCDAD_HOME") {
		t.Errorf("the detail does not offer the other reading — a store pointing somewhere unintended: %s", detail)
	}
	if code != ExitOK {
		t.Errorf("exit %d for a machine ccdad has never run on, want 0", code)
	}
	// Everything downstream of the store is skipped rather than answered from
	// nothing: a "permissions ok" for a directory that does not exist is a
	// diagnostic that lies.
	for _, name := range []string{"permissions", "pidfile", "usage-cache", "engine-state", "config", "sessions"} {
		if got := r.level(t, name); got != "skipped" {
			t.Errorf("%s = %q with no store to check, want skipped: %s", name, got, r.detail(t, name))
		}
	}
}

// A relative store IS a failure: store.Open refuses one outright, because it
// would put a credentials tree in whatever directory ccdad happened to be run
// from, a different one each time, with live tokens in it.
func TestDoctorFailsOnARelativeStore(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_HOME", "relative-store")

	code, r, _ := runDoctor(t)
	if got := r.level(t, "store"); got != "fail" {
		t.Errorf("store check = %q, want fail: %s", got, r.detail(t, "store"))
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
}

// The highest-value check and the easiest to get wrong. The question is not "is
// a daemon running" but "do locks work on this filesystem at all". A doctor that
// reports "no daemon" where locks are broken has reproduced clauth's bug inside
// the diagnostic tool meant to find it.
func TestDoctorReportsABrokenLockAsBrokenAndNotAsNoDaemon(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubDaemon(t, daemon.Report{State: daemon.DaemonUnknown}, daemon.ErrLocksUnsupported)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "locks"); got != "fail" {
		t.Errorf("locks check = %q, want fail", got)
	}
	detail := strings.ToLower(r.detail(t, "locks"))
	if strings.Contains(detail, "not running") || strings.Contains(detail, "no daemon") {
		t.Errorf("a filesystem that cannot lock was reported as an absent daemon: %s", detail)
	}
	if !strings.Contains(detail, "lock") {
		t.Errorf("the locks check does not mention locking: %s", detail)
	}
	// Naming the condition is only half of doctor's job; the other half is
	// saying what to do about it. A generic "could not be probed" leaves the
	// user with a diagnostic and no next step.
	if !strings.Contains(detail, "nfs") && !strings.Contains(detail, "cifs") {
		t.Errorf("the locks check does not name what does this — an NFS or CIFS mount with no lock daemon: %s", detail)
	}
	if !strings.Contains(detail, "ccdad_home") {
		t.Errorf("the locks check does not say what to do about it: %s", detail)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1 when a check fails", code)
	}
}

// Any probe failure is a failure, not just the recognised errno. "Cannot tell"
// is the condition the three-outcome contract exists for, whatever produced it.
func TestDoctorFailsOnAnyUnprobeableLock(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubDaemon(t, daemon.Report{State: daemon.DaemonUnknown}, errors.New("something else went wrong"))

	code, r, _ := runDoctor(t)
	if got := r.level(t, "locks"); got != "fail" {
		t.Errorf("locks = %q, want fail: %s", got, r.detail(t, "locks"))
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
}

// A machine with no daemon running is a healthy machine. A doctor that failed on
// it would cry wolf on every laptop that has not started one yet.
func TestDoctorTreatsAnAbsentDaemonAsHealthy(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "locks"); got != "ok" {
		t.Errorf("locks check = %q on a machine where locking works, want ok: %s", got, r.detail(t, "locks"))
	}
	if code != ExitOK {
		t.Errorf("exit %d on a healthy machine, want 0", code)
	}
}

// §4.3's "on startup" half — the only part of it that was still missing. Six
// machine keys drifted in after clauth's one-key list was written, so this is
// demonstrated drift rather than a hypothetical.
func TestDoctorReportsUnknownCredentialKeys(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	addLiveKey(t, "somethingAnthropicAddedLater", `{"a":1}`)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "credential-keys"); got != "warn" {
		t.Errorf("credential-keys = %q, want warn", got)
	}
	if !strings.Contains(r.detail(t, "credential-keys"), "somethingAnthropicAddedLater") {
		t.Errorf("the drifted key is not named: %s", r.detail(t, "credential-keys"))
	}
	// A warning is not a failure: an unrecognised key is preserved by the
	// deny-list, so nothing is broken — the user is being told to update ccdad.
	if code != ExitOK {
		t.Errorf("exit %d for a warning, want 0", code)
	}
}

func TestDoctorIsQuietWhenNoKeyHasDrifted(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	addLiveKey(t, "mcpOAuth", `{"server":{}}`)
	addLiveKey(t, "gatewayTrust", `{"host":"fp"}`)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-keys"); got != "ok" {
		t.Errorf("credential-keys = %q for two known machine keys, want ok: %s", got, r.detail(t, "credential-keys"))
	}
}

// switch.go already warns when it is about to be a no-op; doctor says it before
// the user tries.
func TestDoctorReportsTheEnvironmentHazards(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-whatever")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-whatever")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "environment"); got != "warn" {
		t.Errorf("environment = %q, want warn", got)
	}
	detail := r.detail(t, "environment")
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the environment check does not name %s: %s", want, detail)
		}
	}
	// The secrets themselves must never be echoed by a diagnostic a user pastes
	// into an issue.
	for _, secret := range []string{"sk-ant-oat01-whatever", "sk-ant-api03-whatever"} {
		if strings.Contains(detail, secret) {
			t.Errorf("doctor printed a live token: %s", detail)
		}
	}
}

func TestDoctorIsQuietWithACleanEnvironment(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "environment"); got != "ok" {
		t.Errorf("environment = %q, want ok: %s", got, r.detail(t, "environment"))
	}
}

func TestDoctorReportsLooseStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows")
	}
	isolate(t)
	seedHealthyMachine(t)
	if err := os.Chmod(mustPath(ccpath.StoreHome()), 0o755); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "permissions"); got != "fail" {
		t.Errorf("permissions = %q, want fail", got)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestDoctorReportsALooseCredentialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows")
	}
	isolate(t)
	seedHealthyMachine(t)
	matches, err := filepath.Glob(filepath.Join(mustPath(ccpath.StoreHome()), "credentials", "*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no stored credential to loosen: %v %v", matches, err)
	}
	if err := os.Chmod(matches[0], 0o644); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "permissions"); got != "fail" {
		t.Errorf("permissions = %q, want fail: %s", got, r.detail(t, "permissions"))
	}
	if !strings.Contains(r.detail(t, "permissions"), filepath.Base(matches[0])) {
		t.Errorf("the loose file is not named: %s", r.detail(t, "permissions"))
	}
}

func TestDoctorAcceptsATightStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows")
	}
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "permissions"); got != "ok" {
		t.Errorf("permissions = %q on a store ccdad wrote itself, want ok: %s", got, r.detail(t, "permissions"))
	}
}

// pidfile.go names doctor as the reader that has to see this: a body that IS
// committed but does not parse is a damaged store, and folding it into "nothing
// to read" is what sends a supervisor into a respawn loop.
func TestDoctorReportsACorruptPidfile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(mustPath(daemon.PIDPath()), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "pidfile"); got != "fail" {
		t.Errorf("pidfile = %q, want fail", got)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
	if r.payload["ok"] != false {
		t.Errorf("ok = %v with a failing check", r.payload["ok"])
	}
}

func TestDoctorAcceptsAnAbsentPidfile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "pidfile"); got != "ok" {
		t.Errorf("pidfile = %q with no pidfile at all, want ok: %s", got, r.detail(t, "pidfile"))
	}
}

func TestDoctorReportsACorruptStatusFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonStopped,
		StatusErr: os.ErrInvalid,
	}, nil)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "status-file"); got != "fail" {
		t.Errorf("status-file = %q, want fail", got)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestDoctorReportsCorruptEngineState(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), "strategy.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "engine-state"); got != "warn" {
		t.Errorf("engine-state = %q, want warn: %s", got, r.detail(t, "engine-state"))
	}
}

func TestDoctorReportsACorruptUsageCache(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), "usage.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "usage-cache"); got != "warn" {
		t.Errorf("usage-cache = %q, want warn: %s", got, r.detail(t, "usage-cache"))
	}
}

// §12's High-severity risk: Claude Code changes these internals between
// releases. A credentials file ccdad cannot parse is the loudest form of that,
// and switch deliberately refuses to repair one.
func TestDoctorReportsAnUnparseableCredentialsFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	writeLiveFile(t, "this is not json")

	code, r, _ := runDoctor(t)
	if got := r.level(t, "claude-code"); got != "fail" {
		t.Errorf("claude-code = %q, want fail", got)
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestDoctorAcceptsAMachineWhereClaudeCodeHasNeverLoggedIn(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-a", "work@example.com")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "claude-code"); got == "fail" {
		t.Errorf("claude-code = fail with no credentials file; that is a fresh machine, not a broken one: %s",
			r.detail(t, "claude-code"))
	}
}

func TestDoctorHumanOutputNamesEveryCheck(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	code, stdout, _, _ := runRoot(t, "doctor")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	for _, name := range []string{"store", "path", "permissions", "locks", "pidfile", "status-file", "usage-cache", "engine-state", "config", "sessions", "profiles", "credential-home", "claude-code", "credential-keys", "keychain", "environment", "api-key"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("the human report does not mention the %s check:\n%s", name, stdout)
		}
	}

	// And the same question asked structurally, because the list above is
	// hand-kept and asserts CONTAINMENT: a check added to runChecks and not to
	// that literal is a gap this test would otherwise pass straight over. The
	// JSON report is generated from the same slice, so anything it names must
	// appear on the human path too.
	_, report, _ := runDoctor(t)
	for name := range report.checks {
		if !strings.Contains(stdout, name) {
			t.Errorf("the %s check is in the JSON report and not in the human one:\n%s", name, stdout)
		}
	}
}

// §13 open question 4 is unsettled, so this ships report-only. A repair would
// have to be a deliberate act behind a flag, and there is no flag.
func TestDoctorRepairsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows")
	}
	isolate(t)
	seedHealthyMachine(t)
	if err := os.Chmod(mustPath(ccpath.StoreHome()), 0o755); err != nil {
		t.Fatal(err)
	}

	runDoctor(t)
	info, err := os.Stat(mustPath(ccpath.StoreHome()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("doctor changed the store mode to %04o; it reports, it does not repair", info.Mode().Perm())
	}
}

func TestDoctorJSONCarriesASchemaVersion(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if r.payload["schemaVersion"] != float64(1) {
		t.Errorf("schemaVersion = %v", r.payload["schemaVersion"])
	}
	if r.payload["ok"] != true {
		t.Errorf("ok = %v on a healthy machine", r.payload["ok"])
	}
}

func TestDoctorTakesNoArguments(t *testing.T) {
	isolate(t)
	if code, _, _, _ := runRoot(t, "doctor", "extra"); code != ExitUsage {
		t.Errorf("exit %d, want 2", code)
	}
}

// A broken config.toml is ignored SILENTLY by the engine — that is the whole
// point of §8.4's "keep running on the last good config" — so doctor is where a
// user finds out it has been doing nothing.
func TestDoctorReportsAnUnusableConfigFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), config.FileName), []byte("threshold = = 9"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "config"); got != "warn" {
		t.Errorf("config = %q, want warn: %s", got, r.detail(t, "config"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0: a config ccdad can fall back from is not a failure", code)
	}
}

func TestDoctorReportsAConfigItCanRead(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), config.FileName), []byte("threshold = 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "config"); got != "ok" {
		t.Errorf("config = %q, want ok: %s", got, r.detail(t, "config"))
	}
}

// No config file at all is the ordinary state of a machine that never needed
// one, not a warning to act on.
func TestDoctorTreatsAMissingConfigAsOrdinary(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "config"); got != "ok" {
		t.Errorf("config = %q, want ok: %s", got, r.detail(t, "config"))
	}
}

// §12 lists unattended credit spend as a High risk, and doctor is where a user
// checks what their machine will do while they are not watching.
func TestDoctorSaysWhenUnattendedSpendingIsArmed(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), config.FileName),
		[]byte("[credit]\nmax_auto_spend = 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "config"); got != "ok" {
		t.Fatalf("config = %q, want ok", got)
	}
	detail := r.detail(t, "config")
	if !strings.Contains(detail, "100") || !strings.Contains(detail, "spending") {
		t.Errorf("config detail = %q, want it to say unattended spending is armed and up to how much", detail)
	}
}

// A key ccdad does not know is preserved rather than deleted, which means a
// typo survives forever doing nothing. doctor is the one place that says so.
func TestDoctorNamesConfigKeysItDoesNotKnow(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), config.FileName),
		[]byte("threshhold = 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "config"); got != "warn" {
		t.Errorf("config = %q, want warn: %s", got, r.detail(t, "config"))
	}
	if !strings.Contains(r.detail(t, "config"), "threshhold") {
		t.Errorf("config detail = %q, want the key it is ignoring named", r.detail(t, "config"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0: an ignored key is not a failure", code)
	}
}

// seedLeakedSession writes what a `ccdad run` that never got to clean up
// leaves behind: a directory under the store holding a live refresh token.
func seedLeakedSession(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-leaked-secret"}}`
	if err := os.WriteFile(filepath.Join(dir, ccpath.CredentialsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A session directory holds a live refresh token. `ccdad run` deletes its own
// on the way out, but a machine that was powered off mid-session, or a run
// whose adopt-back failed, leaves one — and nothing else in the tree would ever
// mention it again.
func TestDoctorReportsSessionCredentialsLeftBehind(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	seedLeakedSession(t, "u-1-123")
	seedLeakedSession(t, "u-2-456")

	code, r, _ := runDoctor(t)
	if got := r.level(t, "sessions"); got != "warn" {
		t.Errorf("sessions = %q, want warn: %s", got, r.detail(t, "sessions"))
	}
	detail := r.detail(t, "sessions")
	if !strings.Contains(detail, "2") {
		t.Errorf("the detail does not say how many were found: %q", detail)
	}
	// A diagnostic is what a user pastes into an issue.
	if strings.Contains(detail, "RT-leaked-secret") {
		t.Errorf("the detail printed a token out of a session's credentials: %q", detail)
	}
	// Warn does not fail the machine. Only levelFail changes the exit code.
	if code != ExitOK {
		t.Errorf("exit %d for leftover sessions, want 0 — a warn is not a broken machine", code)
	}
}

// The ordinary answer on a machine where every run cleaned up after itself,
// and on one that has never run a session at all: os.ReadDir on a container
// that was never created is "no sessions", not an error, and doctor must not
// create it either.
func TestDoctorReportsNoSessionsWhenTheContainerWasNeverCreated(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "sessions"); got != "ok" {
		t.Errorf("sessions = %q, want ok: %s", got, r.detail(t, "sessions"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0", code)
	}
	container := filepath.Join(mustPath(ccpath.StoreHome()), SessionsDirName)
	if _, err := os.Stat(container); !os.IsNotExist(err) {
		t.Errorf("doctor created %s (err = %v); the probe must not create what it probes", container, err)
	}
}

// A session directory holds a live refresh token, so loose modes on it are the
// same failure as loose modes on the store — and the permissions check does not
// see them: it reads one level deep and skips directories, so a world-readable
// token inside a session would otherwise be reported as "ok".
func TestDoctorFailsOnASessionDirectoryAnyoneCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows")
	}
	isolate(t)
	seedHealthyMachine(t)
	dir := seedLeakedSession(t, "u-1-123")
	if err := os.Chmod(filepath.Join(dir, ccpath.CredentialsFile), 0o644); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "sessions"); got != "fail" {
		t.Errorf("sessions = %q, want fail: %s", got, r.detail(t, "sessions"))
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want %d — only fail changes the exit code, and this is one", code, ExitFailure)
	}
}

// The keychain check, whose whole subject is a platform this suite does not run
// on. Every case below goes through the stub in isolate, because the alternative
// -- letting the real probe decide -- makes the answer depend on which machine
// ran the test, and on a Mac would spawn /usr/bin/security once per doctor test.

// Off macOS there is nothing to check, and "skipped" says so without pretending
// the machine is clean.
func TestDoctorSkipsTheKeychainWhereThereIsNone(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "skipped" {
		t.Fatalf("keychain level = %q, want skipped\n%s", got, report.detail(t, "keychain"))
	}
}

// A machine that has been switched to the file store and never cleaned up. The
// assertion is on the REMEDIATION, not on the level: a warning that named the
// problem without the command to fix it would pass a level check and be useless
// to the person reading it, and there is nothing else in the tree that can spell
// this item's name.
func TestDoctorReportsAStaleKeychainItem(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubKeychain(t, true, cclink.KeychainItem{
		Service: "Claude Code-credentials-aa3d8c96",
		Account: "tester",
	}, nil)

	code, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "warn" {
		t.Fatalf("keychain level = %q, want warn", got)
	}
	// Still exit 0. A stale item breaks nothing on any Claude Code that can be
	// installed today; only a downgrade makes it bite, and doctor's exit code is
	// reserved for what is broken now.
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 — a warning is not a failure", code)
	}
	detail := report.detail(t, "keychain")
	for _, want := range []string{
		"delete-generic-password",
		"Claude Code-credentials-aa3d8c96",
		"tester",
		// The boundary, both sides of it. "An older Claude Code" is not
		// actionable on its own: a user cannot tell whether the build they have
		// is one, and the answer is a version number that was measured rather
		// than guessed.
		"2.1.112",
		"2.1.113",
		// The half that was missing, and the one that matters most. On a
		// machine still reading the keychain, deleting the item is not a fix at
		// all: the next credential write puts it back and takes the credentials
		// file with it, because that era's update() deletes the file whenever
		// the pre-write keychain read came back empty.
		"recreates it and deletes the credentials file",
		"upgrade Claude Code instead",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not mention %q:\n%s", want, detail)
		}
	}
	// The caveat comes BEFORE the command, not after it. Someone who reads far
	// enough to copy the invocation has necessarily already read the case in
	// which running it makes things worse — which is the whole difference
	// between a remedy and a way to lose the login and the file at once.
	if cost, cmd := strings.Index(detail, "recreates it"), strings.Index(detail, "delete-generic-password"); cost > cmd {
		t.Errorf("the command is offered before its caveat is named:\n%s", detail)
	}
}

// A decomposed CLAUDE_CONFIG_DIR splits the item into two names, and a clean
// answer has to say it looked for both. Naming one of them would be the same
// half-answer as the securestorage derivation this check was corrected from:
// certain, specific, and about a name Claude Code never wrote.
func TestDoctorNamesEveryKeychainNameItCheckedFor(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	composed := cclink.KeychainItem{Service: "Claude Code-credentials-0873cca0", Account: "tester"}
	raw := cclink.KeychainItem{Service: "Claude Code-credentials-16eb4464", Account: "tester"}
	stubKeychainCandidates(t, false, composed, []cclink.KeychainItem{composed, raw}, nil)

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "ok" {
		t.Fatalf("keychain level = %q, want ok", got)
	}
	detail := report.detail(t, "keychain")
	for _, want := range []string{composed.Service, raw.Service} {
		if !strings.Contains(detail, want) {
			t.Errorf("the clean answer does not name %q as checked:\n%s", want, detail)
		}
	}
}

// The clean answer names the item it looked for. A bare "ok" would hide the case
// this check exists to make visible: CLAUDE_CONFIG_DIR changes the item's name,
// so a user can be told "nothing there" about an item Claude Code never wrote.
func TestDoctorNamesTheKeychainItemItLookedFor(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubKeychain(t, false, cclink.KeychainItem{
		Service: "Claude Code-credentials-aa3d8c96",
		Account: "tester",
	}, nil)

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "ok" {
		t.Fatalf("keychain level = %q, want ok", got)
	}
	if detail := report.detail(t, "keychain"); !strings.Contains(detail, "Claude Code-credentials-aa3d8c96") {
		t.Fatalf("the detail does not name the item it checked:\n%s", detail)
	}
}

// lockedKeychain is a probe failure that can explain itself, which is the shape
// cclink.KeychainError has. The interface is what doctor matches on, so a test
// can describe a locked keychain without cclink exporting its classification.
type lockedKeychain struct{}

func (lockedKeychain) Error() string  { return "security find-generic-password: keychain-locked" }
func (lockedKeychain) Detail() string { return "the login keychain is locked, so ccdad cannot tell" }

// §8.2's rule, at the level of one subprocess: a probe that could not answer is
// not an absence. This is the failure mode that would make the whole check
// worthless -- a machine with a stale credential AND a locked keychain reported
// as having neither.
func TestDoctorTreatsAnUnreadableKeychainAsUnknown(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubKeychain(t, false, cclink.KeychainItem{
		Service: "Claude Code-credentials",
		Account: "tester",
	}, lockedKeychain{})

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "warn" {
		t.Fatalf("keychain level = %q, want warn", got)
	}
	detail := report.detail(t, "keychain")
	if !strings.Contains(detail, "could not check") {
		t.Errorf("the detail does not say the check failed to run:\n%s", detail)
	}
	if !strings.Contains(detail, "locked") {
		t.Errorf("the detail does not carry the explanation the error offered:\n%s", detail)
	}
	// The sentence a clean machine gets must not appear here. Both branches are
	// warnings on a clean-looking report, so the level alone cannot tell them
	// apart -- what only one of them says is "no legacy item".
	if strings.Contains(detail, "no legacy") {
		t.Errorf("an unreadable keychain was reported as an empty one:\n%s", detail)
	}
}

// A failure with nothing to explain still has to reach the report rather than
// being swallowed into a clean answer.
func TestDoctorCarriesAKeychainErrorItCannotInterpret(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubKeychain(t, false, cclink.KeychainItem{Service: "Claude Code-credentials"},
		errors.New("something nobody has seen before"))

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "warn" {
		t.Fatalf("keychain level = %q, want warn", got)
	}
	if detail := report.detail(t, "keychain"); !strings.Contains(detail, "something nobody has seen before") {
		t.Fatalf("the underlying error did not reach the report:\n%s", detail)
	}
}

// doctor reports; it does not repair. The item is left where it is, and the
// report hands the user the command instead.
func TestDoctorDeletesNoKeychainItem(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	item := cclink.KeychainItem{Service: "Claude Code-credentials", Account: "tester"}
	var asked []string
	saved := keychainProbe
	t.Cleanup(func() { keychainProbe = saved })
	keychainProbe = func(context.Context) (cclink.KeychainLookup, error) {
		asked = append(asked, "probe")
		return cclink.KeychainLookup{Present: true, Item: item, Checked: []cclink.KeychainItem{item}}, nil
	}

	runDoctor(t)
	if len(asked) != 1 {
		t.Fatalf("the keychain was consulted %d times, want exactly one attribute lookup", len(asked))
	}
}

// The defect this file's newest checks were written for: `environment` printed
// "nothing set that would make a switch a no-op" while looking at two of the
// variables that make it one, so a machine with either of the other two got a
// diagnostic that lied on the single question a user runs doctor to answer.
func TestDoctorNamesEveryEnvironmentVariableThatDefeatsASwitch(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			seedHealthyMachine(t)
			t.Setenv(name, "the-live-credential")

			_, r, _ := runDoctor(t)
			detail := r.detail(t, "environment")
			if got := r.level(t, "environment"); got != "warn" {
				t.Errorf("environment = %q with %s set, want warn: %s", got, name, detail)
			}
			if !strings.Contains(detail, name) {
				t.Errorf("the environment check does not name %s: %s", name, detail)
			}
			// Same rule as the two variables that were already covered: this is
			// output a user pastes into an issue.
			if strings.Contains(detail, "the-live-credential") {
				t.Errorf("doctor printed a live credential: %s", detail)
			}
		})
	}
}

// An apiKeyHelper is one of the two sources that are not environment variables
// at all, so no widening of `environment` could ever have reached it. It
// displaces the login outright, which makes it the loudest of the five.
func TestDoctorReportsAnAPIKeyHelperThatDisplacesTheLogin(t *testing.T) {
	claude := isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(claude, "settings.json"),
		[]byte(`{"apiKeyHelper":"/usr/local/bin/get-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "api-key")
	if got := r.level(t, "api-key"); got != "warn" {
		t.Errorf("api-key = %q with an apiKeyHelper configured, want warn: %s", got, detail)
	}
	if !strings.Contains(detail, "apiKeyHelper") {
		t.Errorf("the api-key check does not name the helper: %s", detail)
	}
}

// The reason this check runs identity's resolver instead of listing the five
// sources it models. `ccdad switch` WRITES primaryApiKey for every api-key
// account, and it is the one source that does not displace an OAuth login — so
// a hazard list would warn about ccdad's own steady state, on the machine of
// every user who has an api-key account, forever.
func TestDoctorDoesNotWarnAboutTheKeyCcdadItselfStores(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(mustPath(ccpath.GlobalConfigPath()),
		[]byte(`{"primaryApiKey":"sk-ant-api-STORED"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	detail := r.detail(t, "api-key")
	if got := r.level(t, "api-key"); got != "ok" {
		t.Errorf("api-key = %q for the key ccdad itself stores, want ok: %s", got, detail)
	}
	if strings.Contains(detail, "sk-ant-api-STORED") {
		t.Errorf("doctor printed the stored key: %s", detail)
	}
	if code != ExitOK {
		t.Errorf("exit %d for an ordinary api-key account, want 0", code)
	}
}

// The one state where the answer depends on how Claude Code is started. doctor
// is asked about a session that has not started yet, so it reports the split
// rather than picking one of the two and being wrong half the time.
func TestDoctorReportsTheUnapprovedEnvironmentKeyBothWays(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-unapproved")

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "api-key")
	if !strings.Contains(detail, "claude -p") {
		t.Errorf("the api-key check does not say the two start modes disagree: %s", detail)
	}
	if strings.Contains(detail, "sk-ant-api03-unapproved") {
		t.Errorf("doctor printed a live key: %s", detail)
	}
}

// <store>/profiles/<uuid> holds an api-key account's primaryApiKey in its own
// Claude Code config. An orphan is therefore a credential that nothing else on
// the machine would ever mention again.
func TestDoctorReportsAProfileWhoseAccountIsGone(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(filepath.Join(root, ProfilesDirName, "uuid-gone"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	detail := r.detail(t, "profiles")
	if got := r.level(t, "profiles"); got != "warn" {
		t.Errorf("profiles = %q with an orphan, want warn: %s", got, detail)
	}
	if !strings.Contains(detail, "uuid-gone") {
		t.Errorf("the orphaned profile is not named: %s", detail)
	}
	if code != ExitOK {
		t.Errorf("exit %d for an orphaned profile, want 0", code)
	}
}

// This is what makes the check a set difference rather than a count of
// directories: a profile whose account still exists is legitimate persistent
// state, and reporting it would tell every `--full-profile` user that their
// working setup is a problem.
func TestDoctorDoesNotReportAProfileInDailyUse(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	// seedHealthyMachine stores uuid-a, so this profile has an account.
	if err := os.MkdirAll(filepath.Join(root, ProfilesDirName, "uuid-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Claude Code's legacy refresh lock is a directory named after its
	// neighbour with ".lock" appended, created BESIDE it. Counting it would
	// report an orphan for every profile in daily use.
	if err := os.MkdirAll(filepath.Join(root, ProfilesDirName, "uuid-a.lock"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "profiles"); got != "ok" {
		t.Errorf("profiles = %q for a profile whose account exists, want ok: %s", got, r.detail(t, "profiles"))
	}
}

// "I typed ccdad and got command not found" is the class doctor answers, and it
// now has a named remedy. It is never a FAILURE: ccdad invoked by its absolute
// path works exactly as well, which is also what lets the shared fixtures keep
// building their binary in a t.TempDir() that is by construction off PATH.
func TestDoctorSaysTheBinaryIsNotOnPath(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	exe := fakeBinary(t)
	stubExecutable(t, exe)
	t.Setenv("PATH", t.TempDir())

	code, r, _ := runDoctor(t)
	detail := r.detail(t, "path")
	if got := r.level(t, "path"); got != "warn" {
		t.Errorf("path = %q for a binary off PATH, want warn: %s", got, detail)
	}
	if !strings.Contains(detail, "setup-path") {
		t.Errorf("the path check does not name the remedy: %s", detail)
	}
	if code != ExitOK {
		t.Errorf("exit %d for a binary off PATH, want 0 — this is not a failure", code)
	}
}

func TestDoctorSaysTheBinaryIsOnPath(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	exe := fakeBinary(t)
	stubExecutable(t, exe)
	t.Setenv("PATH", filepath.Dir(exe))

	_, r, _ := runDoctor(t)
	if got := r.level(t, "path"); got != "ok" {
		t.Errorf("path = %q for a binary on PATH, want ok: %s", got, r.detail(t, "path"))
	}
}

// The two facts disagree in the ordinary case, which is why both are read:
// `ccdad setup-path` writes a block a NEW shell reads, and the process running
// doctor cannot see it. Reporting only the live PATH would tell a user to run
// the command they have already run.
func TestDoctorSeparatesARegisteredPathEntryFromALiveOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the registration is a registry value there; this writes an rc file")
	}
	isolate(t)
	seedHealthyMachine(t)
	exe := fakeBinary(t)
	stubExecutable(t, exe)
	t.Setenv("PATH", t.TempDir())

	rc := filepath.Join(mustPath(ccpath.Home()), ".bashrc")
	body := setupPathBegin + "\nexport PATH=\"" + filepath.Dir(exe) + ":$PATH\"\n" + setupPathEnd + "\n"
	if err := os.WriteFile(rc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "path")
	if got := r.level(t, "path"); got != "warn" {
		t.Errorf("path = %q, want warn: %s", got, detail)
	}
	if !strings.Contains(detail, rc) {
		t.Errorf("the path check does not name where the entry is registered: %s", detail)
	}
	if !strings.Contains(detail, "new shell") {
		t.Errorf("the path check does not offer the remedy that fits a registered entry: %s", detail)
	}
	if strings.Contains(detail, "setup-path` adds it") {
		t.Errorf("the path check told a user who has already run setup-path to run it again: %s", detail)
	}
}

// Without the account list there is no set to difference against, and answering
// "no orphans" out of a failed read would be exactly the lie this check exists
// to remove — it would report a machine full of stranded API keys as clean.
func TestDoctorRefusesToClearTheProfilesItCouldNotMatch(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(filepath.Join(root, ProfilesDirName, "uuid-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("not toml{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	if got := r.level(t, "profiles"); got != "fail" {
		t.Errorf("profiles = %q when the account list could not be read, want fail: %s", got, r.detail(t, "profiles"))
	}
	if code == ExitOK {
		t.Error("exit 0 while doctor could not answer the question it was asked")
	}
}

// Nothing else in doctor reads ~/.claude.json, so an unreadable one used to be
// invisible in the report. It is reported here — and the half of the answer
// that does NOT depend on the file is still given, because saying "cannot tell"
// for both halves would be the weaker diagnostic: every displacing source is
// visible without the file, and the one source the file carries never displaces.
func TestDoctorStillAnswersWithAnUnreadableClaudeConfig(t *testing.T) {
	broken := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(mustPath(ccpath.GlobalConfigPath()), []byte("{ this is not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a displacing source is still certain", func(t *testing.T) {
		claude := isolate(t)
		seedHealthyMachine(t)
		broken(t)
		if err := os.WriteFile(filepath.Join(claude, "settings.json"),
			[]byte(`{"apiKeyHelper":"/usr/local/bin/get-key"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		_, r, _ := runDoctor(t)
		detail := r.detail(t, "api-key")
		if got := r.level(t, "api-key"); got != "warn" {
			t.Errorf("api-key = %q, want warn: %s", got, detail)
		}
		if !strings.Contains(detail, "nothing reads") {
			t.Errorf("the check hedged on a verdict the unreadable file does not affect: %s", detail)
		}
	})

	t.Run("and so is its absence", func(t *testing.T) {
		isolate(t)
		seedHealthyMachine(t)
		broken(t)

		code, r, _ := runDoctor(t)
		detail := r.detail(t, "api-key")
		if got := r.level(t, "api-key"); got != "warn" {
			t.Errorf("api-key = %q, want warn: %s", got, detail)
		}
		if !strings.Contains(detail, "takes effect") {
			t.Errorf("the check does not say a switch survives, which the file cannot change: %s", detail)
		}
		// A config ccdad cannot read is not a broken machine.
		if code != ExitOK {
			t.Errorf("exit %d for an unreadable ~/.claude.json, want 0", code)
		}
	})
}

// The claude-version row, which is the whole point of the item this file's
// keychain remedy was blocked on. Every case goes through stubClaudeInstall,
// because the alternative -- letting the real probe read PATH -- makes the
// answer depend on which Claude Code the developer happens to have, and that is
// how the first run of the keychain test after this row landed failed.

// The ordinary machine. A version doctor could not name was the defect; naming
// it is the fix, and the method and path are named with it so a user who
// disagrees can see which file ccdad read.
func TestDoctorNamesTheInstalledClaudeCode(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, ccver.Install{
		Launcher: "/home/u/.local/bin/claude",
		Target:   "/home/u/.local/share/claude/versions/2.1.241",
		Method:   ccver.MethodNative,
		Version:  ccver.Version{Major: 2, Minor: 1, Patch: 241},
		Known:    true,
	}, nil)

	code, report, _ := runDoctor(t)
	if got := report.level(t, "claude-version"); got != "ok" {
		t.Fatalf("claude-version = %q, want ok: %s", got, report.detail(t, "claude-version"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0 — a current Claude Code is not a finding", code)
	}
	detail := report.detail(t, "claude-version")
	for _, want := range []string{"2.1.241", "native", "/home/u/.local/bin/claude"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not carry %q:\n%s", want, detail)
		}
	}
}

// The machine ccdad cannot serve. It is a FAILURE rather than a warning, and
// the exit code is asserted with the level for that reason: on such a build a
// switch is a silent no-op and `ccdad run`'s default scoping is ignored, so a
// report that came back green would be telling the user their machine is fine
// while nothing ccdad does reaches Claude Code.
func TestDoctorFailsOnAKeychainEraClaudeCode(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, claudeVersion(2, 1, 112), nil)

	code, report, _ := runDoctor(t)
	if got := report.level(t, "claude-version"); got != "fail" {
		t.Fatalf("claude-version = %q, want fail: %s", got, report.detail(t, "claude-version"))
	}
	if code != ExitFailure {
		t.Errorf("exit %d, want %d — only fail moves the exit code, and this is one", code, ExitFailure)
	}
	detail := report.detail(t, "claude-version")
	for _, want := range []string{
		"2.1.112",
		// Both defeats, because they bite on different platforms: the keychain
		// shadow only on macOS, the missing variable everywhere. A message that
		// named one would read as inapplicable to half the affected machines.
		"Keychain",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR",
		// What to do, and what still works meanwhile. A failure with no
		// remedy is the shape this file's keychain row was corrected from.
		"2.1.113",
		"--full-profile",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not carry %q:\n%s", want, detail)
		}
	}
}

// A launcher found and not classified is neither of the two answers above, and
// it must not be reported as either: a warn that read as "fine" would hide it,
// and a fail would make an unclassifiable install look like a broken one.
func TestDoctorWarnsWhenItCannotNameTheVersion(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, ccver.Install{
		Launcher: "/opt/weird/claude",
		Method:   ccver.MethodUnknown,
		Why:      "/opt/weird/claude is neither a symlink into a claude/versions directory nor an npm install",
	}, nil)

	code, report, _ := runDoctor(t)
	if got := report.level(t, "claude-version"); got != "warn" {
		t.Fatalf("claude-version = %q, want warn: %s", got, report.detail(t, "claude-version"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0 — ccdad not being able to classify an install is not the install being broken", code)
	}
	if detail := report.detail(t, "claude-version"); !strings.Contains(detail, "/opt/weird/claude") {
		t.Errorf("the detail does not say what it could not read:\n%s", detail)
	}
}

// No claude at all is its own row, and it is not a failure: ccdad is installed
// before Claude Code on some machines, and a fresh one must not read as broken —
// the judgement checkStore makes about a store that is not there yet.
func TestDoctorWarnsWhenThereIsNoClaudeCode(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	code, report, _ := runDoctor(t)
	if got := report.level(t, "claude-version"); got != "warn" {
		t.Fatalf("claude-version = %q, want warn: %s", got, report.detail(t, "claude-version"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0", code)
	}
	// The level is NOT the behaviour here, and asserting it alone is a test that
	// cannot fail: "there is no claude" and "the probe itself errored" are both
	// warnings, and the second's message ("ccdad could not look for a claude
	// launcher: ...") describes a broken probe rather than a machine with no
	// Claude Code on it. Only the sentence tells them apart.
	detail := report.detail(t, "claude-version")
	if !strings.Contains(detail, "on PATH") {
		t.Errorf("the detail does not say where it looked:\n%s", detail)
	}
	if strings.Contains(detail, "could not look for") {
		t.Errorf("a machine with no Claude Code was reported as a failed probe:\n%s", detail)
	}
}

// The remedy INVERTS across 2.1.113, and this is the assertion that pins it.
// Both directions are asserted positively and negatively: the two branches are
// both `warn` and both name the item and the command, so a test that only
// checked the level, or only checked that its own sentence was present, would
// pass against the branches swapped.
func TestDoctorPicksTheKeychainRemedyForTheEraTheMachineIsOn(t *testing.T) {
	for _, tc := range []struct {
		name          string
		install       ccver.Install
		want, notWant string
		// pinsVersion says whether asserting the version string proves the
		// remedy quotes the install ccdad READ. It is per-row because one
		// branch's own static prose contains a version number, and there an
		// assertion on it matches that prose and passes with the install
		// dropped entirely -- a test that cannot fail.
		pinsVersion bool
	}{
		{
			name:        "keychain era: the item is the live login and deleting it undoes itself",
			install:     claudeVersion(2, 1, 112),
			want:        "Do NOT delete it",
			notWant:     "nothing is broken right now",
			pinsVersion: true,
		},
		{
			name:        "a current release: nothing reads it, so removing it is cleanup",
			install:     claudeVersion(2, 1, 241),
			want:        "nothing is broken right now",
			notWant:     "Do NOT delete it",
			pinsVersion: true,
		},
		{
			// The boundary release itself is on the modern side. The version
			// is NOT pinned here: this branch says "2.1.113 removed that
			// backend" in its own words, so the assertion would match static
			// text. Which branch was chosen is what this row is for.
			name:        "the boundary release itself is treated as modern",
			install:     claudeVersion(2, 1, 113),
			want:        "nothing is broken right now",
			notWant:     "Do NOT delete it",
			pinsVersion: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedHealthyMachine(t)
			stubClaudeInstall(t, tc.install, nil)
			stubKeychain(t, true, cclink.KeychainItem{
				Service: "Claude Code-credentials-aa3d8c96",
				Account: "tester",
			}, nil)

			_, report, _ := runDoctor(t)
			detail := report.detail(t, "keychain")
			if !strings.Contains(detail, tc.want) {
				t.Errorf("the detail does not carry %q:\n%s", tc.want, detail)
			}
			if strings.Contains(detail, tc.notWant) {
				t.Errorf("the detail carries the OTHER era's remedy %q:\n%s", tc.notWant, detail)
			}
			// The version is named in the remedy itself. "An older Claude Code"
			// is not actionable; the number ccdad read is.
			if tc.pinsVersion && !strings.Contains(detail, tc.install.Version.String()) {
				t.Errorf("the remedy does not say which Claude Code it applies to:\n%s", detail)
			}
		})
	}
}

// The keychain-era remedy leads with the cost and does NOT lead with the
// command, which is the property the row was corrected for once already. On this
// machine the deletion destroys the live login and reverts within hours, so
// anyone who reads far enough to copy the invocation has necessarily read that
// first.
func TestTheKeychainEraRemedyNamesItsCostBeforeItsCommand(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, claudeVersion(2, 1, 100), nil)
	stubKeychain(t, true, cclink.KeychainItem{Service: "Claude Code-credentials-aa3d8c96", Account: "tester"}, nil)

	_, report, _ := runDoctor(t)
	detail := report.detail(t, "keychain")
	cost, cmd := strings.Index(detail, "Do NOT delete it"), strings.Index(detail, "delete-generic-password")
	if cost < 0 || cmd < 0 {
		t.Fatalf("the detail is missing the caveat or the command:\n%s", detail)
	}
	if cost > cmd {
		t.Errorf("the command is offered before its caveat is named:\n%s", detail)
	}
}

// A probe that could not LOOK is not a probe that looked and found nothing, and
// doctor words them differently. The state is reachable rather than defensive:
// ccpath.StoreHome returns from CCDAD_HOME without ever consulting the home
// directory, so runChecks gets past its opening guard on a machine that has
// CCDAD_HOME set and no resolvable HOME — and there ccver cannot even spell the
// two fallback launcher paths, let alone stat them.
func TestDoctorSaysWhenItCouldNotLookForAClaudeLauncher(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, ccver.Install{}, errors.New("ccdad cannot tell where your home directory is"))

	code, report, _ := runDoctor(t)
	if got := report.level(t, "claude-version"); got != "warn" {
		t.Fatalf("claude-version = %q, want warn: %s", got, report.detail(t, "claude-version"))
	}
	if code != ExitOK {
		t.Errorf("exit %d, want 0", code)
	}
	detail := report.detail(t, "claude-version")
	if !strings.Contains(detail, "could not look for") {
		t.Errorf("a failed search was reported as a completed one:\n%s", detail)
	}
	// The negative half, and the one that matters: doctor must not claim it
	// searched ~/.local/bin and ~/.claude/local when it never resolved either.
	if strings.Contains(detail, "on PATH") {
		t.Errorf("doctor asserted a negative result for a search it did not perform:\n%s", detail)
	}
}
