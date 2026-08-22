package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
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
	for _, name := range []string{"store", "permissions", "locks", "pidfile", "status-file", "usage-cache", "engine-state", "config", "sessions", "claude-code", "credential-keys", "environment"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("the human report does not mention the %s check:\n%s", name, stdout)
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
