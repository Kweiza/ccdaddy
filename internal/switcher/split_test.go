package switcher

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// itemLogin is the compact JSON Claude Code keeps in the keychain item, for the
// account seeded under uuid.
func itemLogin(uuid string) string {
	return `{"claudeAiOauth":{"accessToken":"AT-RT-` + uuid + `","refreshToken":"RT-` + uuid + `"}}`
}

// withItem puts a fake keychain item on this machine, holding the account
// seeded under uuid.
func withItem(t *testing.T, uuid string) *cclinktest.Keychain {
	t.Helper()
	kc := cclinktest.NewKeychain(itemLogin(uuid))
	cclinktest.Install(t, kc)
	return kc
}

func recordActive(t *testing.T, uuid string) {
	t.Helper()
	if err := openStore(t).SetActive(uuid); err != nil {
		t.Fatal(err)
	}
}

func itemHolds(t *testing.T, kc *cclinktest.Keychain, uuid string) bool {
	t.Helper()
	secret, ok := kc.Secret()
	return ok && strings.Contains(secret, `"RT-`+uuid+`"`)
}

func fileHolds(t *testing.T, uuid string) bool {
	t.Helper()
	return strings.Contains(string(readLive(t)), `"RT-`+uuid+`"`)
}

// --- the view, which is pure ---

var epoch = time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)

func managed(uuid string) StoreHolder {
	return StoreHolder{Account: store.Account{UUID: uuid}, State: LiveManaged, ID: "refresh:RT-" + uuid}
}

// expiring is h with an expiry, minutes after epoch.
func expiring(h StoreHolder, minutes int) StoreHolder {
	h.Expires = epoch.Add(time.Duration(minutes) * time.Minute)
	return h
}

func unnamed(id string) StoreHolder {
	return StoreHolder{State: LiveUnattributed, ID: id}
}

// resolved is uuid's login in a generation ccdad has not stored.
func resolved(uuid, id string) StoreHolder {
	return StoreHolder{Account: store.Account{UUID: uuid}, State: LiveResolved, ID: id}
}

func TestWhatCountsAsASplitLogin(t *testing.T) {
	rec := func(uuid string) store.Account { return store.Account{UUID: uuid} }
	cases := []struct {
		name       string
		v          LoginView
		split      bool
		unresolved bool
	}{
		{"all three agree", LoginView{HasItem: true, Item: managed("a"), File: managed("a"), Recorded: rec("a")}, false, false},
		// THE INCIDENT: the keychain write landed, the file never followed, and
		// nothing was recorded.
		{"item and file disagree", LoginView{HasItem: true, Item: managed("b"), File: managed("a"), Recorded: rec("a")}, true, false},
		{"item and file disagree, record on the item", LoginView{HasItem: true, Item: managed("b"), File: managed("a"), Recorded: rec("b")}, true, false},
		{"stores agree, record behind", LoginView{HasItem: true, Item: managed("b"), File: managed("b"), Recorded: rec("a")}, true, false},
		{"no item, file and record disagree", LoginView{File: managed("b"), Recorded: rec("a")}, true, false},
		{"no item, all agree", LoginView{File: managed("a"), Recorded: rec("a")}, false, false},
		{"item with no file behind it", LoginView{HasItem: true, Item: managed("a"), Recorded: rec("a")}, false, false},
		// A file this store cannot name beside a named item is NOT fine: it is
		// what Claude Code leaves when it refreshes in the item, and ALSO what
		// it leaves when its keychain write fails and the newer login goes to
		// the file. Unresolved until the oracle names it.
		{"file unnamed beside a named item", LoginView{HasItem: true, Item: managed("a"),
			File: unnamed("refresh:X"), Recorded: rec("a")}, false, true},
		// A lead this store cannot name may be a managed account mid-rotation;
		// a repair is a write, and writing over it is the rotation stomp.
		{"item unnamed", LoginView{HasItem: true, Item: unnamed("refresh:X"),
			File: managed("a"), Recorded: rec("b")}, false, true},
		{"item unreadable", LoginView{HasItem: true, Item: StoreHolder{State: LiveUnreadable},
			File: managed("a"), Recorded: rec("b")}, false, false},
		// Two generations of one account, the newer stored: a split, repaired
		// towards the newer.
		{"one account, the file a newer stored generation", LoginView{HasItem: true,
			Item: expiring(resolved("a", "refresh:old"), 0), File: expiring(managed("a"), 60), Recorded: rec("a")}, true, false},
		// The newer login is not ccdad's stored one yet: it is adopted first.
		{"the newer login not yet stored", LoginView{HasItem: true,
			Item: expiring(managed("a"), 0), File: expiring(resolved("a", "refresh:new"), 60), Recorded: rec("a")}, false, true},
	}
	for _, tc := range cases {
		if got := tc.v.Split(); got != tc.split {
			t.Errorf("%s: Split() = %v, want %v (%s)", tc.name, got, tc.split, tc.v.Describe())
		}
		if got := tc.v.Unresolved(); got != tc.unresolved {
			t.Errorf("%s: Unresolved() = %v, want %v (%s)", tc.name, got, tc.unresolved, tc.v.Describe())
		}
	}
}

// StoresOn -- what `ccdad switch` answers "Already on" from -- draws the same
// line: a machine the daemon and doctor do not call fine is not "already on".
func TestStoresOnAgreesWithSplitAndUnresolved(t *testing.T) {
	views := []LoginView{
		{HasItem: true, Item: managed("a"), File: unnamed("refresh:X"), Recorded: store.Account{UUID: "a"}},
		{HasItem: true, Item: managed("a"), File: managed("b"), Recorded: store.Account{UUID: "a"}},
		{HasItem: true, Item: managed("a"), File: managed("a"), Recorded: store.Account{UUID: "a"}},
		{HasItem: true, Item: managed("a"), Recorded: store.Account{UUID: "a"}},
	}
	for _, v := range views {
		fine := !v.Split() && !v.Unresolved()
		if got := v.AllOn("a"); got != fine {
			t.Errorf("AllOn(a) = %v but split=%v unresolved=%v: %s", got, v.Split(), v.Unresolved(), v.Describe())
		}
	}
}

// The repair goes TOWARDS THE LOGIN ISSUED LATER, the item on a tie.
func TestASplitIsRepairedTowardsTheNewerLogin(t *testing.T) {
	tie := LoginView{HasItem: true, Item: managed("support"), File: managed("chan"),
		Recorded: store.Account{UUID: "chan"}}
	if got := tie.RepairTarget().UUID; got != "support" {
		t.Fatalf("on a tie RepairTarget = %q, want the item's account", got)
	}
	itemNewer := LoginView{HasItem: true, Item: expiring(managed("support"), 60), File: expiring(managed("chan"), 0)}
	if got := itemNewer.RepairTarget().UUID; got != "support" {
		t.Fatalf("item newer: RepairTarget = %q, want support", got)
	}
	// A /login whose keychain write failed lands in the FILE: newer, and the
	// user's latest decision.
	fileNewer := LoginView{HasItem: true, Item: expiring(managed("support"), 0), File: expiring(managed("chan"), 60)}
	if got := fileNewer.RepairTarget().UUID; got != "chan" {
		t.Fatalf("file newer: RepairTarget = %q, want chan", got)
	}
	if got := (LoginView{File: managed("b"), Recorded: store.Account{UUID: "a"}}).RepairTarget().UUID; got != "b" {
		t.Fatalf("with no item RepairTarget = %q, want the file's account", got)
	}
}

func TestDescribeNamesEachStoreWithoutATokenInSight(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-1"))
	withItem(t, "u-2")
	recordActive(t, "u-1")

	st, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	got := ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID(), nil).Describe()
	for _, want := range []string{
		"keychain item Claude Code reads first holds support@example.com (u-2)",
		"credentials file holds chan@example.com (u-1)",
		"records chan@example.com (u-1) as live",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "RT-") || strings.Contains(got, "AT-") {
		t.Fatalf("Describe() prints a token: %q", got)
	}
}

// --- the switch's no-op rule ---

// `ccdad switch support` on the incident's machine said "Already on support",
// because it asked the item alone. Already-on needs every store; anything else
// installs the target into both.
func TestASwitchIsNotAlreadyOnWhileTheFileHoldsSomeoneElse(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-1"))
	kc := withItem(t, "u-2")
	recordActive(t, "u-1")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want switched: the file still holds the other account", res.Outcome)
	}
	if !res.Before.Split() {
		t.Fatal("the result does not say the login was split before")
	}
	if !itemHolds(t, kc, "u-2") || !fileHolds(t, "u-2") {
		t.Fatalf("both stores should hold u-2 now; file:\n%s", readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
}

func TestASwitchIsAlreadyOnOnlyWhenEveryStoreAndTheRecordAgree(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-2"))
	withItem(t, "u-2")
	recordActive(t, "u-2")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != AlreadyOn || res.RecordedNow {
		t.Fatalf("outcome = %v (recorded now %v), want already on with nothing to correct", res.Outcome, res.RecordedNow)
	}
	if string(before) != string(readLive(t)) {
		t.Fatal("an already-on switch rewrote the credentials file")
	}
}

// Both stores on the target and only the record behind: nothing to install,
// and the record is corrected.
func TestAStaleRecordIsCorrectedWithoutAWrite(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-2"))
	withItem(t, "u-2")
	recordActive(t, "u-1")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != AlreadyOn || !res.RecordedNow {
		t.Fatalf("outcome = %v (recorded now %v), want already on with the record corrected", res.Outcome, res.RecordedNow)
	}
	if got := openStore(t).ActiveUUID(); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if string(before) != string(readLive(t)) {
		t.Fatal("a record-only correction rewrote the credentials file")
	}
}

// --- a keychain write that ran out of time ---

// THE INCIDENT, end to end through the executor: the write lands and reports a
// timeout. Reading it back finds the target, so the switch completes exactly as
// a normal success does -- file, record, cooldown and the departure the plan
// named.
func TestASwitchWhoseKeychainWriteLandedAndTimedOutCompletes(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut = true
	recordActive(t, "u-1")

	res, err := Execute(openStore(t), Request{
		Target: target, LiveUUID: "u-1", Unattended: true,
		Departure: strategy.Departure{From: "u-1", Closing: usage.WindowSevenDay},
	})
	if err != nil {
		t.Fatalf("Execute = %v, want the switch completed: the item holds the target", err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want switched", res.Outcome)
	}
	if !itemHolds(t, kc, "u-2") || !fileHolds(t, "u-2") {
		t.Fatalf("the item holds u-2 and the file was left behind:\n%s", readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if _, to := lastSwitch(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2", to)
	}
	path, err := strategy.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"u-1"`) || !strings.Contains(string(raw), `"seven_day"`) {
		t.Fatalf("the departure the plan named was not recorded:\n%s", raw)
	}
}

// The write timed out and did NOT land: the item still holds the previous
// login. That is a failure, reported as today, with nothing recorded.
func TestASwitchWhoseKeychainWriteTimedOutWithoutLandingFails(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteTimesOutUnlanded = true
	recordActive(t, "u-1")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err == nil {
		t.Fatal("Execute succeeded although the item still holds u-1")
	}
	if errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("the item was read back and named u-1, so the outcome is known: %v", err)
	}
	if res.Outcome == Switched || !fileHolds(t, "u-1") || !itemHolds(t, kc, "u-1") {
		t.Fatalf("outcome %v; a write that did not land moved something", res.Outcome)
	}
	if got := openStore(t).ActiveUUID(); got != "u-1" {
		t.Fatalf("active = %q, want u-1 untouched", got)
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a switch that did not happen stamped the cooldown")
	}
}

// The write timed out and the item cannot be read back. Neither outcome is
// claimed and nothing is recorded; the split this may leave is the daemon's
// next tick to find.
func TestASwitchWhoseKeychainWriteCannotBeConfirmedClaimsNeither(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	target := seed(t, "u-2", "support@example.com")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	recordActive(t, "u-1")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Execute = %v, want ErrLoginOutcomeUnknown", err)
	}
	if res.Outcome == Switched || res.Outcome == AlreadyOn {
		t.Fatalf("outcome = %v; an unconfirmed write claimed an outcome", res.Outcome)
	}
	if !fileHolds(t, "u-1") {
		t.Fatalf("the file was written past an unconfirmed keychain write:\n%s", readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got != "u-1" {
		t.Fatalf("active = %q; an unconfirmed switch was recorded", got)
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("an unconfirmed switch stamped the cooldown")
	}
}

// --- the engine's read ---

// Evaluate reports the split from the same read it attributes Live from.
func TestEvaluateSeesASplitLogin(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "chan@example.com")
	seed(t, "u-2", "support@example.com")
	writeLive(t, itemLogin("u-1"))
	withItem(t, "u-2")
	recordActive(t, "u-1")

	ev, err := Evaluate(openStore(t), EvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.LoginKnown || !ev.Login.Split() {
		t.Fatalf("Evaluate did not see the split: known=%v %s", ev.LoginKnown, ev.Login.Describe())
	}
	if ev.Live.UUID != "u-2" {
		t.Fatalf("Live = %q, want the item's account", ev.Live.UUID)
	}
}
