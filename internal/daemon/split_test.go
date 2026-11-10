package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The daemon's half of the 2026-11-10 incident: the keychain item and the
// credentials file named different accounts, the engine read the item, saw
// room, and never acted again while Claude Code spent the file's account.

func itemLogin(uuid string) string {
	return `{"claudeAiOauth":{"accessToken":"AT-RT-` + uuid + `","refreshToken":"RT-` + uuid + `"}}`
}

func withItem(t *testing.T, uuid string) *cclink.FakeKeychain {
	t.Helper()
	kc := cclink.NewFakeKeychain(itemLogin(uuid))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	return kc
}

func recordActive(t *testing.T, uuid string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(uuid); err != nil {
		t.Fatal(err)
	}
}

func activeOnDisk(t *testing.T) string {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s.ActiveUUID()
}

func fileNow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func holds(blob, uuid string) bool { return strings.Contains(blob, `"RT-`+uuid+`"`) }

// splitReadings is the incident's usage: chan nearly spent, support with room.
// With the item on support the plan stays put -- which is exactly why the
// engine never acted again.
func splitReadings(t *testing.T) {
	t.Helper()
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("u-1", usage.Entry{Snapshot: snapshotWith(99), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		c.Put("u-2", usage.Entry{Snapshot: snapshotWith(36), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) attach(e *Engine) {
	e.Log = func(format string, a ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, fmt.Sprintf(format, a...))
	}
}

func (l *logLines) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

func noFetch(context.Context, string) (*usage.Snapshot, error) { return snapshotWith(50), nil }

// blobExpiring is a compact login under refresh token rt, expiring at exp.
func blobExpiring(rt string, exp time.Time) string {
	return `{"claudeAiOauth":{"accessToken":"AT-` + rt + `","refreshToken":"` + rt + `","expiresAt":` +
		fmt.Sprint(exp.UnixMilli()) + `,"scopes":["user:inference","user:profile"]}}`
}

func loginExpiring(uuid string, exp time.Time) string { return blobExpiring("RT-"+uuid, exp) }

// seedAccountExpiring is seedAccount with the stored login expiring at exp.
func seedAccountExpiring(t *testing.T, uuid, org string, exp time.Time) {
	t.Helper()
	seedAccount(t, uuid, org)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get(uuid)
	var b cclink.Blob
	if err := json.Unmarshal([]byte(loginExpiring(uuid, exp)), &b); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(a, b); err != nil {
		t.Fatal(err)
	}
}

func writeLiveLoginRT(t *testing.T, rt string, exp time.Time) {
	t.Helper()
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(blobExpiring(rt, exp)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLiveLogin(t *testing.T, uuid string, exp time.Time) { writeLiveLoginRT(t, "RT-"+uuid, exp) }

// readings puts one five-hour reading per account into the cache.
func readings(t *testing.T, used map[string]float64) {
	t.Helper()
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		for uuid, u := range used {
			c.Put(uuid, usage.Entry{Snapshot: snapshotWith(u), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// departureOnDisk reports whether strategy.json remembers leaving uuid.
func departureOnDisk(t *testing.T, uuid string) bool {
	t.Helper()
	path, err := strategy.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Departures map[string]json.RawMessage `json:"departures"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	_, ok := doc.Departures[uuid]
	return ok
}

// THE REPAIR. A tick that finds the item on one account and the file on
// another puts the item's account -- the tie goes to the item -- in both
// stores and records it, without stamping a cooldown: it is a sync, not a
// move. And it writes only the store that differed.
func TestATickRepairsASplitLoginTowardsTheItem(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-2")
	recordActive(t, "u-1")
	splitReadings(t)

	e := engineFor(t, tokensAreFine, noFetch)
	var log logLines
	log.attach(e)
	tick(t, e)

	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the file was not repaired to the item's account:\n%s", fileNow(t))
	}
	if secret, _ := kc.Secret(); !holds(secret, "u-2") {
		t.Fatalf("the item no longer holds u-2: %s", secret)
	}
	if n := kc.Writes(); n != 0 {
		t.Fatalf("the item, which already held u-2, was written %d times", n)
	}
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if at, _ := lastSwitchOnDisk(t); !at.IsZero() {
		t.Fatal("a sync repair stamped the cooldown")
	}
	if !log.contains("login is split") || !log.contains("u-1@example.com") || !log.contains("u-2@example.com") {
		t.Fatalf("the log does not name which store held which account: %q", log.lines)
	}
	if !log.contains("repaired the split login") {
		t.Fatalf("the log does not say the split was repaired: %q", log.lines)
	}
}

// D1. A /login whose keychain write failed lands in the FILE: the newer login,
// and the user's latest decision. The repair goes towards it.
func TestATickRepairsTowardsTheNewerLoginInTheFile(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(8*time.Hour))
	seedAccountExpiring(t, "u-2", "org-2", tickEpoch.Add(time.Hour))
	writeLiveLogin(t, "u-1", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(loginExpiring("u-2", tickEpoch.Add(time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-2")
	// Both have room, so no plan moves the login and only the repair
	// direction decides where it ends up.
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if secret, _ := kc.Secret(); !holds(secret, "u-1") {
		t.Fatalf("the item was not repaired to the newer login in the file: %s", secret)
	}
	if got := activeOnDisk(t); got != "u-1" {
		t.Fatalf("active = %q, want u-1", got)
	}
}

// D4. When the plan wants to LEAVE the account the split is repaired towards,
// its switch is the repair: one write, both stores, with the plan's departure
// and cooldown -- instead of installing a spent account and then holding the
// engine on it with a cooldown.
func TestAPlanThatLeavesTheRepairTargetIsTheRepair(t *testing.T) {
	isolateEngine(t)
	writeConfig(t, "hover = true\n")
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	withItem(t, "u-2")
	recordActive(t, "u-1")
	// The item's account is spent; the file's has room.
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("u-1", usage.Entry{Snapshot: snapshotWith(5), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		c.Put("u-2", usage.Entry{Snapshot: snapshotWith(100), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if !holds(fileNow(t), "u-1") {
		t.Fatalf("the file left the account with room:\n%s", fileNow(t))
	}
	if got := activeOnDisk(t); got != "u-1" {
		t.Fatalf("active = %q, want u-1: the plan's switch is the repair", got)
	}
	if _, to := lastSwitchOnDisk(t); to != "u-1" {
		t.Fatalf("cooldown stamped for %q, want the plan's switch to u-1", to)
	}
	if !departureOnDisk(t, "u-2") {
		t.Fatal("the plan's departure from the spent account was not recorded")
	}
}

// A machine whose stores and record agree is not touched, however the plan
// reads.
func TestATickLeavesAnAgreeingLoginAlone(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-2")
	withItem(t, "u-2")
	recordActive(t, "u-2")
	splitReadings(t)
	before := fileNow(t)

	e := engineFor(t, tokensAreFine, noFetch)
	var log logLines
	log.attach(e)
	tick(t, e)

	if fileNow(t) != before {
		t.Fatalf("an agreeing login was rewritten:\n%s", fileNow(t))
	}
	if log.contains("split") || log.contains("record of the live account") {
		t.Fatalf("an agreeing login was reported as split: %q", log.lines)
	}
}

// A record that only lags behind agreeing stores is corrected with no
// credential write, and the plan is not held off for it.
func TestATickCorrectsARecordBehindAgreeingStores(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-2")
	withItem(t, "u-2")
	recordActive(t, "u-1")
	splitReadings(t)
	before := fileNow(t)

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if fileNow(t) != before {
		t.Fatalf("a record-only correction rewrote the credentials file:\n%s", fileNow(t))
	}
	if at, _ := lastSwitchOnDisk(t); !at.IsZero() {
		t.Fatal("a record-only correction stamped the cooldown; nothing switched")
	}
}

// D5. ONE backoff for a failed credential write, the plan's included: each
// attempt holds Claude Code's credential locks while the keychain is slow. And
// a tick whose read failed does not clear it -- only a write that succeeds does.
func TestAFailedWriteWaitsBeforeTheNextAndAReadFailureDoesNotClearTheWait(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WritesRefused = true
	recordActive(t, "u-1")
	splitReadings(t) // u-1 at 99%: the plan moves to u-2

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }

	if err := e.Tick(context.Background()); err == nil {
		t.Fatal("a switch whose keychain write was refused reported success")
	}
	e.Wait()
	if n := kc.Writes(); n != 1 {
		t.Fatalf("writes = %d after the first tick, want 1", n)
	}
	// A tick whose read fails, then one whose read succeeds, inside the wait.
	kc.FailReads(true)
	_ = e.Tick(context.Background())
	e.Wait()
	kc.FailReads(false)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("a tick inside the wait failed: %v", err)
	}
	e.Wait()
	if n := kc.Writes(); n != 1 {
		t.Fatalf("writes = %d: the switch was retried inside the wait, after a read failure", n)
	}
	now = now.Add(failedWriteRetry)
	kc.Heal()
	tick(t, e)
	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the switch was not retried once the wait was over:\n%s", fileNow(t))
	}
}

// THE INCIDENT, END TO END, and the departure the switch named. The plan moves
// u-1 (spent) to u-2; the keychain write lands and runs out of time, and the
// read-back fails too. The switch claims neither outcome. When the keychain
// answers again -- after the write backoff -- the next tick finds the item on
// u-2 and the file on u-1, and the repair completes THAT switch: cooldown and
// departure recorded as the plan named them.
func TestAnUnconfirmedSwitchIsCompletedByTheRepairWithItsDeparture(t *testing.T) {
	isolateEngine(t)
	writeConfig(t, "hover = true\n")
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	recordActive(t, "u-1")
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("u-1", usage.Entry{Snapshot: snapshotWith(100), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		c.Put("u-2", usage.Entry{Snapshot: snapshotWith(5), FetchedAt: tickEpoch.Add(-time.Minute), NextPollAt: tickEpoch.Add(time.Hour)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	err := e.Tick(context.Background())
	e.Wait()
	if !errors.Is(err, cclink.ErrLoginOutcomeUnknown) {
		t.Fatalf("Tick = %v, want ErrLoginOutcomeUnknown: the outcome could not be confirmed", err)
	}
	if got := activeOnDisk(t); got != "u-1" {
		t.Fatalf("active = %q: an unconfirmed switch was recorded", got)
	}
	if !holds(fileNow(t), "u-1") {
		t.Fatalf("the file moved past an unconfirmed keychain write:\n%s", fileNow(t))
	}

	kc.Heal()
	now = now.Add(failedWriteRetry)
	tick(t, e)
	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the next tick did not repair the file to the item's account:\n%s", fileNow(t))
	}
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2 after the repair", got)
	}
	if _, to := lastSwitchOnDisk(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2: the repair completed an engine switch", to)
	}
	if !departureOnDisk(t, "u-1") {
		t.Fatal("the departure the unconfirmed switch named was not recorded by the repair that completed it")
	}
}

// D1. A file this store cannot name beside a different login in the item is
// not "fine". The oracle names it; a NEWER login of a managed account is
// adopted into the store, and the pass after repairs towards it.
func TestAnUnnamedNewerFileIsAdoptedAndRepairedTowards(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Hour))
	seedAccount(t, "u-2", "org-2")
	// Claude Code refreshed u-1 and wrote the new pair to the FILE, its
	// keychain write having failed; the item still holds u-2.
	writeLiveLoginRT(t, "RT-u-1-refreshed", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(loginExpiring("u-2", tickEpoch.Add(time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-2")
	readings(t, map[string]float64{"u-1": 36, "u-2": 40})

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-refreshed" {
			return "u-1", nil
		}
		return "", errors.New("unknown token")
	}
	var log logLines
	log.attach(e)
	tick(t, e)
	if !log.contains("newer login of") {
		t.Fatalf("the newer login in the file was not adopted: %q", log.lines)
	}
	tick(t, e)
	if secret, _ := kc.Secret(); !holds(secret, "u-1-refreshed") {
		t.Fatalf("the item was not repaired to the newer login: %s", secret)
	}
}

// D1. An unnamed file the oracle cannot answer for is left alone and said,
// once; nothing is written over a login nobody has named.
func TestAnUnnamedFileTheOracleCannotNameIsLeftAlone(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	writeLiveLoginRT(t, "RT-stranger", tickEpoch.Add(8*time.Hour))
	withItem(t, "u-2")
	recordActive(t, "u-2")
	splitReadings(t)
	before := fileNow(t)

	e := engineFor(t, tokensAreFine, noFetch)
	var log logLines
	log.attach(e)
	tick(t, e)
	tick(t, e)

	if fileNow(t) != before {
		t.Fatalf("a login nobody named was written over:\n%s", fileNow(t))
	}
	if !log.contains("cannot name") {
		t.Fatalf("the unnamed login was not reported: %q", log.lines)
	}
}

// D1. A login of a managed account that is OLDER than the stored one is not
// adopted: its refresh token is already spent, and storing it over the newer
// one would undo the adoption the other store had just made. The oracle still
// names it, from memory, without asking again.
func TestAnOlderLoginIsNamedButNotAdopted(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(8*time.Hour))
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	var older cclink.Blob
	if err := json.Unmarshal([]byte(blobExpiring("RT-u-1-older", tickEpoch.Add(time.Hour))), &older); err != nil {
		t.Fatal(err)
	}
	asks := 0
	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { asks++; return "u-1", nil }

	if _, verdict := e.resolveStore(context.Background(), s, older); verdict != liveOwned {
		t.Fatalf("verdict = %v, want liveOwned: the login is older than the stored one", verdict)
	}
	stored, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if cclink.LoginIdentity(stored) != "refresh:RT-u-1" {
		t.Fatalf("the older login was adopted over the newer one: %s", cclink.LoginIdentity(stored))
	}
	if uuid, ok := e.ownerOf(cclink.LoginIdentity(older)); !ok || uuid != "u-1" {
		t.Fatalf("the oracle's answer was not remembered: %q %v", uuid, ok)
	}
	e.nextResolveAt = time.Time{}
	_, _ = e.resolveStore(context.Background(), s, older)
	if asks != 1 {
		t.Fatalf("the oracle was asked %d times about one login", asks)
	}
}
