package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// readRaw returns status.json as a generic map, which is how an external
// consumer of the §9.4 contract sees it.
func readRaw(t *testing.T) map[string]any {
	t.Helper()
	body, err := os.ReadFile(StatusPath())
	if err != nil {
		t.Fatalf("reading status.json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("status.json does not parse: %v", err)
	}
	return out
}

func TestStatusWriterPublishesTheDocument(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	now := at("2026-08-22T05:00:00Z")

	w := NewStatusWriter()
	wrote, err := w.Write(Status{
		PID:        4242,
		StartedAt:  at("2026-08-22T04:00:00Z"),
		ActiveUUID: "uuid-a",
		Accounts: []AccountStatus{{
			UUID:       "uuid-a",
			State:      StateActive,
			NextPollAt: at("2026-08-22T05:03:00Z"),
		}},
	}, now)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !wrote {
		t.Fatal("the first Write skipped; there was nothing on disk to be unchanged from")
	}

	raw := readRaw(t)
	if got := raw["schemaVersion"]; got != float64(StatusSchemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", got, StatusSchemaVersion)
	}
	if got := raw["generatedAt"]; got != "2026-08-22T05:00:00Z" {
		t.Errorf("generatedAt = %v, want the write time", got)
	}
	if got := raw["pid"]; got != float64(4242) {
		t.Errorf("pid = %v, want 4242", got)
	}
	if got := raw["activeUuid"]; got != "uuid-a" {
		t.Errorf("activeUuid = %v", got)
	}
}

// A zero time must not reach the wire as year 1. time.Time is a struct, so the
// familiar `omitempty` does nothing to it — this pins that the tag actually
// used is one that works, because a consumer rendering "0001-01-01" as a poll
// time is worse than a consumer seeing no field at all.
func TestStatusOmitsUnsetTimes(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	w := NewStatusWriter()
	if _, err := w.Write(Status{PID: 1, Accounts: []AccountStatus{{UUID: "u"}}}, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(StatusPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "0001-01-01") {
		t.Fatalf("an unset time reached the wire as year 1:\n%s", body)
	}
	raw := readRaw(t)
	if _, ok := raw["startedAt"]; ok {
		t.Error("startedAt is present although it was never set")
	}
}

func TestStatusWriterSkipsAnUnchangedDocument(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Status{PID: 7, Accounts: []AccountStatus{{UUID: "u", State: StateCandidate}}}

	w := NewStatusWriter()
	if _, err := w.Write(s, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(StatusPath())
	if err != nil {
		t.Fatal(err)
	}

	wrote, err := w.Write(s, at("2026-08-22T05:00:01Z"))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("an unchanged document was written again; at 1 Hz that is 86,400 fsync+rename cycles a day for nothing")
	}
	// generatedAt is a change stamp, not a heartbeat. An unchanged tick must
	// leave it exactly where it was, or the skip is unobservable and the next
	// reader of this file deletes it as pointless.
	if got := readRaw(t)["generatedAt"]; got != "2026-08-22T05:00:00Z" {
		t.Errorf("generatedAt = %v after a skipped write, want the earlier stamp", got)
	}
	after, err := os.Stat(StatusPath())
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the file was replaced although nothing changed")
	}
}

func TestStatusWriterPublishesAChange(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	w := NewStatusWriter()
	if _, err := w.Write(Status{PID: 7, ActiveUUID: "uuid-a"}, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	wrote, err := w.Write(Status{PID: 7, ActiveUUID: "uuid-b"}, at("2026-08-22T05:00:01Z"))
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("a changed document was skipped")
	}
	raw := readRaw(t)
	if raw["activeUuid"] != "uuid-b" {
		t.Errorf("activeUuid = %v, want the new value", raw["activeUuid"])
	}
	if raw["generatedAt"] != "2026-08-22T05:00:01Z" {
		t.Errorf("generatedAt = %v, want the time the change was published", raw["generatedAt"])
	}
}

// The skip is remembered in memory, so a file that vanished — a `rm`, a
// half-finished uninstall, a store on a filesystem that was remounted — would
// otherwise stay gone for the daemon's whole life while the writer keeps
// reporting "unchanged".
func TestStatusWriterRepublishesWhenTheFileIsGone(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Status{PID: 7}
	w := NewStatusWriter()
	if _, err := w.Write(s, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(StatusPath()); err != nil {
		t.Fatal(err)
	}
	wrote, err := w.Write(s, at("2026-08-22T05:00:01Z"))
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("the writer skipped a document that is no longer on disk")
	}
	if _, err := os.Stat(StatusPath()); err != nil {
		t.Fatalf("status.json was not restored: %v", err)
	}
}

func TestStatusWriterRepublishesWhenTheFileWasTruncated(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	s := Status{PID: 7}
	w := NewStatusWriter()
	if _, err := w.Write(s, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatusPath(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err := w.Write(s, at("2026-08-22T05:00:01Z"))
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("the writer skipped although what is on disk is not what it published")
	}
	if readRaw(t)["pid"] != float64(7) {
		t.Error("the document was not restored")
	}
}

func TestStatusFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("§10.3: chmod is a no-op on Windows and nothing may depend on the mode")
	}
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStatusWriter().Write(Status{PID: 1}, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(StatusPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("status.json mode = %04o, want 0600 to match the rest of the store", got)
	}
}

func TestStatusWriterCreatesTheStore(t *testing.T) {
	isolate(t)
	// No MkdirAll here: the daemon writes its first status before anything else
	// has necessarily created the store.
	if _, err := NewStatusWriter().Write(Status{PID: 1}, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatalf("Write into a store that does not exist yet: %v", err)
	}
	if _, err := os.Stat(StatusPath()); err != nil {
		t.Fatal(err)
	}
}

// §9.4's contract is additive, and the reason it has to be right in v1 is that
// an upgrade leaves an old daemon running against a new CLI until something
// stops it. Both directions are the same requirement: ignore what you do not
// know.
func TestReadStatusIgnoresUnknownFields(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schemaVersion":9,"generatedAt":"2026-08-22T05:00:00Z","pid":11,
	          "activeUuid":"uuid-a","somethingV9Added":{"deep":[1,2,3]},
	          "accounts":[{"uuid":"uuid-a","state":"orbiting","perAccountV9Field":true}]}`
	if err := os.WriteFile(StatusPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s, ok, err := ReadStatus()
	if err != nil {
		t.Fatalf("a v9 document must still read: %v", err)
	}
	if !ok {
		t.Fatal("ReadStatus reported no document")
	}
	if s.PID != 11 || s.ActiveUUID != "uuid-a" {
		t.Errorf("known fields were lost: %+v", s)
	}
	if len(s.Accounts) != 1 || s.Accounts[0].UUID != "uuid-a" {
		t.Fatalf("accounts = %+v", s.Accounts)
	}
	// A state this binary has never heard of is carried through verbatim rather
	// than normalised to "unknown": the renderer decides what to do with it, and
	// flattening it here would erase the only evidence the file came from a
	// newer daemon.
	if s.Accounts[0].State != "orbiting" {
		t.Errorf("state = %q, want the value as written", s.Accounts[0].State)
	}
}

func TestReadStatusReportsAbsenceRatherThanFailing(t *testing.T) {
	isolate(t)
	s, ok, err := ReadStatus()
	if err != nil {
		t.Fatalf("a store with no status.json is not an error: %v", err)
	}
	if ok {
		t.Fatalf("ReadStatus invented a document: %+v", s)
	}
}

func TestReadStatusRefusesADocumentWithNoSchemaVersion(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatusPath(), []byte(`{"pid":11}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadStatus(); err == nil || ok {
		t.Fatalf("a document with no schemaVersion was accepted (ok=%v, err=%v)", ok, err)
	}
}

func TestReadStatusRefusesAnUnparseableDocument(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatusPath(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadStatus(); err == nil || ok {
		t.Fatalf("a corrupt document was accepted (ok=%v, err=%v)", ok, err)
	}
}

func TestStatusRoundTrips(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(filepath.Dir(StatusPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	want := Status{
		PID:          9,
		StartedAt:    at("2026-08-22T04:00:00Z"),
		Stopped:      true,
		ActiveUUID:   "uuid-a",
		LastSwitchAt: at("2026-08-22T04:30:00Z"),
		LastSwitchTo: "uuid-a",
		Accounts: []AccountStatus{{
			UUID:          "uuid-b",
			State:         StateQuarantined,
			NextPollAt:    at("2026-08-22T05:10:00Z"),
			LastPollAt:    at("2026-08-22T05:00:00Z"),
			LastPollError: "the usage endpoint rejected the token",
		}},
	}
	if _, err := NewStatusWriter().Write(want, at("2026-08-22T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadStatus()
	if err != nil || !ok {
		t.Fatalf("ReadStatus: ok=%v err=%v", ok, err)
	}
	want.SchemaVersion = StatusSchemaVersion
	want.GeneratedAt = at("2026-08-22T05:00:00Z")
	if !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Errorf("generatedAt = %v, want %v", got.GeneratedAt, want.GeneratedAt)
	}
	got.GeneratedAt, want.GeneratedAt = time.Time{}, time.Time{}
	if !got.StartedAt.Equal(want.StartedAt) || !got.LastSwitchAt.Equal(want.LastSwitchAt) {
		t.Errorf("times did not round-trip: %+v", got)
	}
	got.StartedAt, want.StartedAt = time.Time{}, time.Time{}
	got.LastSwitchAt, want.LastSwitchAt = time.Time{}, time.Time{}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
	if !got.Accounts[0].NextPollAt.Equal(want.Accounts[0].NextPollAt) ||
		!got.Accounts[0].LastPollAt.Equal(want.Accounts[0].LastPollAt) {
		t.Errorf("account times did not round-trip: %+v", got.Accounts[0])
	}
	got.Accounts[0].NextPollAt, want.Accounts[0].NextPollAt = time.Time{}, time.Time{}
	got.Accounts[0].LastPollAt, want.Accounts[0].LastPollAt = time.Time{}, time.Time{}
	if got.SchemaVersion != want.SchemaVersion || got.PID != want.PID || got.Stopped != want.Stopped ||
		got.ActiveUUID != want.ActiveUUID || got.LastSwitchTo != want.LastSwitchTo ||
		got.Accounts[0] != want.Accounts[0] {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// WriteFileAtomic's own comment accepts an orphaned temp file as rare. At one
// write per second it stops being rare, so the daemon sweeps them at startup.
func TestSweepStatusTempsRemovesOrphansAndNothingElse(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := map[string]string{
		"status.json":      `{"schemaVersion":1}`,
		"ccdad.pid":        "1\n",
		"ccdad.lock":       "",
		"usage.json":       "{}",
		"usage.json.tmp-1": "{}",
	}
	for name, body := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orphans := []string{"status.json.tmp-1", "status.json.tmp-987654321"}
	for _, name := range orphans {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := SweepStatusTemps(); err != nil {
		t.Fatalf("SweepStatusTemps: %v", err)
	}
	for _, name := range orphans {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep", name)
		}
	}
	for name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			// usage.json.tmp-1 belongs to a writer that may be a live CLI
			// process holding the cache lock; sweeping it would race a
			// stranger's rename.
			t.Errorf("%s was removed but is not this file's orphan: %v", name, err)
		}
	}
}

func TestSweepStatusTempsToleratesAMissingStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CCDAD_HOME", filepath.Join(dir, "never-created"))
	if err := SweepStatusTemps(); err != nil {
		t.Fatalf("a store that does not exist has no orphans, not an error: %v", err)
	}
}

// A crashed daemon leaves a perfectly valid status.json behind. Nothing in the
// document can report that, so the reader has to ask the singleton.
func TestObserveReportsStoppedWhenTheSingletonIsFree(t *testing.T) {
	isolate(t)
	if _, err := NewStatusWriter().Write(Status{PID: 4242}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if r.State != DaemonStopped {
		t.Errorf("state = %v, want stopped: the document is fresh but nothing holds the lock", r.State)
	}
	if !r.HasStatus || r.Status.PID != 4242 {
		t.Errorf("the document should still be reported: %+v", r)
	}
}

func TestObserveReportsRunningWhileTheSingletonIsHeld(t *testing.T) {
	isolate(t)
	s, err := AcquireSingleton()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	if _, err := NewStatusWriter().Write(Status{PID: os.Getpid()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := Observe()
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if r.State != DaemonRunning {
		t.Errorf("state = %v, want running", r.State)
	}
}

// "Cannot tell" is not "no", one layer up as well: a dashboard that renders a
// broken lock as "no daemon" tells the user to start a second one.
func TestObserveCannotDetermineWhenLocksAreBroken(t *testing.T) {
	isolate(t)
	if _, err := NewStatusWriter().Write(Status{PID: 4242}, time.Now()); err != nil {
		t.Fatal(err)
	}
	restore := setTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	defer restore()

	r, err := Observe()
	if err == nil {
		t.Fatal("Observe reported success on a filesystem that cannot lock")
	}
	if !errors.Is(err, ErrLocksUnsupported) {
		t.Errorf("err = %v, want it to carry ErrLocksUnsupported", err)
	}
	if r.State != DaemonUnknown {
		t.Errorf("state = %v, want unknown", r.State)
	}
	// The document is still worth reporting; it is the liveness verdict that is
	// missing, not the contents.
	if !r.HasStatus {
		t.Error("the document was discarded along with the probe failure")
	}
}

func TestObserveCarriesAnUnreadableDocumentWithoutFailing(t *testing.T) {
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatusPath(), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Observe()
	if err != nil {
		t.Fatalf("a corrupt status document must not break the liveness answer: %v", err)
	}
	if r.State != DaemonStopped {
		t.Errorf("state = %v, want stopped", r.State)
	}
	if r.HasStatus {
		t.Error("a corrupt document was reported as readable")
	}
	if r.StatusErr == nil {
		t.Error("the parse failure was swallowed; doctor has nothing to print")
	}
}
