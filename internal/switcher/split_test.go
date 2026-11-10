package switcher

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
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
func withItem(t *testing.T, uuid string) *cclink.FakeKeychain {
	t.Helper()
	kc := cclink.NewFakeKeychain(itemLogin(uuid))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	return kc
}

func recordActive(t *testing.T, uuid string) {
	t.Helper()
	if err := openStore(t).SetActive(uuid); err != nil {
		t.Fatal(err)
	}
}

func itemHolds(t *testing.T, kc *cclink.FakeKeychain, uuid string) bool {
	t.Helper()
	secret, ok := kc.Secret()
	return ok && strings.Contains(secret, `"RT-`+uuid+`"`)
}

func fileHolds(t *testing.T, uuid string) bool {
	t.Helper()
	return strings.Contains(string(readLive(t)), `"RT-`+uuid+`"`)
}

// --- the view, which is pure ---

func managed(uuid string) StoreHolder {
	return StoreHolder{Account: store.Account{UUID: uuid}, State: LiveManaged}
}

func TestWhatCountsAsASplitLogin(t *testing.T) {
	rec := func(uuid string) store.Account { return store.Account{UUID: uuid} }
	cases := []struct {
		name  string
		v     LoginView
		split bool
	}{
		{"all three agree", LoginView{HasItem: true, Item: managed("a"), File: managed("a"), Recorded: rec("a")}, false},
		// THE INCIDENT: the keychain write landed, the file never followed, and
		// nothing was recorded.
		{"item and file disagree", LoginView{HasItem: true, Item: managed("b"), File: managed("a"), Recorded: rec("a")}, true},
		{"item and file disagree, record on the item", LoginView{HasItem: true, Item: managed("b"), File: managed("a"), Recorded: rec("b")}, true},
		{"stores agree, record behind", LoginView{HasItem: true, Item: managed("b"), File: managed("b"), Recorded: rec("a")}, true},
		{"no item, file and record disagree", LoginView{File: managed("b"), Recorded: rec("a")}, true},
		{"no item, all agree", LoginView{File: managed("a"), Recorded: rec("a")}, false},
		// Claude Code refreshes the login in the item and leaves the file
		// behind: the ordinary state of a machine in use, not a split.
		{"file unnamed beside a named item", LoginView{HasItem: true, Item: managed("a"),
			File: StoreHolder{State: LiveUnattributed}, Recorded: rec("a")}, false},
		// A lead this store cannot name may be a managed account mid-rotation;
		// a repair is a write, and writing over it is the rotation stomp.
		{"item unnamed", LoginView{HasItem: true, Item: StoreHolder{State: LiveUnattributed},
			File: managed("a"), Recorded: rec("b")}, false},
		{"item unreadable", LoginView{HasItem: true, Item: StoreHolder{State: LiveUnreadable},
			File: managed("a"), Recorded: rec("b")}, false},
	}
	for _, tc := range cases {
		if got := tc.v.Split(); got != tc.split {
			t.Errorf("%s: Split() = %v, want %v (%s)", tc.name, got, tc.split, tc.v.Describe())
		}
	}
}

// The repair goes TOWARDS THE ITEM. It is the store every writer reaches
// first -- ccdad writes it before the file, Claude Code's /login and refresh
// write it and skip the file -- so when the two disagree it holds the newer
// decision.
func TestASplitIsRepairedTowardsTheItem(t *testing.T) {
	v := LoginView{HasItem: true, Item: managed("support"), File: managed("chan"),
		Recorded: store.Account{UUID: "chan"}}
	if got := v.RepairTarget().UUID; got != "support" {
		t.Fatalf("RepairTarget = %q, want the item's account", got)
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
	got := ViewLogin(st, s.Accounts(), s.Credentials, s.ActiveUUID()).Describe()
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
