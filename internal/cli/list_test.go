package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// An empty store is exit 0: "no accounts yet" is a fact, not a failure, and the
// notice is a notice, so it belongs on stderr.
func TestListEmptyStoreExitsZeroWithTheNoticeOnStderr(t *testing.T) {
	isolate(t)

	code, out, errOut, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 for an empty store (%s)", code, top)
	}
	if !strings.Contains(out, "Daemon:") {
		t.Fatalf("stdout = %q, want the daemon summary even when there are no accounts", out)
	}
	if !strings.Contains(errOut, "No accounts yet") {
		t.Fatalf("stderr = %q, want the notice", errOut)
	}
}

func TestListEmptyStoreJSONIsOneDocument(t *testing.T) {
	isolate(t)

	code, out, errOut, _ := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
	var payload struct {
		SchemaVersion int              `json:"schemaVersion"`
		Accounts      []map[string]any `json:"accounts"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatal("--json emitted more than one document on stdout")
	}
	if payload.SchemaVersion != 1 || payload.Accounts == nil || len(payload.Accounts) != 0 {
		t.Fatalf("payload = %+v, want schemaVersion 1 and an empty (not null) accounts array", payload)
	}
}

func TestListJSONCarriesSchemaVersionAndUUIDs(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Accounts      []struct {
			UUID  string `json:"uuid"`
			Idx   int    `json:"idx"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if payload.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", payload.SchemaVersion)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].UUID != "u-1" {
		t.Fatalf("accounts = %+v", payload.Accounts)
	}
}

// The human table goes to stdout; nothing else may, or `ccdad status --json`
// would emit two documents.
func TestListHumanOutputMentionsTheAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if !strings.Contains(out, "a@example.com") {
		t.Fatalf("list output does not mention the account:\n%s", out)
	}
}

// Unified status keeps disabled accounts visible and labels why they are not
// part of rotation.
func TestListWithEveryAccountDisabledSaysSo(t *testing.T) {
	isolate(t)
	seedDisabledAccount(t, "u-1", "a@example.com")

	code, out, errOut, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if !strings.Contains(out, "a@example.com") || !strings.Contains(out, "(disabled)") {
		t.Fatalf("status does not show and label the disabled account:\n%s", out)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want no hidden-account advice from unified status", errOut)
	}
}

// Which account is live is the whole point of the listing. Nothing asserted the
// `*` marker, the per-row `active` field or `activeUuid`, so the entire
// attribution step could be deleted with the suite green.
func TestListMarksTheActiveAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	_, out, _, _ := runRoot(t, "status")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(ansi.Strip(line))
		if strings.Contains(line, "b@example.com") && len(fields) >= 3 && fields[0] != "*" {
			t.Fatalf("the live account is not marked:\n%s", out)
		}
		if strings.Contains(line, "a@example.com") && len(fields) >= 3 && fields[0] == "*" {
			t.Fatalf("a non-live account is marked:\n%s", out)
		}
	}

	_, jsonOut, _, _ := runRoot(t, "status", "--json")
	var payload struct {
		ActiveUUID string `json:"activeUuid"`
		Accounts   []struct {
			UUID   string `json:"uuid"`
			Active bool   `json:"active"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ActiveUUID != "u-2" {
		t.Fatalf("activeUuid = %q, want u-2", payload.ActiveUUID)
	}
	for _, a := range payload.Accounts {
		if (a.UUID == "u-2") != a.Active {
			t.Fatalf("account %s active = %v, want it true only for u-2", a.UUID, a.Active)
		}
	}
}

// listNow is the clock every dated test in this file runs against.
var listNow = mustTime("2026-08-22T12:00:00Z")

// seedTieredAccount stores an account whose TYPE and TIER are not the zero
// values, which is what it takes to assert those two columns at all.
func seedTieredAccount(t *testing.T, uuid, email string, kind identity.Kind, tier string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: email, Kind: kind, Tier: tier},
		credsFor("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
}

// The listing's own columns. Nothing asserted TYPE or TIER, so both could be
// dropped from the row with the suite green — and TYPE is the column that says
// an account is metered in money rather than in windows.
func TestListShowsTheTypeAndTierOfEachAccount(t *testing.T) {
	isolate(t)
	seedTieredAccount(t, "u-1", "a@example.com", identity.KindCredit, "claude_max")
	seedTieredAccount(t, "u-2", "b@example.com", identity.KindSubscription, "")

	_, out, _, top := runRoot(t, "status")
	if !strings.Contains(out, "TYPE") || !strings.Contains(out, "TIER") {
		t.Fatalf("the header has no TYPE/TIER columns (%s):\n%s", top, out)
	}
	credit := rowFor(t, out, "a@example.com")
	if !strings.Contains(credit, "credit") {
		t.Errorf("the credit account's TYPE column is missing:\n%s", credit)
	}
	if !strings.Contains(credit, "claude_max") {
		t.Errorf("the account's TIER column is missing:\n%s", credit)
	}
	// An unreported tier is a dash, never blank: a blank cell reads as a
	// rendering bug rather than as "the profile did not say".
	if plain := rowFor(t, out, "b@example.com"); !strings.Contains(plain, "-") {
		t.Errorf("an account with no tier does not render a dash:\n%s", plain)
	}
}

// rowFor is the one line of a table that mentions label.
func rowFor(t *testing.T, out, label string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	t.Fatalf("no row for %s in:\n%s", label, out)
	return ""
}

// Status usage columns read the same cache as its JSON form, and the binding
// window is the one with LEAST left rather than the first one the response
// happened to carry.
// The row shows EVERY window the account carries, and derives none of them.
// This replaces a test that asserted the opposite -- that only the binding
// window reached the table -- which is the behaviour a reader could not tell
// apart from `ccdad status` naming a different one.
func TestListShowsEveryWindowTheAccountCarries(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, listNow.Add(30*time.Minute)),
			SevenDay: window(62, listNow.Add(2*time.Hour+14*time.Minute)),
		},
	})

	_, out, _, top := runRoot(t, "status")
	row := rowFor(t, out, "a@example.com")
	// Both windows, as USED, and both rollovers. Neither is chosen over the
	// other and neither is 100 minus the other.
	for _, want := range []string{"20%", "62%", "30m", "2h14m"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row does not carry %q (%s):\n%s", want, top, row)
		}
	}
	// The headers name the windows, and the legend maps them back to the wire
	// keys `ccdad config` takes a threshold on.
	for _, want := range []string{"5H", "7D", "5H = five_hour", "7D = seven_day"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not carry %q:\n%s", want, out)
		}
	}
}

// Unknown is never read as zero, and the defect that parked cswap's engine: an
// account nobody could read is not an account at 0%.
func TestListRendersAnUnreadableAccountAsAQuestionMark(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")

	_, out, _, _ := runRoot(t, "status")
	row := rowFor(t, out, "a@example.com")
	if !strings.Contains(row, "?") {
		t.Errorf("an account with no reading does not render '?':\n%s", row)
	}
	if strings.Contains(row, "0%") {
		t.Errorf("an account with no reading renders as 0%%:\n%s", row)
	}
}

// A credit-metered seat — the enterprise and pay-as-you-go accounts KindCredit
// names — carries no five_hour or seven_day window at all, so Headroom is
// never Known for it and LEFT used to print "?" for the whole class rather
// than for the accounts that actually failed to poll. With both money figures
// on the wire, LEFT reports the remaining amount and the used/limit pair.
func TestListShowsRemainingCreditWhenBothFiguresAreKnown(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedTieredAccount(t, "u-1", "a@example.com", identity.KindCredit, "")
	limit, used := 10000.0, 2550.0 // cents: a $100 cap, $25.50 spent
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
				State: usage.ExtraUsageEnabled, Currency: "USD",
				MonthlyLimit: &limit, UsedCredits: &used,
			}),
		},
	})

	// The balance is a line under the table rather than a cell in it: money is
	// not commensurable with a percentage, and a column of one cannot carry the
	// other. What matters is that it is still printed and still exact.
	_, out, _, top := runRoot(t, "status")
	if !strings.Contains(out, "74.50") {
		t.Errorf("the listing does not carry the remaining amount ($100 cap - $25.50 spent) (%s):\n%s", top, out)
	}
	if !strings.Contains(out, "25.50/100.00") {
		t.Errorf("the listing does not carry the used/limit pair:\n%s", out)
	}
}

// An account whose organization set no cap of its own has nothing to show a
// remainder against, so LEFT says what was spent and stops rather than
// inventing a denominator.
func TestListShowsCreditUsedWithNoAccountLimit(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedTieredAccount(t, "u-1", "a@example.com", identity.KindCredit, "")
	used := 500.0 // cents: $5.00 spent, no monthly_limit on the wire
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
				State: usage.ExtraUsageEnabled, Currency: "USD",
				UsedCredits: &used,
			}),
		},
	})

	_, out, _, _ := runRoot(t, "status")
	if !strings.Contains(out, "5.00 used") {
		t.Errorf("the listing does not carry the used amount:\n%s", out)
	}
	if !strings.Contains(out, "no account limit") {
		t.Errorf("the listing does not say there is no account limit:\n%s", out)
	}
}

// An account with no reading carries no `usage` key at all, for the same reason
// the table prints "?": a consumer that saw an object of zeros could not tell it
// from an account at 0%.
func TestListJSONOmitsUsageForAnAccountWithNoReading(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")

	_, out, _, _ := runRoot(t, "status", "--json")
	if row := accountRow(t, statusJSON(t, out), "u-1"); row["usage"] != nil {
		t.Fatalf("usage = %v, want the key absent when there is no reading", row["usage"])
	}
}

// A reading whose organization has overage switched on carries the credit axis
// alongside the window figures, and list --json is where a script reads it:
// there is no other command that prints monthlyLimit or usedCredits at all.
func TestListJSONCarriesTheCreditAxisWhenAReadingHasOne(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	limit, used := 10000.0, 2550.0 // cents: a $100 cap, $25.50 spent
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, listNow.Add(30*time.Minute)),
			ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
				State: usage.ExtraUsageEnabled, Currency: "USD",
				MonthlyLimit: &limit, UsedCredits: &used,
			}),
		},
	})

	_, out, _, _ := runRoot(t, "status", "--json")
	usageObj, _ := accountRow(t, statusJSON(t, out), "u-1")["usage"].(map[string]any)
	credit, _ := usageObj["credit"].(map[string]any)
	if credit == nil {
		t.Fatalf("usage.credit is absent, want the extra_usage axis:\n%s", out)
	}
	if credit["state"] != "enabled" || credit["currency"] != "USD" {
		t.Fatalf("credit = %v, want state=enabled currency=USD", credit)
	}
	// MonthlyLimit and UsedCredits arrive on the wire in cents; the JSON figures
	// are in the currency's major unit, the same conversion max_auto_spend uses.
	if credit["monthlyLimit"] != 100.0 || credit["usedCredits"] != 25.5 {
		t.Fatalf("credit = %v, want monthlyLimit=100 usedCredits=25.5", credit)
	}
}

// An account whose reading never carried extra_usage — no organization ever
// turned overage on for it — must not grow a `credit` key at all: a present
// object, even an empty one, would read as "this account has a credit axis".
func TestListJSONOmitsCreditWhenTheReadingHasNone(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot:  &usage.Snapshot{FiveHour: window(20, listNow.Add(30*time.Minute))},
	})

	_, out, _, _ := runRoot(t, "status", "--json")
	usageObj, _ := accountRow(t, statusJSON(t, out), "u-1")["usage"].(map[string]any)
	if credit, ok := usageObj["credit"]; ok {
		t.Fatalf("usage.credit = %v, want the key absent when the reading carried no extra_usage", credit)
	}
}

// The ACCOUNT column carries both the address and the alias. Account.Label()
// returns the alias ALONE, which is right where one account is being named and
// wrong here: a listing is where a user learns which handle is which address.
// Nothing asserted it, so the column could quietly become one or the other.
func TestListShowsBothTheEmailAndTheAlias(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if code, _, _, top := runRoot(t, "alias", "1", "work"); code != ExitOK {
		t.Fatalf("alias = %d (%s)", code, top)
	}

	_, out, _, _ := runRoot(t, "status")
	row := rowFor(t, out, "a@example.com")
	if !strings.Contains(row, "work") {
		t.Fatalf("the ACCOUNT column carries only one of the address and the alias:\n%s", row)
	}
}

// movableClock is freezeClock with a hand on it, which every serveTTL and
// post-429 assertion needs: both floors are spans, so a frozen clock can never
// leave one.
func movableClock(t *testing.T, at *time.Time) {
	t.Helper()
	saved := timeNow
	t.Cleanup(func() { timeNow = saved })
	timeNow = func() time.Time { return *at }
}

// stubRefresh replaces the poller `--refresh` borrows with one that answers
// from the test. The engine's clock is the command's, so a test that moves one
// moves both.
func stubRefresh(t *testing.T, fetch func() (*usage.Snapshot, error)) *atomic.Int64 {
	t.Helper()
	calls := &atomic.Int64{}
	saved := newEngine
	t.Cleanup(func() { newEngine = saved })
	newEngine = func() *daemon.Engine {
		e := daemon.NewEngine()
		e.Now = func() time.Time { return timeNow() }
		e.Rand = func() float64 { return 0.5 }
		e.AccessToken = func(context.Context, string) (string, error) { return "AT", nil }
		e.FetchUsage = func(context.Context, string) (*usage.Snapshot, error) {
			calls.Add(1)
			return fetch()
		}
		return e
	}
	return calls
}

func snapshotUsing(now time.Time, used float64) *usage.Snapshot {
	resets := now.Add(90 * time.Minute)
	return &usage.Snapshot{FiveHour: usage.NewWindow(&used, &resets)}
}

// The one exception to "list never fetches".
func TestListRefreshFetchesAndShowsTheFreshReading(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	code, out, _, top := runRoot(t, "status", "--refresh")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", calls.Load())
	}
	if row := rowFor(t, out, "a@example.com"); !strings.Contains(row, "25%") {
		t.Fatalf("the row does not show the reading just taken:\n%s", row)
	}
	// And it landed in the SAME cache the daemon and `status` read.
	if _, statusOut, _, _ := runRoot(t, "status"); !strings.Contains(statusOut, "25%") {
		t.Fatalf("status cannot see the refreshed reading:\n%s", statusOut)
	}
}

// The plain form still never fetches. This is the whole reason `list` is safe
// to run in a loop.
func TestListWithoutRefreshNeverFetches(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	runRoot(t, "status")
	if calls.Load() != 0 {
		t.Fatalf("fetches = %d, want 0 — `list` fetched without being asked", calls.Load())
	}
}

// The poll policy's serveTTL, on the hand-held path. The endpoint's allowance
// is a sliding window, so a user leaning on this flag must not be able to
// saturate an identity for an hour.
func TestListRefreshInsideTheServeTTLMakesNoSecondRequest(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	runRoot(t, "status", "--refresh")
	now = listNow.Add(pollpolicy.ServeTTL - time.Second)
	_, _, errOut, _ := runRoot(t, "status", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — the second --refresh re-fetched inside the TTL", calls.Load())
	}
	if !strings.Contains(errOut, "Nothing needed refreshing") {
		t.Fatalf("stderr = %q, want it to say why nothing was fetched", errOut)
	}

	now = listNow.Add(pollpolicy.ServeTTL)
	runRoot(t, "status", "--refresh")
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2 once the TTL has elapsed", calls.Load())
	}
}

// The floor a 429 earns outlives the serveTTL, and the user is told how long.
func TestListRefreshHonoursThePost429FloorAndSaysSo(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return nil, &usage.StatusError{Status: 429}
	})

	runRoot(t, "status", "--refresh")
	now = listNow.Add(pollpolicy.ServeTTL + time.Minute)

	code, _, errOut, top := runRoot(t, "status", "--refresh")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0 — the listing still rendered", code, top)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — the second --refresh went through the backoff", calls.Load())
	}
	if !strings.Contains(errOut, "rate-limited") {
		t.Fatalf("stderr = %q, want it to name the rate limit", errOut)
	}
	// 540 s of backoff, 240 s of it already elapsed: five minutes to go.
	if !strings.Contains(errOut, "5m") {
		t.Fatalf("stderr = %q, want it to say how long the hold has left", errOut)
	}
}

// Every notice --refresh produces is a notice, so `--json` still puts exactly
// one document on stdout.
func TestListRefreshJSONIsStillOneDocument(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	stubRefresh(t, func() (*usage.Snapshot, error) {
		return nil, errors.New("the endpoint is having a bad day")
	})

	code, out, errOut, top := runRoot(t, "status", "--refresh", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("--refresh --json emitted more than one document on stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "could not be refreshed") {
		t.Fatalf("stderr = %q, want the failure reported there", errOut)
	}
}

// --refresh spends a rate-limited, per-identity allowance, so it spends it on
// the rows it is about to print and on no others.
func TestStatusRefreshLeavesDisabledAccountsAlone(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	seedDisabledAccount(t, "u-2", "b@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	runRoot(t, "status", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — the disabled account is not on the listing", calls.Load())
	}

	now = listNow.Add(pollpolicy.ServeTTL)
	runRoot(t, "status", "--refresh")
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want the enabled account refreshed twice and the disabled account never polled", calls.Load())
	}
}

// An account with no OAuth grant behind it can never be polled, and saying so
// on every listing would be noise: `ccdad add-token` accounts are the ordinary
// case, and the '?' plus the api-key TYPE already says it.
func TestListRefreshIsQuietAboutAnAccountItCannotPoll(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAPIKeyAccount(t, "u-1", "a@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		t.Error("an api-key account was polled; it has no refresh grant behind it")
		return nil, errors.New("unreachable")
	})

	_, _, errOut, _ := runRoot(t, "status", "--refresh")
	if calls.Load() != 0 {
		t.Fatalf("fetches = %d, want 0", calls.Load())
	}
	if strings.Contains(errOut, "could not be refreshed") {
		t.Fatalf("stderr = %q, want no failure for an account that never had a grant", errOut)
	}
}

// "Nothing needed refreshing" answers "why are these numbers the same as a
// minute ago". Said after a fetch that DID happen it is simply false, and the
// mixed case — one account inside its TTL, one outside — is the only fixture
// that can tell the two conditions apart.
func TestListRefreshDoesNotSayNothingWasNeededWhenSomethingWasFetched(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-time.Second),
		Snapshot:  snapshotUsing(listNow, 10),
	})
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	_, _, errOut, _ := runRoot(t, "status", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — one account was inside its TTL and one was not", calls.Load())
	}
	if strings.Contains(errOut, "Nothing needed refreshing") {
		t.Fatalf("stderr = %q, want no such claim after a real fetch", errOut)
	}
}

// `auto` and `switch` tell a user to run `ccdad status --refresh` when the
// cache is empty or stale. This keeps that advice tied to a real flag.
func TestTheAdviceToRunStatusRefreshNamesAFlagThatExists(t *testing.T) {
	root := NewRootCmd()

	var status *cobra.Command
	advice := map[string]string{}
	for _, c := range root.Commands() {
		switch c.Name() {
		case "status":
			status = c
		case "auto", "switch":
			advice[c.Name()] = c.Long
		}
	}
	if status == nil {
		t.Fatal("there is no status command")
	}
	if status.Flags().Lookup("refresh") == nil {
		t.Fatal("`status` has no --refresh flag, and two commands tell users to run it")
	}
	for name, long := range advice {
		if !strings.Contains(long, "ccdad status --refresh") {
			t.Errorf("`%s --help` no longer points at the one flag that can freshen the cache:\n%s",
				name, long)
		}
	}
}

// The same unknown-key probe, on the other read command a supervisor scripts.
// `list` and `which` publish it independently, so pinning one leaves the other
// free to drop it.
func TestListJSONCarriesTheUnknownKeyProbe(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	writeLiveFile(t, liveLoginJSON("RT-u-1", `"somethingNew":{"a":1}`))

	code, out, _, top := runRoot(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	var payload struct {
		ActiveUUID  string   `json:"activeUuid"`
		UnknownKeys []string `json:"unknownKeys"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if payload.ActiveUUID != "u-1" {
		t.Fatalf("activeUuid = %q, want the live login to be attributed: %s", payload.ActiveUUID, out)
	}
	if len(payload.UnknownKeys) != 1 || payload.UnknownKeys[0] != "somethingNew" {
		t.Fatalf("unknownKeys = %v, want [somethingNew]", payload.UnknownKeys)
	}
}

// The flag has no column of its own, so --json is where a script reads it and
// a parenthesis is where a person does. Both are asserted because they come
// from two different pieces of code, and only one of them is shared with
// `ccdad status`.
func TestListReportsAPrimaryAccount(t *testing.T) {
	isolate(t)
	seedPrimaryCreditAccount(t, "u-1", "seat@example.com")
	seedAccount(t, "u-2", "plain@example.com")

	_, out, _, _ := runRoot(t, "status")
	if !strings.Contains(out, "(primary)") {
		t.Errorf("the listing does not mark the primary account:\n%s", out)
	}

	_, jsonOut, _, _ := runRoot(t, "status", "--json")
	payload := statusJSON(t, jsonOut)
	if got := accountRow(t, payload, "u-1")["primary"]; got != true {
		t.Errorf("u-1 primary = %v, want true", got)
	}
	// Omitted rather than false for an ordinary account, which is the shape
	// `disabled` already has: a key present on every row is a key a consumer
	// reads as carrying information.
	if _, present := accountRow(t, payload, "u-2")["primary"]; present {
		t.Errorf("an ordinary account carries a primary key: %v", accountRow(t, payload, "u-2"))
	}
}

// Both flags at once, which is the case the suffix block was rewritten for. The
// two say opposite things — primary is "ranked beside the subscriptions",
// disabled is "left out of rotation entirely" — so a listing that printed only
// the first one it found would hide whichever one the reader came looking for,
// half the time. Nothing asserted the disabled suffix at all before this.
func TestListReportsBothPerAccountFlagsAtOnce(t *testing.T) {
	isolate(t)
	seedPrimaryCreditAccount(t, "u-1", "seat@example.com")
	if code, _, _, top := runRoot(t, "disable", "1"); code != ExitOK {
		t.Fatalf("disable = %d (%s)", code, top)
	}

	_, out, _, _ := runRoot(t, "status")
	if !strings.Contains(out, "(primary, disabled)") {
		t.Fatalf("the listing does not carry both flags:\n%s", out)
	}

	// And the ordinary single-flag rendering is still exactly one word in
	// parentheses, with no stray separator.
	isolate(t)
	seedDisabledAccount(t, "u-2", "held@example.com")
	if _, out, _, _ := runRoot(t, "status"); !strings.Contains(out, "(disabled)") {
		t.Fatalf("the listing does not mark a disabled account:\n%s", out)
	}
}

// Status measures usage against thresholds hover derived. A reader who set
// threshold = 80 and sees a row held to 93 needs the applied threshold beside
// the reading to distinguish that policy from a defect.
//
// The note is on stderr, so a piped listing is unchanged, and it is human-only:
// --json already carries the real per-row windowThreshold, which is a better
// answer than a sentence about a column that document does not have.
func TestListSaysWhenItsNumbersCameFromHover(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot:  &usage.Snapshot{SevenDay: window(62, listNow.Add(2*time.Hour))},
	})

	const note = "hover:"
	if _, stdout, _, _ := runRoot(t, "status"); strings.Contains(stdout, note) {
		t.Errorf("status credits hover with hover off:\n%s", stdout)
	}

	if code, _, errOut, _ := runRoot(t, "strategy", "hover"); code != 0 {
		t.Fatalf("strategy hover exited %v: %s", code, errOut)
	}

	code, stdout, _, _ := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, note) {
		t.Errorf("status does not say its cells use hover thresholds:\n%s", stdout)
	}
	// It names what hover actually moved. There is no single derived column
	// left for it to name -- every window has a cell -- so what hover changes
	// here is the thresholds the cells are coloured against.
	if !strings.Contains(stdout, "threshold") {
		t.Errorf("status credits hover without saying what it moved:\n%s", stdout)
	}

	if _, jsonOut, jsonErr, _ := runRoot(t, "status", "--json"); jsonErr != "" || !strings.Contains(jsonOut, `"hover": true`) {
		t.Errorf("--json does not carry hover as structured data; stdout=%s stderr=%s", jsonOut, jsonErr)
	}
}

// The note has to describe the cells truthfully. A cell is one window's raw
// utilization and no threshold enters it; what hover derives is the number the
// cell is COLOURED against.
//
// The test above pins that the note exists. Neither of them constrains what it
// claims, which is how a note saying the number was "measured against the
// thresholds hover derived" stood while the same command printed a raw figure:
// a reader doing the subtraction the sentence invited gets margin that is not
// there.
func TestTheHoverNoteDoesNotClaimACellIsMeasuredAgainstAThreshold(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot:  &usage.Snapshot{SevenDay: window(62, listNow.Add(2*time.Hour))},
	})
	if code, _, errOut, _ := runRoot(t, "strategy", "hover"); code != 0 {
		t.Fatalf("strategy hover exited %v: %s", code, errOut)
	}

	_, stdout, _, _ := runRoot(t, "status")

	// The arithmetic the note must not misdescribe: the cell is the window's own
	// utilization, with hover on and whatever threshold it derived. If the cell
	// were threshold-relative it would move when the threshold did; it does not,
	// and that is the whole content of the correction.
	if row := rowFor(t, stdout, "a@example.com"); !strings.Contains(row, "62%") {
		t.Fatalf("the cell is not the window's own utilization:\n%s", row)
	}

	// "measured against" is the specific false claim. A note that says the
	// number is relative to a threshold is wrong however it spells it, so the
	// assertion is on the shape of the claim rather than on one sentence.
	for _, wrong := range []string{"LEFT is measured against", "measured against the thresholds"} {
		if strings.Contains(stdout, wrong) {
			t.Errorf("the note still claims a cell is threshold-relative (%q):\n%s", wrong, stdout)
		}
	}
	// It must still say what hover DID move, or it explains nothing. With a
	// cell per window there is no chosen window left to name; what hover
	// derives is the threshold each cell is judged against.
	if !strings.Contains(stdout, "derived per account and window") {
		t.Errorf("the note does not say what hover derives:\n%s", stdout)
	}
}

// The note describes a COLUMN, so a listing that drew no table must not carry
// it. Above "No accounts yet" it explains the provenance of numbers that are not
// there, and the two sentences together read as though something was hidden.
func TestTheHoverNoteIsNotPrintedAboveAnEmptyListing(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	if code, _, errOut, _ := runRoot(t, "strategy", "hover"); code != 0 {
		t.Fatalf("strategy hover exited %v: %s", code, errOut)
	}

	const note = "hover:"
	if _, stdout, _, _ := runRoot(t, "status"); strings.Contains(stdout, note) {
		t.Errorf("an empty status credits hover for a table it never drew:\n%s", stdout)
	}

	// The OTHER empty listing, which leaves by a different return: accounts
	// exist, and every one of them is held out of rotation.
	seedAccount(t, "u-1", "a@example.com")
	if code, _, errOut, _ := runRoot(t, "disable", "a@example.com"); code != 0 {
		t.Fatalf("disable exited %v: %s", code, errOut)
	}
	_, stdout, _, _ := runRoot(t, "status")
	if !strings.Contains(stdout, "(disabled)") {
		t.Fatalf("the disabled account is not visible:\n%s", stdout)
	}
	if !strings.Contains(stdout, note) {
		t.Errorf("a rendered quota table does not explain its hover thresholds:\n%s", stdout)
	}
}

// runwayLineOf is the one summary line out of a rendered page, whichever
// command rendered it. It is a helper rather than an inlined loop because two
// tests below compare the line ACROSS commands, and a comparison whose two
// halves were extracted by two loops would pass on a difference in the loops.
func runwayLineOf(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Runway:") {
			return line
		}
	}
	t.Fatalf("no runway line in:\n%s", out)
	return ""
}

// A page with no rows on it carries no summary either — and neither does a
// page whose every row is out of the rotation.
//
// A measurable fleet behind an EMPTY listing is not a state this build can
// reach: the listing hides only disabled accounts, the measurement covers only
// the accounts the rotation can reach, and a disabled account is in neither. So
// the placement of the print — after the early return, after the table — is
// pinned by TestTheRunwayLinePrecedesTheTable below, and what is pinned here is
// the fail-closed half: a fleet the engine cannot switch to has no runway, and
// --all showing its rows must not put one on the screen.
func TestAnEmptyStoreStillPrintsNoRunwayLine(t *testing.T) {
	t.Run("no accounts at all", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)

		_, out, _, _ := runRoot(t, "status")
		if strings.Contains(out, "Runway:") {
			t.Fatalf("an empty store was given a runway:\n%s", out)
		}
	})

	t.Run("every account disabled, with readings behind them", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedBurningFleet(t)
		for _, idx := range []string{"1", "2"} {
			if code, _, errOut, _ := runRoot(t, "disable", idx); code != ExitOK {
				t.Fatalf("disable %s = %d (%s)", idx, code, errOut)
			}
		}
		// --all is a filter on a listing, so it shows them; it is not a
		// statement about what the engine can do with them. Every one of these
		// seats is held out of rotation by the user, so there is no quota here
		// the fleet can spend and no claim to make about how long it lasts.
		_, all, _, _ := runRoot(t, "status")
		if !strings.Contains(all, "(disabled)") {
			t.Fatalf("the fixture no longer shows disabled accounts, so the assertion below asserts nothing:\n%s", all)
		}
		if strings.Contains(all, "Runway:") {
			t.Errorf("a fleet the rotation cannot reach was given a runway:\n%s", all)
		}

	})
}

// The summary comes after the table, and only when there is a measurement
// behind it.
//
// Placement is not cosmetic here. columns() writes where it is called, so a
// summary written before it would land where list_test.go's rowFor scans for
// account rows and change which line it matches.
func TestTheRunwayLinePrecedesTheTable(t *testing.T) {
	t.Run("with a series", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		seedBurningFleet(t)

		_, out, _, top := runRoot(t, "status")
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		runway, lastRow := -1, -1
		for i, l := range lines {
			switch {
			case strings.HasPrefix(l, "Runway:"):
				runway = i
			case strings.Contains(l, "@example.com"):
				lastRow = i
			}
		}
		if runway < 0 {
			t.Fatalf("no runway line at all (%s):\n%s", top, out)
		}
		if lastRow < 0 {
			t.Fatalf("no account rows to place it against:\n%s", out)
		}
		if runway > lastRow {
			t.Fatalf("the runway summary is not in the status block above the account table:\n%s", out)
		}
		// The span the rates were measured over rides on the line: a verdict
		// from two hours of evidence and one from four support different
		// claims, and only the reader can weigh that. It also pins that a real
		// forecast reached the renderer rather than a zero value, which would
		// print a line saying nothing with perfect confidence.
		if got := lines[runway]; !strings.Contains(got, "basis 2h00m") {
			t.Errorf("the line states no basis: %q", got)
		}
	})

	t.Run("without one", func(t *testing.T) {
		isolate(t)
		freezeClock(t, statusNow)
		// One reading and nothing older than it: the machine that has been
		// recording for ten minutes.
		seedAccountAddedAt(t, "uuid-a", "a@example.com", runwayAddedAt)
		seedUsageEntry(t, "uuid-a", usage.Entry{
			FetchedAt: statusNow,
			Snapshot:  &usage.Snapshot{SevenDay: window(48, runwayWeeklyReset)},
		})

		_, out, _, _ := runRoot(t, "status")
		if !strings.Contains(out, "a@example.com") {
			t.Fatalf("the listing itself is missing, so the absence below is not the one under test:\n%s", out)
		}
		if strings.Contains(out, "Runway:") {
			t.Errorf("a runway was stated with no reading behind it:\n%s", out)
		}

		_, jsonOut, _, _ := runRoot(t, "status", "--json")
		if f, ok := statusJSON(t, jsonOut)["forecast"]; ok {
			t.Errorf("forecast = %v was published on a machine with nothing measured; absent and zero are different answers", f)
		}
	})
}

// A series that cannot be read costs the rates and nothing else, and the
// listing says so out loud. A summary that simply vanished would read as a
// fleet with nothing to report rather than as a file nobody could open, and
// those two want different things from a reader: one is a quiet week, the other
// is a store to go and look at.
func TestListSaysSoWhenTheSeriesCannotBeRead(t *testing.T) {
	isolate(t)
	freezeClock(t, statusNow)
	seedBurningFleet(t)
	// Truncated JSON rather than an unreadable mode: a parse failure is the one
	// a real store reaches after a crash mid-write, and it is the case where
	// every level the table prints is still perfectly readable from the cache.
	if err := os.WriteFile(mustPath(history.Path()), []byte("{\"accounts\":"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut, top := runRoot(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d (%s); a listing renders whatever else is wrong\n%s", code, top, out)
	}
	if !strings.Contains(errOut, history.FileName) {
		t.Errorf("stderr names no file that could not be read:\n%s", errOut)
	}
	if strings.Contains(out, "Runway:") {
		t.Errorf("a runway was stated from a series that could not be read:\n%s", out)
	}
	if !strings.Contains(out, "a@example.com") {
		t.Errorf("the rows the usage cache still answers for were dropped with it:\n%s", out)
	}
}

// `status --refresh` is the command a user reaches for precisely when no daemon
// is running, so it is the only thing that will read a Codex account on that
// machine. Nothing in this package covered that pairing, and deleting the line
// in list.go that wires the read seams left the whole suite green.
//
// The seams are stubbed rather than real: this test spends no token and reaches
// no endpoint. It only asks whether the wiring exists at all.
func TestListRefreshReadsACodexAccountThroughTheStubbedSeams(t *testing.T) {
	isolate(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	movableClock(t, &now)
	seedCodexAccount(t, "cx-1", "codexuser@example.com")

	var reads atomic.Int64
	saved := codexReadSeams
	t.Cleanup(func() { codexReadSeams = saved })
	codexReadSeams = func(*http.Client) (
		func(context.Context, string) (string, string, error),
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error)) {

		return func(context.Context, string) (string, string, error) {
				return "AT-cx-1", "ws-1", nil
			}, func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
				reads.Add(1)
				pct := 40.0
				resets := now.Add(3 * time.Hour)
				snap := &usage.Snapshot{
					CodexPrimary: usage.NewWindowWithLength(&pct, &resets, 5*time.Hour),
				}
				return snap, codexusage.Identity{PlanType: "plus"}, nil
			}
	}

	code, _, _, top := runRoot(t, "status", "--refresh")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if reads.Load() != 1 {
		t.Fatalf("codex reads = %d, want 1 — the codex account was never read", reads.Load())
	}
}
