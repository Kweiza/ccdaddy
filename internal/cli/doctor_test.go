package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/theme"
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
	writeLiveFile(t, liveLoginJSON("RT-uuid-a", ""))
}

// The same rule the daemon singleton probe follows, one layer up: creating the
// lock file while checking for it destroys the one piece of genuine evidence
// that no daemon ever started here. store.Open would create the store directory
// too, which is why doctor does not use it — a diagnostic that manufactures
// what it reports on is worthless.
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

// Naming a store that points at nothing is doctor's job: it is a configuration
// question, and the singleton answers "not running" for it on purpose rather
// than "cannot determine".
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
	for _, name := range []string{"permissions", "pidfile", "usage-cache", "history", "engine-state", "config", "sessions"} {
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

// The unknown-key probe's "on startup" half — the only part of it that was
// still missing. Six machine keys drifted in after clauth's one-key list was
// written, so this is demonstrated drift rather than a hypothetical.
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
		t.Skip("chmod is a no-op on Windows")
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
		t.Skip("chmod is a no-op on Windows")
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
		t.Skip("chmod is a no-op on Windows")
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

// A machine with no series yet is a healthy machine, and saying otherwise would
// warn on every install for the first hours of its life: history.json appears
// only once a daemon has polled, and nothing else in the tree ever creates it.
// So absent and damaged are different verdicts here, which is the distinction
// history.LoadHistory already draws for its own callers — an absent file is an
// empty series and no error, a damaged one keeps its reason.
//
// The second assertion is this file's standing rule applied to the newest row:
// a probe that brought history.json into existence while checking for it would
// destroy the evidence that no daemon has ever recorded anything here.
func TestDoctorTreatsAnAbsentHistoryAsHealthy(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "history"); got != "ok" {
		t.Errorf("history = %q on a machine that has never polled, want ok: %s", got, r.detail(t, "history"))
	}
	if _, err := os.Stat(filepath.Join(mustPath(ccpath.StoreHome()), "history.json")); !os.IsNotExist(err) {
		t.Errorf("doctor created the series it was asked to report on: %v", err)
	}
}

// A series that cannot be parsed is invisible on every other surface: the
// forecast simply reports no measured rate, which is also exactly what a fresh
// machine reports. doctor is the only place the two are ever told apart, and
// the level is a warning rather than a failure because nothing is broken —
// the next write replaces the file and measurement resumes.
func TestDoctorReportsACorruptHistory(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	if err := os.WriteFile(filepath.Join(mustPath(ccpath.StoreHome()), "history.json"), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "history"); got != "warn" {
		t.Errorf("history = %q, want warn: %s", got, r.detail(t, "history"))
	}
}

// A document that cannot be READ at all splits the two rows apart: the series
// fails and the usage cache only warns, on one and the same breakage.
//
// That is deliberate and it is the opposite of what the two do on a document
// that cannot be PARSED, where both warn. The difference is what the loss costs.
// usage.LoadCache degrades an unreadable cache to an empty one and reports it
// through LoadError, because the next poll rewrites the file and every account
// reads as unknown in the meantime — recoverable without a human. history's
// WithHistory reloads before it writes and returns that error instead of saving,
// so an unreadable history.json is never replaced by the next poll: it stops
// accumulating until somebody moves it aside, and no other surface would ever
// say so. A reader of these two rows will notice the asymmetry, so it is pinned
// here rather than left to look like an oversight in whichever row they read
// second.
//
// A directory where the file belongs, rather than chmod 0000: it is unreadable
// for root as well, and containerised CI runs as root.
func TestDoctorFailsOnAnUnreadableHistoryAndWarnsOnAnUnreadableCache(t *testing.T) {
	for _, tc := range []struct {
		file, check, level string
		code               ExitCode
	}{
		{"history.json", "history", "fail", ExitFailure},
		{"usage.json", "usage-cache", "warn", ExitOK},
	} {
		t.Run(tc.file, func(t *testing.T) {
			isolate(t)
			seedHealthyMachine(t)
			if err := os.Mkdir(filepath.Join(mustPath(ccpath.StoreHome()), tc.file), 0o700); err != nil {
				t.Fatal(err)
			}

			code, r, _ := runDoctor(t)
			if got := r.level(t, tc.check); got != tc.level {
				t.Errorf("%s = %q, want %q: %s", tc.check, got, tc.level, r.detail(t, tc.check))
			}
			if code != tc.code {
				t.Errorf("doctor exited %v on an unreadable %s, want %v", code, tc.file, tc.code)
			}
		})
	}
}

// Claude Code changing these internals between releases is the risk doctor
// exists against. A credentials file ccdad cannot parse is the loudest form of
// that, and switch deliberately refuses to repair one.
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
	for _, name := range []string{"store", "path", "permissions", "locks", "pidfile", "status-file", "usage-cache", "history", "engine-state", "config", "update-check", "sessions", "profiles", "primary-accounts", "credential-files", "codex-relogin", "codex-proxy", "codex-shim", "credential-home", "claude-version", "claude-code", "credential-keys", "keychain", "environment", "api-key", "oauth-source"} {
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

// Whether doctor should repair or only report is undecided, so it ships
// report-only. A repair would have to be a deliberate act behind a flag, and
// there is no flag.
func TestDoctorRepairsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows")
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

// A broken config.toml is ignored SILENTLY by the engine — config.Reloader
// hands back the last config that parsed rather than stopping the daemon — so
// doctor is where a user finds out it has been doing nothing.
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

// Unattended credit spend is a high-severity risk, and doctor is where a user
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
		t.Skip("chmod is a no-op on Windows")
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

// The same rule the daemon singleton probe follows, at the level of one
// subprocess: a probe that could not answer is not an absence. This is the
// failure mode that would make the whole check worthless -- a machine with a
// stale credential AND a locked keychain reported as having neither.
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
	if !strings.Contains(detail, "could not read") {
		t.Errorf("the detail does not say the check failed to run:\n%s", detail)
	}
	if !strings.Contains(detail, "locked") {
		t.Errorf("the detail does not carry the explanation the error offered:\n%s", detail)
	}
	// The sentence a clean machine gets must not appear here. Both branches are
	// warnings on a clean-looking report, so the level alone cannot tell them
	// apart -- what only one of them says is "no item".
	if strings.Contains(detail, "no item for") {
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
	// It skips rather than failing on its own account, and that is not a
	// softening: the cause is one document, and it is named once at fail level
	// on the accounts-file row that gates this one. What this test forbids is
	// unchanged — an "ok" here would say every profile is matched out of a read
	// that never happened — and the exit code is still 1.
	if got := r.level(t, "profiles"); got != string(levelSkipped) {
		t.Errorf("profiles = %q when the account list could not be read, want skipped: %s", got, r.detail(t, "profiles"))
	}
	if got := r.level(t, "accounts-file"); got != string(levelFail) {
		t.Errorf("accounts-file = %q for an accounts.toml that does not parse, want fail: %s",
			got, r.detail(t, "accounts-file"))
	}
	if !strings.Contains(r.detail(t, "profiles"), "accounts-file") {
		t.Errorf("the skipped profiles row does not point at the row carrying the cause: %s", r.detail(t, "profiles"))
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
func TestDoctorFailsOnAClaudeCodeThatCannotScope(t *testing.T) {
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
		// The variable, and ONLY the variable. This row used to name a keychain
		// shadow as the other defeat of this era; every release has that
		// shadow, so it was never what put 2.1.112 on the far side of anything.
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

// The answer does not depend on the release any more, and this is what that
// costs to assert: three versions spanning the old boundary, one answer. The
// row used to hand out opposite remedies on either side of 2.1.113 -- "do NOT
// delete it" below, "removing it is cleanup" above -- and the split rested on a
// backend removal that never happened.
func TestDoctorReportsTheSameKeychainAnswerOnEveryRelease(t *testing.T) {
	for _, v := range []ccver.Install{
		claudeVersion(2, 1, 112),
		claudeVersion(2, 1, 113),
		claudeVersion(2, 1, 251),
	} {
		t.Run(v.Version.String(), func(t *testing.T) {
			isolate(t)
			seedHealthyMachine(t)
			stubClaudeInstall(t, v, nil)
			stubKeychain(t, true, cclink.KeychainItem{
				Service: "Claude Code-credentials-aa3d8c96",
				Account: "tester",
			}, nil)

			_, report, _ := runDoctor(t)
			detail := report.detail(t, "keychain")
			if !strings.Contains(detail, "live login") {
				t.Errorf("the detail does not say the item is the login:\n%s", detail)
			}
			for _, gone := range []string{"nothing is broken right now", "Do NOT delete it", "delete-generic-password"} {
				if strings.Contains(detail, gone) {
					t.Errorf("the per-era remedy %q survives:\n%s", gone, detail)
				}
			}
		})
	}
}

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

// A token a session host injected at a path compiled into Claude Code
// outranks the login, and there is NO VARIABLE to unset. A model built on an
// environment variable named CCR_OAUTH_TOKEN_FILE would never fire on this
// machine.
func TestDoctorReportsTheHostInjectedTokenFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	writeFile(t, identity.HostOAuthTokenFile, "sk-ant-oat-INJECTED")

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "oauth-source")
	if got := r.level(t, "oauth-source"); got != "warn" {
		t.Fatalf("oauth-source = %q, want warn: %s", got, detail)
	}
	for _, want := range []string{identity.HostOAuthTokenFile, "check the host session", "no variable to unset"} {
		if !strings.Contains(detail, want) {
			t.Errorf("oauth-source does not carry %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "sk-ant-oat-INJECTED") {
		t.Errorf("doctor printed the injected token:\n%s", detail)
	}
	// The environment row must NOT claim this one: it has no variable, and the
	// row's OK sentence is about variables.
	if got := r.level(t, "environment"); got != "ok" {
		t.Errorf("environment = %q, want ok — nothing is SET on this machine: %s", got, r.detail(t, "environment"))
	}
}

// The division of labour between the two rows, pinned. Without the word
// "variable" in the environment row's OK sentence, that row keeps saying
// "nothing set that would make a switch a no-op" on a machine whose live
// credential is a host-injected file — the same lie the hazard list was widened
// to stop telling, in a new place.
func TestEnvironmentDoesNotClaimMoreThanItChecked(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	writeFile(t, identity.HostOAuthTokenFile, "sk-ant-oat-INJECTED")

	_, r, _ := runDoctor(t)
	env := r.detail(t, "environment")
	if !strings.Contains(env, "no environment variable set") {
		t.Errorf("the environment row claims more than it looked at:\n%s", env)
	}
	if got := r.level(t, "oauth-source"); got != "warn" {
		t.Errorf("oauth-source = %q, want warn — it is the row that answers this", got)
	}
}

// An empty host file is not a credential. Claude Code reads and trims, so zero
// bytes give it nothing — and an in-progress write must not be reported as a
// live token.
func TestAnEmptyHostTokenFileIsNotACredential(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	writeFile(t, identity.HostOAuthTokenFile, "")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "oauth-source"); got != "ok" {
		t.Errorf("oauth-source = %q for a zero-byte host file, want ok: %s", got, r.detail(t, "oauth-source"))
	}
}

// THE GAP THIS ITEM FOUND IN ALREADY-SHIPPED CODE. ccdad modelled
// CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR and not the well-known file behind it, so
// a machine with the file and no variable had a key that displaces the login
// and a report that said nothing about it.
func TestDoctorReportsAHostInjectedAPIKeyWithNoVariableSet(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	writeFile(t, identity.HostAPIKeyFile, "sk-ant-api03-INJECTED")

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "api-key")
	if got := r.level(t, "api-key"); got == "ok" {
		t.Fatalf("api-key = ok on a machine with a host-injected key: %s", detail)
	}
	if !strings.Contains(detail, identity.HostAPIKeyFile) {
		t.Errorf("the api-key row does not name the file that resolved:\n%s", detail)
	}
	if strings.Contains(detail, "sk-ant-api03-INJECTED") {
		t.Errorf("doctor printed the injected key:\n%s", detail)
	}
}

// A login object whose scopes do not carry user:inference is not a credential
// to Claude Code, and saying only "no OAuth credential resolves" would read as
// a bug in ccdad rather than as the actionable fact that the user has to sign
// in again.
func TestDoctorSaysAScopelessLoginResolvesNothing(t *testing.T) {
	isolate(t)
	seedAccount(t, "uuid-a", "work@example.com")
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-uuid-a","scopes":["org:create_api_key","user:profile"]}}`)

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "oauth-source")
	if got := r.level(t, "oauth-source"); got != "warn" {
		t.Fatalf("oauth-source = %q, want warn: %s", got, detail)
	}
	if !strings.Contains(detail, "user:inference") {
		t.Errorf("oauth-source does not name the scope that decides it:\n%s", detail)
	}
}

// The bg-auth snapshot is the one state ccdad declines on — and it must neither
// read the file nor delete it, which is what Claude Code's own reader does.
func TestABgAuthSnapshotMakesTheAnswerADecline(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	snapshot := filepath.Join(t.TempDir(), "snapshot.json")
	const body = `{"accessToken":"sk-ant-oat-SNAPSHOT"}`
	writeFile(t, snapshot, body)
	t.Setenv("CLAUDE_BG_AUTH_SNAPSHOT_PATH", snapshot)

	_, r, _ := runDoctor(t)
	detail := r.detail(t, "oauth-source")
	if got := r.level(t, "oauth-source"); got != "warn" {
		t.Fatalf("oauth-source = %q, want warn: %s", got, detail)
	}
	if strings.Contains(detail, "sk-ant-oat-SNAPSHOT") {
		t.Errorf("doctor printed the snapshot's token:\n%s", detail)
	}
	got, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("doctor deleted the snapshot Claude Code has not consumed yet: %v", err)
	}
	if string(got) != body {
		t.Errorf("the snapshot changed under doctor: %q", got)
	}
}

// The new probes stat two paths outside the home directory. Neither they nor
// the directory above them may be created.
func TestDoctorCreatesNeitherHostFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	runDoctor(t)
	for _, path := range []string{
		identity.HostOAuthTokenFile,
		identity.HostAPIKeyFile,
		filepath.Dir(identity.HostOAuthTokenFile),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("doctor created %s (err=%v)", path, err)
		}
	}
}

// CLAUDE_CODE_SIMPLE=0 is not bare mode. Claude Code parses it with a
// four-spelling truthiness test, and reading it as "any non-empty string" put
// ccdad in a mode where the answer is "no credential" on a machine that has one.
func TestCLAUDECODESIMPLEZeroIsNotBareMode(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	t.Setenv("CLAUDE_CODE_SIMPLE", "0")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "oauth-source"); got != "ok" {
		t.Fatalf("oauth-source = %q with CLAUDE_CODE_SIMPLE=0, want ok: %s", got, r.detail(t, "oauth-source"))
	}
	if !strings.Contains(r.detail(t, "oauth-source"), "login in the credentials file") {
		t.Errorf("oauth-source does not name the login:\n%s", r.detail(t, "oauth-source"))
	}
}

// primary turns the credit ceiling off for one account and nothing a person
// routinely reads says so out loud — it has no column, no status line, and one
// parenthesis on a listing. doctor is where a seat somebody armed months ago is
// found without opening accounts.toml.
func TestDoctorNamesThePrimaryAccounts(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "primary-accounts"); got != "ok" {
		t.Fatalf("primary-accounts = %q on a machine with none, want ok", got)
	}
	if detail := r.detail(t, "primary-accounts"); !strings.Contains(detail, "no account") {
		t.Errorf("detail = %q, want it to say there are none", detail)
	}

	seedPrimaryCreditAccount(t, "u-seat", "seat@example.com")
	code, r, _ := runDoctor(t)
	if code != ExitOK {
		t.Fatalf("doctor = %d, want 0 — an armed seat is a choice, not a fault", code)
	}
	detail := r.detail(t, "primary-accounts")
	if !strings.Contains(detail, "seat@example.com") {
		t.Errorf("detail = %q, want it to name the primary account", detail)
	}
	if !strings.Contains(detail, "max_auto_spend") {
		t.Errorf("detail = %q, want it to say the credit ceiling does not hold the account back", detail)
	}
}

// The rows that read the account list go through store.AccountsAt rather than
// store.Open, and THIS is the state that tells the two apart: a store root that
// is there with no credentials directory under it. Open would MkdirAll one and
// chmod both, quietly repairing the damage the report was asked about, while
// AccountsAt reads accounts.toml and creates nothing.
//
// TestDoctorCreatesNothing cannot reach this: it points CCDAD_HOME at a
// directory that does not exist, so every store-reading row is skipped before
// it can create anything and an Open in one of them is never executed.
func TestDoctorDoesNotRepairTheStoreItIsReportingOn(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	seedPrimaryCreditAccount(t, "u-seat", "seat@example.com")
	root := mustPath(ccpath.StoreHome())
	// A profile directory, so the profiles row reaches its own read of the
	// account list rather than stopping at the missing container. Both rows
	// take the same route for the same reason, and only one of them is new.
	if err := os.MkdirAll(filepath.Join(root, ProfilesDirName, "uuid-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(root, "credentials")
	if err := os.RemoveAll(creds); err != nil {
		t.Fatal(err)
	}

	runDoctor(t)

	if _, err := os.Stat(creds); !os.IsNotExist(err) {
		t.Fatalf("doctor re-created the credentials directory it was reporting on: %v", err)
	}
}

// The orphan credential file: one written by a build older than the rollback
// journal, or left by a rollback whose os.Remove was refused and whose error
// the user did not act on. It holds a live refresh token at 0600 under
// credentials/, and nothing on the machine can find it — `ccdad status`, `ccdad
// remove` and doctor's own account rows all read accounts.toml, and an orphan
// is by definition a uuid the document does not carry.
//
// The path is spelled out here rather than taken from store's accessor on
// purpose: a test that asked production for the directory would pass while
// checking nothing if that directory ever moved. checkPermissions's glob makes
// the same argument.
func TestDoctorNamesACredentialFileNoAccountNames(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	leaked := filepath.Join(root, "credentials", "uuid-gone.json")
	if err := os.WriteFile(leaked, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-files"); got != string(levelWarn) {
		t.Fatalf("level = %q, want warn: %s", got, r.detail(t, "credential-files"))
	}
	d := r.detail(t, "credential-files")
	if !strings.Contains(d, "uuid-gone") {
		t.Errorf("detail does not name the orphaned uuid: %s", d)
	}
	if !strings.Contains(d, leaked) {
		t.Errorf("detail does not give the path of the file holding the token: %s", d)
	}
}

// A machine whose credential files are all accounted for says so, rather than
// staying silent: the row has to be readable as evidence, and a check that only
// speaks up when something is wrong cannot be distinguished from one that is
// not running.
func TestDoctorSaysSoWhenEveryCredentialFileIsNamed(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-files"); got != string(levelOK) {
		t.Errorf("level = %q, want ok on a store with no orphan: %s", got, r.detail(t, "credential-files"))
	}
}

// doctor reports; it does not repair. A file holding a live token is exactly
// where a user wants to look before anything else does, and deleting a file
// the store cannot explain is not ccdad's call to make on its own initiative.
func TestDoctorDoesNotDeleteAnOrphanedCredentialFile(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	leaked := filepath.Join(root, "credentials", "uuid-gone.json")
	if err := os.WriteFile(leaked, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runDoctor(t)
	if _, err := os.Stat(leaked); err != nil {
		t.Errorf("doctor removed the credential file it was asked to report on: %v", err)
	}
}

// A store that is not there is a fresh machine, not a broken one — the
// judgement checkStore makes and every store-derived row repeats.
func TestDoctorSkipsTheCredentialFilesRowWithNoStore(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "never-created"))

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-files"); got != string(levelSkipped) {
		t.Errorf("level = %q, want skipped when there is no store: %s", got, r.detail(t, "credential-files"))
	}
}

// A row that answers out of a read it could not perform is worse than no row.
// accounts.toml is what says which uuids are accounted for, so a document that
// cannot be parsed leaves this question unanswerable — and "every credential
// file belongs to an account" is the one answer here that must never be
// guessed. checkProfiles and checkPrimary refuse the same way.
func TestDoctorWillNotClearTheCredentialFilesRowFromADamagedDocument(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("this is not toml"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)
	// Skipped, with the failure on the row that owns the cause. The forbidden
	// answer is the reassuring one -- "every stored credential file belongs to
	// an account this store still has", said out of a document that did not
	// parse -- and it is still forbidden.
	if got := r.level(t, "credential-files"); got != string(levelSkipped) {
		t.Errorf("level = %q, want skipped from an accounts.toml that cannot be read: %s",
			got, r.detail(t, "credential-files"))
	}
	if got := r.level(t, "accounts-file"); got != string(levelFail) {
		t.Errorf("accounts-file = %q, want fail from an accounts.toml that cannot be read: %s",
			got, r.detail(t, "accounts-file"))
	}
	if code == ExitOK {
		t.Error("exit 0 while the account list could not be read at all")
	}
}

// The count and the words around it are built by hand out of six Sprintf
// arguments, and "1 credential files belong" is the kind of thing that makes a
// reader stop believing the rest of the sentence — plural's own comment says
// so. Both arms are pinned because only the pair of them catches a transposed
// argument: one arm alone reads correctly for the count it was written against.
func TestDoctorCountsOneOrphanedCredentialFileGrammatically(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	dir := filepath.Join(mustPath(ccpath.StoreHome()), "credentials")
	if err := os.WriteFile(filepath.Join(dir, "uuid-gone.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	d := r.detail(t, "credential-files")
	for _, want := range []string{"1 credential file belongs", "does not name it", "Delete it"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail does not read %q for one file: %s", want, d)
		}
	}
}

func TestDoctorCountsSeveralOrphanedCredentialFilesGrammatically(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	dir := filepath.Join(mustPath(ccpath.StoreHome()), "credentials")
	for _, name := range []string{"uuid-x.json", "uuid-y.json", "uuid-z.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, r, _ := runDoctor(t)
	d := r.detail(t, "credential-files")
	for _, want := range []string{"3 credential files belong", "does not name them", "Delete them"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail does not read %q for three files: %s", want, d)
		}
	}
}

// A deleted accounts.toml used to read as a store with no accounts, and that
// was the charitable description of it. Every row that takes a set difference
// against the account list read the empty list as the truth, so the
// credential-files row turned every login on the machine into an orphan and
// ended its sentence with "Delete them once you have looked". This is the test
// that ccdad no longer tells a user whose document was deleted to destroy the
// only copies of the logins they can still recover from.
func TestDoctorNamesADeletedAccountsFileRatherThanCallingEveryLoginALeak(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.Remove(filepath.Join(root, "accounts.toml")); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)

	if got := r.level(t, "accounts-file"); got != string(levelFail) {
		t.Fatalf("accounts-file = %q for a deleted document with credentials still beside it, want fail: %s",
			got, r.detail(t, "accounts-file"))
	}
	detail := r.detail(t, "accounts-file")
	for _, want := range []string{"accounts.toml", "uuid-a.json", "Do NOT delete", "ccdad import"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the accounts-file detail does not read %q: %s", want, detail)
		}
	}

	if got := r.level(t, "credential-files"); got != string(levelSkipped) {
		t.Errorf("credential-files = %q with no account list to match against, want skipped: %s",
			got, r.detail(t, "credential-files"))
	}
	// The whole point, and it is asserted across the WHOLE report rather than on
	// the one row: the destructive remedy has to be absent everywhere, not
	// moved. "once you have looked" is the tail of checkCredentialFiles's
	// "Delete them once you have looked", matched instead of the bare verb
	// because the accounts-file row above legitimately says "Do NOT delete".
	for _, c := range runChecks() {
		if strings.Contains(c.Detail, "once you have looked") {
			t.Errorf("%s still tells the user to delete a credential file after the document was deleted: %s",
				c.Name, c.Detail)
		}
	}
	if code == ExitOK {
		t.Error("exit 0 while ccdad had lost its entire account list")
	}
}

// The gate is "is the account list the truth", not "does the file exist", and
// the difference is every fresh install. A machine nobody has added an account
// to has no document either, and its empty list IS the truth — so the three
// rows below must go on answering there rather than turning into "skipped" on
// every new machine.
func TestDoctorDoesNotCallAFreshStoreADeletedOne(t *testing.T) {
	isolate(t)
	root := mustPath(ccpath.StoreHome())
	if err := os.MkdirAll(filepath.Join(root, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, r, _ := runDoctor(t)

	if got := r.level(t, "accounts-file"); got != string(levelOK) {
		t.Errorf("accounts-file = %q on a store nothing has been added to, want ok: %s",
			got, r.detail(t, "accounts-file"))
	}
	if got := r.detail(t, "accounts-file"); !strings.Contains(got, "nothing has been added on this machine") {
		t.Errorf("the accounts-file detail does not say the store is simply new: %s", got)
	}
	for _, name := range []string{"profiles", "primary-accounts", "credential-files"} {
		if got := r.level(t, name); got == string(levelSkipped) {
			t.Errorf("%s = skipped on a fresh machine, want a real answer: %s", name, r.detail(t, name))
		}
	}
	if code != ExitOK {
		t.Errorf("exit %d on a store that is merely new", code)
	}
}

// Both arms of both plurals, because one arm alone reads correctly for the
// count it was written against and a transposed pair is invisible until a user
// with exactly one account reads "1 credential files sit". plural's own comment
// is about this, and the last four guards in this file that went unkilled by
// mutation were all of this shape.
func TestDoctorCountsTheAccountListAndTheStrandedFilesGrammatically(t *testing.T) {
	t.Run("one of each", func(t *testing.T) {
		isolate(t)
		seedAccount(t, "uuid-a", "work@example.com")
		_, r, _ := runDoctor(t)
		if got := r.detail(t, "accounts-file"); !strings.Contains(got, "names 1 account") ||
			strings.Contains(got, "names 1 accounts") {
			t.Errorf("the accounts-file detail miscounts one account: %s", got)
		}

		if err := os.Remove(filepath.Join(mustPath(ccpath.StoreHome()), "accounts.toml")); err != nil {
			t.Fatal(err)
		}
		_, r, _ = runDoctor(t)
		if got := r.detail(t, "accounts-file"); !strings.Contains(got, "1 credential file still sits") {
			t.Errorf("the accounts-file detail miscounts one stranded file: %s", got)
		}
	})

	t.Run("two of each", func(t *testing.T) {
		isolate(t)
		seedAccount(t, "uuid-a", "work@example.com")
		seedAccount(t, "uuid-b", "home@example.com")
		_, r, _ := runDoctor(t)
		if got := r.detail(t, "accounts-file"); !strings.Contains(got, "names 2 accounts") {
			t.Errorf("the accounts-file detail miscounts two accounts: %s", got)
		}

		if err := os.Remove(filepath.Join(mustPath(ccpath.StoreHome()), "accounts.toml")); err != nil {
			t.Fatal(err)
		}
		_, r, _ = runDoctor(t)
		if got := r.detail(t, "accounts-file"); !strings.Contains(got, "2 credential files still sit") {
			t.Errorf("the accounts-file detail miscounts two stranded files: %s", got)
		}
	})
}

// The row exists because the two spellings are not interchangeable and nothing
// else in the tree ever says which one a machine has. CLAUDE_PLUGIN_ROOT is set
// by Claude Code for a plugin-launched server and by nothing else, so when this
// row is produced by `ccdad doctor` running as an MCP tool it answers the
// question the caller actually has: what are my own tools called.
func TestDoctorSaysTheToolsArePluginSpeltWhenThePluginLaunchedThisCcdad(t *testing.T) {
	isolate(t)
	t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())

	got := checkMCPTools()
	if got.Level != levelOK {
		t.Errorf("level = %q, want ok; this row reports and never judges", got.Level)
	}
	if !strings.Contains(got.Detail, "mcp__plugin_ccdad_ccdad__") {
		t.Errorf("the row does not name the plugin spelling:\n%s", got.Detail)
	}
	if strings.Contains(got.Detail, "no ccdad plugin is installed") {
		t.Errorf("the row fell through to the registry while running AS the plugin:\n%s", got.Detail)
	}
}

// The ordinary shell, on a machine with no plugin: the answer is the bare
// spelling, and it is the one a user writing a permission rule needs.
func TestDoctorNamesTheBareToolSpellingWhenNoPluginIsInstalled(t *testing.T) {
	isolate(t)
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")

	got := checkMCPTools()
	if got.Level != levelOK {
		t.Errorf("level = %q, want ok", got.Level)
	}
	if !strings.Contains(got.Detail, "mcp__ccdad__") {
		t.Errorf("the row does not name the bare spelling:\n%s", got.Detail)
	}
	if strings.Contains(got.Detail, "mcp__plugin_ccdad_ccdad__") {
		t.Errorf("the row claims the plugin spelling on a machine with no plugin:\n%s", got.Detail)
	}
}

// A plugin installed and this ccdad NOT launched by it -- the shell case that
// matters, because it is where somebody is about to run `ccdad mcp install`
// and rename every tool they have written a rule for.
func TestDoctorWarnsInProseThatInstallingWouldRenameThePluginsTools(t *testing.T) {
	claude := isolate(t)
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	writeInstalledPlugins(t, claude,
		`{"version":2,"plugins":{"ccdad@my-own-mirror":[{"scope":"user"}]}}`)

	got := checkMCPTools()
	if got.Level != levelOK {
		t.Errorf("level = %q, want ok; a plugin being installed is not a fault", got.Level)
	}
	for _, want := range []string{"ccdad@my-own-mirror", "mcp__plugin_ccdad_ccdad__", "mcp__ccdad__"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the row does not name %q:\n%s", want, got.Detail)
		}
	}
}

// The row travels in the real report, not only when called directly -- and it
// is the last one, because it was appended rather than inserted. The order is
// fixed so that two runs are diffable, and appending is the only edit that
// cannot reorder somebody else's row.
func TestTheMCPToolsRowIsInTheReportDoctorActuallyPrints(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	seedAccount(t, "uuid-aaaa-0001", "work@example.com")

	code, stdout, _, top := runRoot(t, "doctor", "--json")
	if code != ExitOK && code != ExitFailure {
		t.Fatalf("doctor = %d (%s)", code, top)
	}
	if !strings.Contains(stdout, `"mcp-tools"`) {
		t.Errorf("the mcp-tools row is not in the report:\n%s", stdout)
	}
}

// Four levels, four roles, and no two of them the same.
//
// The four-way distinctness is the assertion that matters, not the specific
// roles: the point of colouring this column at all is that a reader picking the
// fail rows out of twenty-two does not have to judge saturation, and two levels
// sharing a role gives that back. skipped is checked against ok by name as well,
// because that is the pair the eye would most naturally conflate -- "this check
// does not apply here" reading as "this check passed" is the one wrong answer
// this column can give, and it is the level the real checks emit on a fresh
// machine, on Windows and off macOS.
//
// The unrecognised level is here because checkLevel is a string and this file's
// check literals are POSITIONAL: a typo in the second field compiles. It must
// come out RoleDefault, which is no colour at all, rather than inheriting an
// arm and reporting a verdict nobody wrote.
func TestEveryCheckLevelTakesItsOwnRole(t *testing.T) {
	roles := map[checkLevel]theme.Role{}
	for _, l := range []checkLevel{levelOK, levelWarn, levelFail, levelSkipped} {
		roles[l] = levelRole(l)
	}
	seen := map[theme.Role]checkLevel{}
	for l, r := range roles {
		if other, dup := seen[r]; dup {
			t.Errorf("%q and %q share a role, so the column asks a reader to tell two "+
				"different verdicts apart by nothing", l, other)
		}
		seen[r] = l
	}
	if roles[levelSkipped] == roles[levelOK] {
		t.Errorf("skipped is painted like ok, which tells a user a check passed when it " +
			"never ran")
	}
	if got := levelRole(checkLevel("bogus")); got != theme.RoleDefault {
		t.Errorf("an unrecognised level took role %v, want RoleDefault: a typo in a "+
			"positional check literal must go out plain, not wearing fail's colour", got)
	}
}

// The arms are ordered and a machine can match several at once, so the order is
// the decision. It is asserted on the pure verdict rather than through a store,
// because arranging a store that matches three arms at once is arranging the
// test rather than the machine.
func TestTheUpdateCheckRowIsDecidedInPrecedenceOrder(t *testing.T) {
	checkedAt := mustTime("2026-08-25T09:00:00Z")
	published := func(s daemon.Status) daemon.Report {
		s.SchemaVersion = daemon.StatusSchemaVersion
		return daemon.Report{State: daemon.DaemonRunning, HasStatus: true, Status: s}
	}
	// A machine that matches arms 3, 4 and 5's inputs at once.
	crowded := daemon.Status{
		UpdateCheckedAt:   checkedAt,
		NextUpdateCheckAt: checkedAt.Add(24 * time.Hour),
		UpdateLatest:      "0.7.0",
		UpdateCheckError:  "dial tcp: i/o timeout",
	}

	for _, tc := range []struct {
		name    string
		enabled bool
		report  daemon.Report
		running string
		level   checkLevel
		want    string
	}{
		{"switched off outranks both a release and a failure", false, published(crowded), "0.6.1", levelOK, "update_check is false"},
		{"a check that never ran outranks a recorded release", true, published(daemon.Status{UpdateLatest: "0.7.0"}), "0.6.1", levelOK, "no daemon has checked"},
		{"a newer release outranks the failure recorded beside it", true, published(crowded), "0.6.1", levelWarn, "0.7.0 is out"},
		{"a failure is reported when nothing newer is recorded", true, published(daemon.Status{
			UpdateCheckedAt: checkedAt, NextUpdateCheckAt: checkedAt.Add(24 * time.Hour),
			UpdateLatest: "0.6.1", UpdateCheckError: "dial tcp: i/o timeout",
		}), "0.6.1", levelWarn, "the last release check failed"},
		{"the recorded release being the current one is an ok row", true, published(daemon.Status{
			UpdateCheckedAt: checkedAt, UpdateLatest: "0.6.1",
		}), "0.6.1", levelOK, "is the newest release"},
		{"a check taken before this build says so rather than naming an older release", true, published(daemon.Status{
			UpdateCheckedAt: checkedAt, NextUpdateCheckAt: checkedAt.Add(24 * time.Hour), UpdateLatest: "0.6.1",
		}), "0.7.0", levelOK, "predates this build"},
		{"a dev build says the two cannot be compared", true, published(daemon.Status{
			UpdateCheckedAt: checkedAt, UpdateLatest: "0.7.0",
		}), "dev", levelOK, "cannot be compared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, detail := updateCheckVerdict(tc.enabled, tc.report, tc.running)
			if level != tc.level {
				t.Errorf("level = %q, want %q (%s)", level, tc.level, detail)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to contain %q", detail, tc.want)
			}
		})
	}
}

// The row itself: it must never be a failure, because fail is the only level
// that changes doctor's exit code and a release landing must not turn every
// health-check script in the world red.
func TestTheUpdateCheckRowIsNeverAFailureAndAsksNobody(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubVersion(t, "0.6.1")
	// A real origin, pointed at by CCDAD_BASE_URL and counting what reaches it.
	// A probe must not create what it probes, and "makes no request" is the
	// half of this row's name that no assertion about its text can reach.
	origin, _ := newFakeRelease(t, "v0.7.0")
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:   daemon.StatusSchemaVersion,
			UpdateCheckedAt: mustTime("2026-08-25T09:00:00Z"),
			UpdateLatest:    "0.7.0",
		},
	}, nil)

	code, r, _ := runDoctor(t)
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 — a release being out is not a failed health check", code)
	}
	if got := r.level(t, "update-check"); got != "warn" {
		t.Errorf("update-check = %q, want warn: %s", got, r.detail(t, "update-check"))
	}
	if !strings.Contains(r.detail(t, "update-check"), "0.7.0") {
		t.Errorf("the row does not name the release: %s", r.detail(t, "update-check"))
	}
	if asked := origin.asked(); len(asked) != 0 {
		t.Errorf("the release origin was asked for %v; doctor probes what the daemon recorded and must not create the request it reports on", asked)
	}
}

// `ccdad doctor`'s three absolute moments are printed in the READER's zone.
//
// They are the only places in the tree that print an absolute timestamp to a
// person in a machine layout rather than through view.Timestamp, and each one
// reads a field out of the published status document. Printing them in the
// document's own zone is what makes them wrong on the day of an upgrade: the
// old daemon keeps running and keeps publishing until something stops it, so a
// new binary reads a document whose stamps carry whatever zone each writer
// happened to hand them. A `generated 2026-08-22T05:00:00Z` under a KST clock
// is the nine-hour stall this whole item started as.
//
// Every stamp below is seeded in UTC and the zone is pinned away from it, so a
// row that passed its input through unchanged fails — on CI too, where nothing
// sets TZ and local is UTC.
func TestDoctorPrintsAbsoluteStampsInTheReaderZone(t *testing.T) {
	pinReaderZone(t, time.FixedZone("KST", 9*60*60))
	at := mustTime("2026-08-22T05:00:00Z")
	report := daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:     daemon.StatusSchemaVersion,
			GeneratedAt:       at,
			UpdateCheckedAt:   at.Add(-time.Hour),
			NextUpdateCheckAt: at.Add(23 * time.Hour),
			UpdateLatest:      "9.9.9",
			UpdateCheckError:  "the release endpoint refused the request",
		},
	}

	for _, tc := range []struct {
		name   string
		detail string
	}{
		{"status-file", checkStatusFile(report).Detail},
		// The newer-release arm, which is where `checked …` is printed.
		{"update-check newer release", detailOf(updateCheckVerdict(true, report, "0.1.0"))},
		// The failed-check arm, which is the only one that prints the DUE
		// instant. It is reached by making the versions incomparable, because
		// a newer release outranks a failed check.
		{"update-check failure", detailOf(updateCheckVerdict(true, report, "not-a-version"))},
	} {
		stamps := rfc3339Pattern.FindAllString(tc.detail, -1)
		if len(stamps) == 0 {
			t.Errorf("%s printed no absolute moment, so this row decides nothing: %q", tc.name, tc.detail)
			continue
		}
		for _, got := range stamps {
			if !strings.HasSuffix(got, "+09:00") {
				t.Errorf("%s printed %s, which is the document's zone and not the reader's: %q",
					tc.name, got, tc.detail)
			}
		}
	}
}

// rfc3339Pattern is jsonStampPattern without the quotes: `ccdad doctor` prints
// its moments inside a sentence rather than as a JSON value, so they are found
// by shape rather than by position.
var rfc3339Pattern = regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:.]+(?:Z|[+-]\d\d:\d\d)`)

func detailOf(_ checkLevel, detail string) string { return detail }

// An item on a CURRENT release is not a leftover. Every build this project has
// measured -- 2.1.234, 2.1.238, 2.1.251 -- carries a whole keychain backend
// that spawns `security find-generic-password`, and the combinator still reads
// it BEFORE .credentials.json. So the item IS the login, on today's Claude Code
// as much as on 2.1.112, and ccdad installs into it on every switch.
//
// The line this replaces said "nothing is broken right now. Removing it is
// cleanup", which is how a live credential store got read as a leftover while
// every switch silently did nothing.
func TestDoctorReportsAKeychainItemAsTheLiveLoginOnACurrentRelease(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubClaudeInstall(t, claudeVersion(2, 1, 251), nil)
	stubKeychain(t, true, cclink.KeychainItem{
		Service: "Claude Code-credentials-aa3d8c96",
		Account: "tester",
	}, nil)

	code, report, _ := runDoctor(t)
	detail := report.detail(t, "keychain")

	if strings.Contains(detail, "nothing is broken right now") {
		t.Errorf("doctor still calls the live credential store a leftover:\n%s", detail)
	}
	if strings.Contains(detail, "delete-generic-password") {
		t.Errorf("doctor still offers to delete the live login:\n%s", detail)
	}
	for _, want := range []string{
		"BEFORE",
		"every switch",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not mention %q:\n%s", want, detail)
		}
	}
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 -- an item ccdad keeps up to date is not a fault", code)
	}
}

// doctor's oauth-source row named the credentials file from a constant, so on a
// machine whose login is the keychain item it contradicted doctor's OWN
// keychain row two lines above. Both rows are in the same report; a reader
// cannot be expected to decide which one is lying.
func TestDoctorOAuthSourceNamesTheKeychainWhenThatIsWhatAnswered(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubLiveSource(t, cclink.SourceKeychainItem)

	_, report, _ := runDoctor(t)
	detail := report.detail(t, "oauth-source")
	if !strings.Contains(detail, "Keychain") {
		t.Errorf("oauth-source does not name the keychain that answered:\n%s", detail)
	}
	if strings.Contains(detail, "login in the credentials file") {
		t.Errorf("oauth-source still names the file from a constant:\n%s", detail)
	}
}

// And the file, when the file is what answered.
func TestDoctorOAuthSourceNamesTheFileWhenTheFileAnswered(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubLiveSource(t, cclink.SourceCredentialsFile)

	_, report, _ := runDoctor(t)
	detail := report.detail(t, "oauth-source")
	if !strings.Contains(detail, "credentials file") {
		t.Errorf("oauth-source does not name the credentials file:\n%s", detail)
	}
}

// The row that was missing, and the incident that asked for it: a daemon whose
// every tick failed for three hours and twenty minutes while doctor reported
// EVERY row ok -- including `locks`, which said "a daemon holds the singleton"
// and was telling the truth. Liveness is not health, and until this row there
// was nothing between them.
func TestTickHealthFailsWhileTheTickLoopIsFailing(t *testing.T) {
	pinReaderZone(t, time.FixedZone("KST", 9*60*60))
	since := mustTime("2026-08-30T03:04:12Z")
	report := daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status: daemon.Status{
			SchemaVersion:      daemon.StatusSchemaVersion,
			GeneratedAt:        since.Add(3*time.Hour + 20*time.Minute),
			TickFailures:       11300,
			TickFailingSince:   since,
			LastTickError:      "security find-generic-password: said-nothing (exit 60)",
			TickHealthReported: true,
		},
	}

	got := checkTickHealth(report)
	if got.Level != levelFail {
		t.Fatalf("level = %q for a daemon that has switched nothing in three hours, want fail", got.Level)
	}
	for _, want := range []string{"11300", "exit 60"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want %q in it", got.Detail, want)
		}
	}
	// The age of the run is the fact that separates "a tick just failed" from
	// "nothing has worked since lunch", and it is printed in the READER's zone
	// for the reason checkStatusFile gives.
	if stamps := rfc3339Pattern.FindAllString(got.Detail, -1); len(stamps) == 0 {
		t.Errorf("detail = %q, printed no absolute moment, so the row cannot say how old the run is", got.Detail)
	} else {
		for _, stamp := range stamps {
			if !strings.HasSuffix(stamp, "+09:00") {
				t.Errorf("detail printed %s, which is the document's zone and not the reader's", stamp)
			}
		}
	}
}

// A healthy daemon, a stopped one, and a daemon too old to publish the fields
// all read the same on the wire -- absent -- so none of them may read as a
// failure. Saying "ok" about a daemon that never told you is the mistake
// `pidfile` names in its own comment, so this row says what it actually knows.
func TestTickHealthDoesNotInventAVerdict(t *testing.T) {
	if got := checkTickHealth(daemon.Report{State: daemon.DaemonRunning, HasStatus: true,
		Status: daemon.Status{SchemaVersion: daemon.StatusSchemaVersion, TickHealthReported: true}}); got.Level != levelOK {
		t.Errorf("a running daemon with no failures = %q, want ok: %s", got.Level, got.Detail)
	}
	// The upgrade-day case, and it is the one this row would otherwise get
	// exactly backwards. A daemon from before these fields existed publishes
	// them absent, which is byte-for-byte what a healthy daemon publishes -- so
	// reading absence as health would print `ok tick-health` about the very
	// daemon that could be wedged and could not say so. status.go's additive
	// contract makes an old daemon publishing into a new CLI the NORMAL state
	// on the day of an upgrade, not an edge case: the old one keeps running
	// until something stops it.
	if got := checkTickHealth(daemon.Report{State: daemon.DaemonRunning, HasStatus: true,
		Status: daemon.Status{SchemaVersion: daemon.StatusSchemaVersion, TickFailures: 0}}); got.Level != levelSkipped {
		t.Errorf("a daemon that does not report tick health = %q, want skipped: %s", got.Level, got.Detail)
	}
	if got := checkTickHealth(daemon.Report{State: daemon.DaemonStopped}); got.Level != levelSkipped {
		t.Errorf("no daemon = %q, want skipped: %s", got.Level, got.Detail)
	}
	if got := checkTickHealth(daemon.Report{State: daemon.DaemonRunning}); got.Level != levelSkipped {
		t.Errorf("a daemon that has published nothing = %q, want skipped: %s", got.Level, got.Detail)
	}
}

// stubLiveError makes the live-store read fail the way a locked keychain does,
// so the report has to answer for a store it could not read.
func stubLiveError(t *testing.T, err error) {
	t.Helper()
	saved := loadLiveWithSource
	t.Cleanup(func() { loadLiveWithSource = saved })
	loadLiveWithSource = func() (cclink.Blob, cclink.CredentialSource, error) {
		return nil, cclink.SourceNone, err
	}
}

// THE REGRESSION. Present spawns find-generic-password WITHOUT -w so it can
// never raise an auth dialog, which also means it answers 0 for an item whose
// SECRET the keychain is refusing. This row read that as "the item is what
// every request authenticates with" and went green, on a machine where every
// ccdad command was failing with exit 36 and Claude Code had fallen back to
// .credentials.json hours earlier.
func TestDoctorWillNotCallAnUnreadableKeychainItemTheLiveLogin(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	stubKeychain(t, true, cclink.KeychainItem{Service: "Claude Code-credentials", Account: "tester"}, nil)
	stubLiveError(t, errors.New("security find-generic-password: interaction-not-allowed (exit 36)"))

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got == "ok" {
		t.Fatalf("keychain level = ok while the item's secret could not be read:\n%s", report.detail(t, "keychain"))
	}
	detail := report.detail(t, "keychain")
	for _, want := range []string{"could not be READ", "unlock-keychain", "FROM THAT SHELL"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the keychain row does not carry %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "is Claude Code's live login") {
		t.Fatalf("the row still asserts the item is the live login:\n%s", detail)
	}
}

// The green sentence must survive for the machine it is true of: an item that
// is present AND readable.
func TestDoctorStillNamesAReadableKeychainItemAsTheLogin(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	stubKeychain(t, true, cclink.KeychainItem{Service: "Claude Code-credentials", Account: "tester"}, nil)
	stubLiveSource(t, cclink.SourceKeychainItem)

	_, report, _ := runDoctor(t)
	if got := report.level(t, "keychain"); got != "ok" {
		t.Fatalf("keychain level = %q for a readable item, want ok:\n%s", got, report.detail(t, "keychain"))
	}
	if !strings.Contains(report.detail(t, "keychain"), "is Claude Code's live login") {
		t.Fatalf("the green row lost its sentence:\n%s", report.detail(t, "keychain"))
	}
}

// THE ROW MUST NEVER CALL A VERSION OLDER THAN ITSELF "the newest release".
//
// The recorded check is a CACHE, and every machine that has just updated holds
// one taken before the build now running it. The old arm fired on
// `latest <= current` and said "ccdad 0.9.7 is the newest release" out of a
// 0.9.8 binary -- a false sentence, printed green, naming a version older than
// the one printing it. Observed on a real machine minutes after 0.9.8 shipped.
func TestTheUpdateCheckRowNeverCallsAnOlderVersionTheNewest(t *testing.T) {
	checkedAt := mustTime("2026-08-25T09:00:00Z")
	report := daemon.Report{State: daemon.DaemonRunning, HasStatus: true, Status: daemon.Status{
		SchemaVersion:     daemon.StatusSchemaVersion,
		UpdateCheckedAt:   checkedAt,
		NextUpdateCheckAt: checkedAt.Add(24 * time.Hour),
		UpdateLatest:      "0.9.7",
	}}
	level, detail := updateCheckVerdict(true, report, "0.9.8")

	if strings.Contains(detail, "is the newest release") {
		t.Fatalf("the row called 0.9.7 the newest release out of a 0.9.8 build: %q", detail)
	}
	if !strings.Contains(detail, "0.9.7") || !strings.Contains(detail, "0.9.8") {
		t.Fatalf("the row hides which two versions it is talking about: %q", detail)
	}
	// Still ok: a build newer than the last check is not a fault, and fail is
	// the only level that moves doctor's exit code.
	if level != levelOK {
		t.Fatalf("level = %q, want ok -- being ahead of the last check is not a fault", level)
	}
}

// codexShimWorld is a machine with one Codex account and a PATH the test
// controls. It returns the directory a real codex can be put in.
//
// It SKIPS on Windows, and that is not squeamishness about the platform: the
// row is `skipped` there by design, so every arm below -- warn, fail, ok --
// would read `skipped` on the windows-latest leg of the matrix and four of
// these tests would be red for a reason that is the feature working.
// TestDoctorSkipsTheShimRowOnWindows is what pins the Windows answer, and it
// builds its own world so it still runs everywhere.
func codexShimWorld(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("there is no codex shim on Windows, so the row is skipped there; TestDoctorSkipsTheShimRowOnWindows pins that")
	}
	isolate(t)
	seedCodexAccount(t, "cx-doc-1", "c@example.com")
	real := t.TempDir()
	t.Setenv("PATH", strings.Join([]string{shimDir(), real}, string(os.PathListSeparator)))
	return real
}

func writeExecutable(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// No Codex accounts is SKIPPED and not ok: a machine that never uses codex has
// nothing to be told, and an ok row would be one more green line claiming a
// check that was not made.
func TestDoctorSkipsTheShimRowWithNoCodexAccounts(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-doc-1", "a@example.com")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelSkipped) {
		t.Errorf("codex-shim = %q, want skipped on a machine with no codex accounts: %s",
			got, r.detail(t, "codex-shim"))
	}
}

// Codex accounts and no shim ahead of codex is a WARNING: nothing is broken,
// and codex sessions are spending an account ccdad neither chose nor can see.
func TestDoctorWarnsWhenCodexIsNotRoutedThroughCcdad(t *testing.T) {
	real := codexShimWorld(t)
	writeExecutable(t, real, codexProgramName)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelWarn) {
		t.Fatalf("codex-shim = %q, want warn with codex accounts and no shim: %s",
			got, r.detail(t, "codex-shim"))
	}
	detail := r.detail(t, "codex-shim")
	if !strings.Contains(detail, "not routed through ccdad") {
		t.Errorf("the row does not say what is wrong: %s", detail)
	}
	if !strings.Contains(detail, "ccdad codex shim install") {
		t.Errorf("the row does not name the fix: %s", detail)
	}
}

// A shim with NOTHING behind it is a FAILURE, and it is the one arm of this row
// that is: `codex` then resolves to a script that runs `ccdad codex exec`, which
// finds no codex and refuses -- so the machine has a codex command that cannot
// work, and only this row can say why.
func TestDoctorFailsWhenTheShimHasNoRealCodexBehindIt(t *testing.T) {
	codexShimWorld(t)
	writeExecutable(t, shimDir(), codexProgramName)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelFail) {
		t.Fatalf("codex-shim = %q, want fail with a shim and no codex behind it: %s",
			got, r.detail(t, "codex-shim"))
	}
	if !strings.Contains(r.detail(t, "codex-shim"), "no other codex") {
		t.Errorf("the row does not say what is missing: %s", r.detail(t, "codex-shim"))
	}
}

func TestDoctorIsHappyWhenTheShimIsFirstAndACodexIsBehindIt(t *testing.T) {
	real := codexShimWorld(t)
	writeExecutable(t, shimDir(), codexProgramName)
	writeExecutable(t, real, codexProgramName)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelOK) {
		t.Fatalf("codex-shim = %q, want ok: %s", got, r.detail(t, "codex-shim"))
	}
	if !strings.Contains(r.detail(t, "codex-shim"), filepath.Join(real, codexProgramName)) {
		t.Errorf("the row does not name the codex behind the shim: %s", r.detail(t, "codex-shim"))
	}
}

// A shim that exists but is NOT first is a warning, not an ok: a bare `codex`
// resolves to something else, so the machine has a shim it never runs.
func TestDoctorWarnsWhenSomethingElseIsAheadOfTheShim(t *testing.T) {
	real := codexShimWorld(t)
	writeExecutable(t, shimDir(), codexProgramName)
	writeExecutable(t, real, codexProgramName)
	// Put the real one FIRST.
	t.Setenv("PATH", strings.Join([]string{real, shimDir()}, string(os.PathListSeparator)))

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelWarn) {
		t.Fatalf("codex-shim = %q, want warn when a bare `codex` is not the shim: %s",
			got, r.detail(t, "codex-shim"))
	}
	if !strings.Contains(r.detail(t, "codex-shim"), "not routed through ccdad") {
		t.Errorf("the row does not say what is wrong: %s", r.detail(t, "codex-shim"))
	}
}

// Windows has no shim in v1, so the row is skipped there rather than warning
// forever about a thing that cannot be installed.
func TestDoctorSkipsTheShimRowOnWindows(t *testing.T) {
	// Its own world rather than codexShimWorld's, because that helper skips on
	// Windows and this is the one test that has to run on every OS: it drives
	// the Windows answer through shimOS, and the answer is the same wherever
	// the test is running.
	isolate(t)
	seedCodexAccount(t, "cx-doc-2", "c@example.com")
	saved := shimOS
	t.Cleanup(func() { shimOS = saved })
	shimOS = "windows"

	_, r, _ := runDoctor(t)
	if got := r.level(t, "codex-shim"); got != string(levelSkipped) {
		t.Errorf("codex-shim = %q, want skipped on Windows: %s", got, r.detail(t, "codex-shim"))
	}
	if !strings.Contains(r.detail(t, "codex-shim"), "ccdad codex exec") {
		t.Errorf("the row does not name what Windows runs instead: %s", r.detail(t, "codex-shim"))
	}
}
