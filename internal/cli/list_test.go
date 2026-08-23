package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// An empty store is exit 0: "no accounts yet" is a fact, not a failure, and the
// notice is a notice, so it belongs on stderr.
func TestListEmptyStoreExitsZeroWithTheNoticeOnStderr(t *testing.T) {
	isolate(t)

	code, out, errOut, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 for an empty store (%s)", code, top)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: 'no accounts yet' is a notice, not data", out)
	}
	if !strings.Contains(errOut, "No accounts yet") {
		t.Fatalf("stderr = %q, want the notice", errOut)
	}
}

func TestListEmptyStoreJSONIsOneDocument(t *testing.T) {
	isolate(t)

	code, out, errOut, _ := runRoot(t, "list", "--json")
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

	code, out, _, top := runRoot(t, "list", "--json")
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

// The human table goes to stdout; nothing else may, or `ccdad list --json`
// would emit two documents.
func TestListHumanOutputMentionsTheAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, out, _, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if !strings.Contains(out, "a@example.com") {
		t.Fatalf("list output does not mention the account:\n%s", out)
	}
}

// A store whose accounts are all disabled must say so, not print a bare header
// row with nothing under it.
func TestListWithEveryAccountDisabledSaysSo(t *testing.T) {
	isolate(t)
	seedDisabledAccount(t, "u-1", "a@example.com")

	code, out, errOut, top := runRoot(t, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no table at all", out)
	}
	if !strings.Contains(errOut, "--all") {
		t.Fatalf("stderr = %q, want it to point at --all", errOut)
	}
	if _, out, _, _ := runRoot(t, "list", "--all"); !strings.Contains(out, "a@example.com") {
		t.Fatalf("list --all does not show the disabled account:\n%s", out)
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

	_, out, _, _ := runRoot(t, "list")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "b@example.com") && !strings.HasPrefix(line, "*") {
			t.Fatalf("the live account is not marked:\n%s", out)
		}
		if strings.Contains(line, "a@example.com") && strings.HasPrefix(line, "*") {
			t.Fatalf("a non-live account is marked:\n%s", out)
		}
	}

	_, jsonOut, _, _ := runRoot(t, "list", "--json")
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
	if err := s.Add(store.Account{UUID: uuid, Email: email, Kind: kind, Tier: tier},
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

	_, out, _, top := runRoot(t, "list")
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

// `list`'s usage columns. They read the same cache `status` reads, which is
// what "`ccdad list` and `ccdad status --json` can never disagree" rests on,
// and the binding window is the one with LEAST left rather than the first one
// the response happened to carry.
func TestListShowsTheHeadroomAndResetOfTheBindingWindow(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")
	// five_hour has more left, so seven_day binds. A renderer that took the
	// first window in schema order would pass a fixture where the two agreed.
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, listNow.Add(30*time.Minute)),
			SevenDay: window(62, listNow.Add(2*time.Hour+14*time.Minute)),
		},
	})

	_, out, _, top := runRoot(t, "list")
	row := rowFor(t, out, "a@example.com")
	// 38% LEFT, not the 62% used and not five_hour's 80.
	if !strings.Contains(row, "38%") {
		t.Errorf("the row does not carry the binding window's headroom (%s):\n%s", top, row)
	}
	if strings.Contains(row, "80%") {
		t.Errorf("the row reports five_hour, which is not the binding window:\n%s", row)
	}
	if !strings.Contains(row, "2h14m") {
		t.Errorf("the row does not carry the binding window's reset:\n%s", row)
	}
}

// Unknown is never read as zero, and the defect that parked cswap's engine: an
// account nobody could read is not an account at 0%.
func TestListRendersAnUnreadableAccountAsAQuestionMark(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")

	_, out, _, _ := runRoot(t, "list")
	row := rowFor(t, out, "a@example.com")
	if !strings.Contains(row, "?") {
		t.Errorf("an account with no reading does not render '?':\n%s", row)
	}
	if strings.Contains(row, "0%") {
		t.Errorf("an account with no reading renders as 0%%:\n%s", row)
	}
}

// `ccdad list` and `ccdad status --json` can never disagree. The strongest form
// of that is the one asserted here — the two commands emit the SAME usage
// object, because they build it from the same cache through the same code.
func TestListJSONCarriesTheSameUsageObjectStatusDoes(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	stubDaemon(t, daemon.Report{}, nil)
	seedAccount(t, "u-1", "a@example.com")
	seedUsageEntry(t, "u-1", usage.Entry{
		FetchedAt: listNow.Add(-90 * time.Second),
		Snapshot: &usage.Snapshot{
			FiveHour: window(20, listNow.Add(30*time.Minute)),
			SevenDay: window(62, listNow.Add(2*time.Hour+14*time.Minute)),
		},
	})

	_, listOut, _, _ := runRoot(t, "list", "--json")
	_, statusOut, _, _ := runRoot(t, "status", "--json")

	fromList := accountRow(t, statusJSON(t, listOut), "u-1")["usage"]
	fromStatus := accountRow(t, statusJSON(t, statusOut), "u-1")["usage"]
	if fromList == nil {
		t.Fatalf("list --json carries no usage object:\n%s", listOut)
	}
	if !reflect.DeepEqual(fromList, fromStatus) {
		t.Fatalf("list and status describe one reading two ways:\nlist:   %v\nstatus: %v",
			fromList, fromStatus)
	}
}

// An account with no reading carries no `usage` key at all, for the same reason
// the table prints "?": a consumer that saw an object of zeros could not tell it
// from an account at 0%.
func TestListJSONOmitsUsageForAnAccountWithNoReading(t *testing.T) {
	isolate(t)
	freezeClock(t, listNow)
	seedAccount(t, "u-1", "a@example.com")

	_, out, _, _ := runRoot(t, "list", "--json")
	if row := accountRow(t, statusJSON(t, out), "u-1"); row["usage"] != nil {
		t.Fatalf("usage = %v, want the key absent when there is no reading", row["usage"])
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

	_, out, _, _ := runRoot(t, "list")
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

	code, out, _, top := runRoot(t, "list", "--refresh")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, top)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", calls.Load())
	}
	if row := rowFor(t, out, "a@example.com"); !strings.Contains(row, "75%") {
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

	runRoot(t, "list")
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

	runRoot(t, "list", "--refresh")
	now = listNow.Add(pollpolicy.ServeTTL - time.Second)
	_, _, errOut, _ := runRoot(t, "list", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — the second --refresh re-fetched inside the TTL", calls.Load())
	}
	if !strings.Contains(errOut, "Nothing needed refreshing") {
		t.Fatalf("stderr = %q, want it to say why nothing was fetched", errOut)
	}

	now = listNow.Add(pollpolicy.ServeTTL)
	runRoot(t, "list", "--refresh")
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

	runRoot(t, "list", "--refresh")
	now = listNow.Add(pollpolicy.ServeTTL + time.Minute)

	code, _, errOut, top := runRoot(t, "list", "--refresh")
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

	code, out, errOut, top := runRoot(t, "list", "--refresh", "--json")
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
func TestListRefreshLeavesDisabledAccountsAloneUnlessAllIsAsked(t *testing.T) {
	isolate(t)
	now := listNow
	movableClock(t, &now)
	seedAccount(t, "u-1", "a@example.com")
	seedDisabledAccount(t, "u-2", "b@example.com")
	calls := stubRefresh(t, func() (*usage.Snapshot, error) {
		return snapshotUsing(now, 25), nil
	})

	runRoot(t, "list", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — the disabled account is not on the listing", calls.Load())
	}

	now = listNow.Add(pollpolicy.ServeTTL)
	runRoot(t, "list", "--all", "--refresh")
	if calls.Load() != 3 {
		t.Fatalf("fetches = %d, want 3 — --all puts the disabled account back on the listing", calls.Load())
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

	_, _, errOut, _ := runRoot(t, "list", "--refresh")
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

	_, _, errOut, _ := runRoot(t, "list", "--refresh")
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 — one account was inside its TTL and one was not", calls.Load())
	}
	if strings.Contains(errOut, "Nothing needed refreshing") {
		t.Fatalf("stderr = %q, want no such claim after a real fetch", errOut)
	}
}

// `auto` and `switch` both tell a user to run `ccdad list --refresh` when the
// cache is empty or stale. That advice named a flag the binary rejected before
// this landed, and the sentences had to be pulled out of both files to stop
// sending people to a usage error. This is what makes putting them back safe:
// it fails if the flag ever leaves again, instead of the advice going stale in
// silence.
func TestTheAdviceToRunListRefreshNamesAFlagThatExists(t *testing.T) {
	root := NewRootCmd()

	var list *cobra.Command
	advice := map[string]string{}
	for _, c := range root.Commands() {
		switch c.Name() {
		case "list":
			list = c
		case "auto", "switch":
			advice[c.Name()] = c.Long
		}
	}
	if list == nil {
		t.Fatal("there is no list command")
	}
	if list.Flags().Lookup("refresh") == nil {
		t.Fatal("`list` has no --refresh flag, and two commands tell users to run it")
	}
	for name, long := range advice {
		if !strings.Contains(long, "ccdad list --refresh") {
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

	code, out, _, top := runRoot(t, "list", "--json")
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
