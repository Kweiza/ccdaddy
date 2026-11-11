package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

// loginIssued is a login whose expiry -- which orders logins by when they were
// issued -- is expiresAt, in Claude Code's milliseconds.
func loginIssued(refresh string, expiresAt int64) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(fmt.Sprintf(
		`{"accessToken":"AT","refreshToken":%q,"expiresAt":%d}`, refresh, expiresAt))}
}

// namedAs is the identity oracle's record of which logins it named whose.
func namedAs(byIdentity map[string]string) NamedLogins {
	byHash := map[string]string{}
	for id, uuid := range byIdentity {
		byHash[LoginHash(id)] = uuid
	}
	return func(h string) (string, bool) { uuid, ok := byHash[h]; return uuid, ok }
}

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
		named  NamedLogins
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
		if got := tc.a.StoredLoginSpent(tc.stored, tc.live, tc.named); got != tc.want {
			t.Errorf("%s: StoredLoginSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

var errUnreadableForTest = errors.New("the credentials file could not be read")
