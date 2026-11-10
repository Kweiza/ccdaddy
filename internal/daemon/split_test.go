package daemon

import (
	"context"
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

// THE REPAIR. A tick that finds the item on one account and the file on
// another puts the item's account in both stores and records it -- with the
// cooldown a normal switch stamps -- and says so in the log.
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
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2", got)
	}
	if _, to := lastSwitchOnDisk(t); to != "u-2" {
		t.Fatalf("cooldown stamped for %q, want u-2: a repair records like a switch", to)
	}
	if !log.contains("login is split") || !log.contains("u-1@example.com") || !log.contains("u-2@example.com") {
		t.Fatalf("the log does not name which store held which account: %q", log.lines)
	}
	if !log.contains("repaired the split login") {
		t.Fatalf("the log does not say the split was repaired: %q", log.lines)
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

// A repair that fails is not retried at the tick rate: every attempt is a
// keychain write, and the failure that matters is a keychain too slow to
// answer.
func TestAFailedRepairWaitsBeforeTryingAgain(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-2")
	kc.WritesRefused = true
	recordActive(t, "u-1")
	splitReadings(t)

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }

	if err := e.Tick(context.Background()); err == nil {
		t.Fatal("a repair whose write failed reported success")
	}
	e.Wait()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("the repair was retried on the very next tick: %v", err)
	}
	e.Wait()
	now = now.Add(splitRepairRetry)
	kc.Heal()
	tick(t, e)
	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the repair was not retried once the wait was over:\n%s", fileNow(t))
	}
}

// THE INCIDENT, END TO END. The plan moves chan (99%) to support; the keychain
// write lands and runs out of time, and the read-back fails too. The switch
// claims neither outcome. When the keychain answers again the next tick finds
// the item on support and the file on chan, and repairs towards support.
func TestAnUnconfirmedSwitchIsRepairedOnTheNextTick(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	kc := withItem(t, "u-1")
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	recordActive(t, "u-1")
	splitReadings(t)

	e := engineFor(t, tokensAreFine, noFetch)
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
	tick(t, e)
	if !holds(fileNow(t), "u-2") {
		t.Fatalf("the next tick did not repair the file to the item's account:\n%s", fileNow(t))
	}
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q, want u-2 after the repair", got)
	}
}
