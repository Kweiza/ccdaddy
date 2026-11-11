package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// loginIssued is a login whose expiry -- which orders logins by when they were
// issued -- is expiresAt, in Claude Code's milliseconds.
func loginIssued(refresh string, expiresAt int64) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(fmt.Sprintf(
		`{"accessToken":"AT","refreshToken":%q,"expiresAt":%d}`, refresh, expiresAt))}
}

// record is the daemon's record (OracleRecord): which logins the identity oracle
// named whose, by identity, and per account the spent and newer logins, by
// identity ("" for a spent login that could not be read).
type record struct {
	named map[string]string
	spent map[string][2]string
	err   error
}

func (r record) LoadError() error { return r.err }

func (r record) NamedLogin(h string) (string, bool) {
	for id, uuid := range r.named {
		if LoginHash(id) == h {
			return uuid, true
		}
	}
	return "", false
}

func (r record) SpentStoredLogin(uuid string) (spent, newer string, ok bool) {
	p, ok := r.spent[uuid]
	if !ok {
		return "", "", false
	}
	if p[0] != "" {
		spent = LoginHash(p[0])
	}
	return spent, LoginHash(p[1]), true
}

func namedAs(byIdentity map[string]string) record { return record{named: byIdentity} }

// THE ONE DECISION every process applies before it presents an account's
// stored refresh token: has a login sitting in one of Claude Code's login
// stores already superseded the stored one? It compares the stores with the
// stored login and the account's superseded history, and takes from the
// identity oracle only the one thing nothing here can derive -- whose a login
// the store does not hold is.
func TestAStoredLoginIsSpentOnlyByANewerNamedLoginOfItsOwnInALiveStore(t *testing.T) {
	stored := loginIssued("RT-stored", 1000)
	newer := loginIssued("RT-newer", 2000)
	a := Account{UUID: "u-1"}
	named := namedAs(map[string]string{"refresh:RT-newer": "u-1"})

	cases := []struct {
		name   string
		a      Account
		stored cclink.Blob
		live   cclink.LoginStores
		named  record
		want   bool
	}{
		{"a newer login of the account in the file", a, stored,
			cclink.LoginStores{File: newer}, named, true},
		{"a newer login of the account in the keychain item", a, stored,
			cclink.LoginStores{HasItem: true, Item: newer, File: stored}, named, true},
		{"the stores agree on the newer login", a, stored,
			cclink.LoginStores{HasItem: true, Item: newer, File: newer}, named, true},
		{"the stored login is the newer one now", a, newer,
			cclink.LoginStores{File: newer}, named, false},
		{"a later login is stored than the one named", a, loginIssued("RT-later", 3000),
			cclink.LoginStores{File: newer}, named, false},
		{"the named login is one the stored login replaced", Account{UUID: "u-1", SupersededLogins: LoginHash("refresh:RT-newer")},
			stored, cclink.LoginStores{File: newer}, named, false},
		{"the named login was issued before the stored one", a, loginIssued("RT-stored", 3000),
			cclink.LoginStores{File: newer}, named, false},
		{"the login is named as another account's", Account{UUID: "u-2"}, stored,
			cclink.LoginStores{File: newer}, named, false},
		{"nobody named the login", a, stored,
			cclink.LoginStores{File: newer}, namedAs(nil), false},
		{"the newer login left the stores", a, stored,
			cclink.LoginStores{File: stored}, named, false},
		{"the file could not be read", a, stored,
			cclink.LoginStores{File: newer, FileErr: errUnreadableForTest}, named, false},
		{"the stored login could not be read", a, nil,
			cclink.LoginStores{File: newer}, named, true},
	}
	for _, tc := range cases {
		if got := tc.a.StoredLoginSpent(tc.stored, tc.live, tc.named).Held(); got != tc.want {
			t.Errorf("%s: StoredLoginSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE RECORD OUTLIVES THE EVIDENCE. Once a stored login is known to be spent it
// stays held until the account stores another, whatever the stores hold by
// then: Claude Code can rotate the newer login again, out of every store,
// before the oracle names what replaced it.
func TestASpentStoredLoginIsHeldUntilTheAccountStoresAnother(t *testing.T) {
	stored := loginIssued("RT-stored", 1000)
	again := cclink.LoginStores{File: loginIssued("RT-rotated-again", 3000)}
	spentBy := record{spent: map[string][2]string{"u-1": {"refresh:RT-stored", "refresh:RT-newer"}}}
	unknown := record{spent: map[string][2]string{"u-1": {"", "refresh:RT-newer"}}}
	a := Account{UUID: "u-1"}

	cases := []struct {
		name   string
		a      Account
		stored cclink.Blob
		rec    record
		want   bool
	}{
		{"the newer login was rotated again before it was named", a, stored, spentBy, true},
		{"the stored login could not be read", a, nil, spentBy, true},
		{"another login is stored since", a, loginIssued("RT-relogin", 500), spentBy, false},
		{"the newer login itself is stored since", a, loginIssued("RT-newer", 2000), spentBy, false},
		{"a login after the newer one is stored", a, loginIssued("RT-later", 4000), spentBy, false},
		{"another account's record", Account{UUID: "u-2"}, stored, spentBy, false},
		{"the spent login was unknown, and the stored one still cannot be read", a, nil, unknown, true},
		{"the spent login was unknown, and a readable login is stored", a, stored, unknown, false},
	}
	for _, tc := range cases {
		if got := tc.a.StoredLoginSpent(tc.stored, again, tc.rec).Held(); got != tc.want {
			t.Errorf("%s: StoredLoginSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

var errUnreadableForTest = errors.New("the credentials file could not be read")

// A RECORD THAT CANNOT BE READ IS NOT AN EMPTY ONE. The engine state that holds
// which stored logins are spent can fail to read or parse, and what it held is
// then unknown: every stored login is held until it can be read. A state that
// was never written is different -- nothing was ever recorded in it -- and
// holds nothing.
func TestAnUnreadableRecordHoldsEveryStoredLogin(t *testing.T) {
	dir := withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-1", Provider: provider.Claude}, claudeLogin("RT-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-2", Provider: provider.Claude}, cclink.Blob{}); err != nil {
		t.Fatal(err)
	}
	if got := s.SpentLogins(cclink.LoginStores{}, LoadOracleRecord()); len(got) != 0 {
		t.Fatalf("with no engine state at all, %v are held", got)
	}

	if err := os.WriteFile(filepath.Join(dir, SpentRecordFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.SpentLogins(cclink.LoginStores{}, LoadOracleRecord())
	if got["u-1"] != HeldUnreadableRecord {
		t.Error("u-1's stored login is presented although the record of spent logins cannot be read")
	}
	if got["u-2"].Held() {
		t.Error("u-2, which stores no login to present, is held")
	}
	unreadable := record{err: errors.New("parsing spent-logins.json: unexpected end of JSON input")}
	if (Account{UUID: "u-1"}).StoredLoginSpent(claudeLogin("RT-1"), cclink.LoginStores{}, unreadable) != HeldUnreadableRecord {
		t.Error("StoredLoginSpent ignores a record that could not be read")
	}
}

// changingRecord is a record whose first look at an account's spent login
// stores another login for it -- another process adopting the newer login at
// the moment the decision is being taken.
type changingRecord struct {
	record
	change func()
}

func (r *changingRecord) SpentStoredLogin(uuid string) (string, string, bool) {
	if r.change != nil {
		r.change()
		r.change = nil
	}
	return r.record.SpentStoredLogin(uuid)
}

// The copy presented is the copy decided on. A held copy is read again, and
// when the account has stored another login meanwhile, the decision is taken
// on THAT one -- which is then the one returned to be presented. A copy that
// has not changed stays refused.
func TestThePresentedLoginIsTheOneTheHoldWasDecidedOn(t *testing.T) {
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	a := Account{UUID: "u-1", Provider: provider.Claude}
	if err := s.Add(a, claudeLogin("RT-spent")); err != nil {
		t.Fatal(err)
	}
	spent := record{spent: map[string][2]string{"u-1": {"refresh:RT-spent", "refresh:RT-newer"}}}

	got, hold, err := s.PresentableLogin("u-1", cclink.LoginStores{}, spent)
	if err != nil || !hold.Held() || cclink.LoginIdentity(got) != "refresh:RT-spent" {
		t.Fatalf("an unchanged spent copy = %s, %v, %v; want it held", cclink.LoginIdentity(got), hold, err)
	}

	adopting := &changingRecord{record: spent, change: func() {
		cur, _ := s.Get("u-1")
		if err := s.Add(cur, claudeLogin("RT-newer")); err != nil {
			t.Fatal(err)
		}
	}}
	got, hold, err = s.PresentableLogin("u-1", cclink.LoginStores{}, adopting)
	if err != nil || hold.Held() || cclink.LoginIdentity(got) != "refresh:RT-newer" {
		t.Fatalf("after the newer login was stored mid-decision = %s, %v, %v; want the newer copy, not held",
			cclink.LoginIdentity(got), hold, err)
	}
}

// Under an unreadable record, the stored login Claude Code's stores hold with
// nothing later beside it is not held: Claude Code is presenting it already.
// Every other stored login is.
func TestAnUnreadableRecordLetsThroughOnlyTheNewestLoginInAStore(t *testing.T) {
	unreadable := record{err: errors.New("parsing spent-logins.json: unexpected end of JSON input")}
	stored := loginIssued("RT-stored", 2000)
	a := Account{UUID: "u-1"}
	cases := []struct {
		name string
		live cclink.LoginStores
		want Hold
	}{
		{"the file holds it and nothing else", cclink.LoginStores{File: stored}, NotHeld},
		{"both stores hold it", cclink.LoginStores{HasItem: true, Item: stored, File: stored}, NotHeld},
		{"the item holds an older login beside it", cclink.LoginStores{HasItem: true, Item: loginIssued("RT-older", 1000), File: stored}, NotHeld},
		{"a store holds a later login beside it", cclink.LoginStores{HasItem: true, Item: loginIssued("RT-later", 3000), File: stored}, HeldUnreadableRecord},
		{"no store holds it", cclink.LoginStores{File: loginIssued("RT-other", 1000)}, HeldUnreadableRecord},
		{"the stores could not be read", cclink.LoginStores{}, HeldUnreadableRecord},
	}
	for _, tc := range cases {
		if got := a.StoredLoginSpent(stored, tc.live, unreadable); got != tc.want {
			t.Errorf("%s: StoredLoginSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
