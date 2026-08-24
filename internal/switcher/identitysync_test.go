package switcher

import (
	"encoding/json"
	"os"
	"testing"
)

// A switch has to change the credentials file's login AND ~/.claude.json's
// cached display of who that login belongs to. Claude Code's own
// token-refresh handler never rewrites accountUuid/emailAddress once one is
// cached (see cclink's oauthaccount.go), so a switch that skips this leaves
// the display naming the account that was live before, forever.
func TestExecuteUpdatesOAuthAccountOnASwitch(t *testing.T) {
	isolate(t)
	one := seed(t, "u-1", "one@example.com")
	two := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"one@example.com",`+
		`"billingType":"stripe_subscription","accountCreatedAt":"2025-01-01T00:00:00Z",`+
		`"subscriptionCreatedAt":"2025-01-01T00:00:00Z","ccOnboardingFlags":{}}}`)

	res, err := Execute(openStore(t), Request{Target: two, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if res.ProfileSyncErr != nil {
		t.Fatalf("ProfileSyncErr = %v, want nil", res.ProfileSyncErr)
	}

	got := readOAuthAccount(t)
	if got["accountUuid"] != "u-2" {
		t.Fatalf("oauthAccount.accountUuid = %v, want u-2", got["accountUuid"])
	}
	if got["emailAddress"] != "two@example.com" {
		t.Fatalf("oauthAccount.emailAddress = %v, want two@example.com", got["emailAddress"])
	}
	// The completeness fields are deliberately absent: Claude Code's own
	// refresh handler skips re-fetching a profile it thinks is already
	// complete, and account two's profile was never actually fetched.
	if _, ok := got["billingType"]; ok {
		t.Fatalf("oauthAccount carried account one's billingType forward: %v", got)
	}

	freshOne, ok := openStore(t).Get(one.UUID)
	if !ok {
		t.Fatal("account one vanished from the store")
	}
	if freshOne.OAuthAccountSnapshot == "" {
		t.Fatal("account one's displaced oauthAccount was not backed up")
	}
	var backedUp map[string]any
	if err := json.Unmarshal([]byte(freshOne.OAuthAccountSnapshot), &backedUp); err != nil {
		t.Fatal(err)
	}
	if backedUp["accountUuid"] != "u-1" {
		t.Fatalf("backed-up snapshot names %v, want u-1", backedUp["accountUuid"])
	}
}

// A switch BACK to an account whose oauthAccount was backed up on the way out
// restores it verbatim -- cosmetic fields included -- rather than reducing it
// to the minimal identity object a never-before-seen account gets.
func TestExecuteRestoresABackedUpOAuthAccountOnSwitchBack(t *testing.T) {
	isolate(t)
	one := seed(t, "u-1", "one@example.com")
	two := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"one@example.com",`+
		`"displayName":"One","billingType":"stripe_subscription"}}`)

	s := openStore(t)
	if _, err := Execute(s, Request{Target: two, LiveUUID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	// Simulate Claude Code having since written its OWN oauthAccount for
	// account two -- a real, different object, not whatever ccdad's own
	// bootstrap reset left. Only an actual restore below can turn this back
	// into account one's original data; leaving the file untouched cannot.
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-2","emailAddress":"two@example.com",`+
		`"displayName":"Two"}}`)

	s = openStore(t)
	oneAgain, ok := s.Get(one.UUID)
	if !ok {
		t.Fatal("account one vanished from the store")
	}
	res, err := Execute(s, Request{Target: oneAgain, LiveUUID: "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}

	got := readOAuthAccount(t)
	if got["accountUuid"] != "u-1" {
		t.Fatalf("oauthAccount.accountUuid = %v, want u-1", got["accountUuid"])
	}
	if got["displayName"] != "One" {
		t.Fatalf("the restored snapshot lost displayName: %v", got)
	}
	if got["billingType"] != "stripe_subscription" {
		t.Fatalf("the restored snapshot lost billingType: %v", got)
	}
}

// On a machine where the display has already been stale across SEVERAL real
// switches -- exactly the state this fix rolls out onto -- the object
// ~/.claude.json holds when a switch runs can belong to neither the target
// nor the account the credentials file just moved off of, but to whoever was
// live further back, before any of ccdad's own switches started correcting
// it. Filing that object under the departing account's backup slot would
// plant a WRONG accountUuid there for some future restore to reinstall, so it
// must be dropped instead of backed up.
func TestExecuteDoesNotBackUpAStaleOAuthAccountUnderTheWrongAccount(t *testing.T) {
	isolate(t)
	one := seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")
	three := seed(t, "u-3", "three@example.com")
	liveAs(t, "u-2")
	// Names neither u-2 (who the credentials file just moved off of) nor u-3
	// (the target): a display left over from whoever was live before u-2,
	// back when nothing corrected it yet.
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"one@example.com"}}`)

	res, err := Execute(openStore(t), Request{Target: three, LiveUUID: "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}

	s := openStore(t)
	freshTwo, ok := s.Get("u-2")
	if !ok {
		t.Fatal("account two vanished from the store")
	}
	if freshTwo.OAuthAccountSnapshot != "" {
		t.Fatalf("account one's stale display was filed under account two's backup slot: %s",
			freshTwo.OAuthAccountSnapshot)
	}
	freshOne, ok := s.Get(one.UUID)
	if !ok {
		t.Fatal("account one vanished from the store")
	}
	if freshOne.OAuthAccountSnapshot != "" {
		t.Fatalf("a display that was never proven live got backed up as account one's own snapshot: %s",
			freshOne.OAuthAccountSnapshot)
	}
}

// The AlreadyOn path is the only one that ever runs for an account that is
// already live and staying live, and it is the only path that can ever fix
// ~/.claude.json's display once it has drifted out of sync with a credentials
// file that a PAST switch already corrected.
func TestExecuteSelfHealsOAuthAccountWhenAlreadyOn(t *testing.T) {
	isolate(t)
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-2")
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"one@example.com"}}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyOn {
		t.Fatalf("outcome = %v, want %v", res.Outcome, AlreadyOn)
	}
	if res.ProfileSyncErr != nil {
		t.Fatalf("ProfileSyncErr = %v, want nil", res.ProfileSyncErr)
	}

	got := readOAuthAccount(t)
	if got["accountUuid"] != "u-2" {
		t.Fatalf("oauthAccount.accountUuid = %v, want u-2 (self-heal did not run)", got["accountUuid"])
	}
}

// A no-op AlreadyOn against an oauthAccount that already names the target must
// not touch ~/.claude.json at all: doing so would discard any cosmetic
// enrichment Claude Code's own refresh has added since ccdad last wrote it,
// for no gain, and would bump the mtime Claude Code watches for no reason.
func TestExecuteLeavesOAuthAccountAloneWhenAlreadyCorrect(t *testing.T) {
	isolate(t)
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-2")
	writeGlobalConfig(t, `{"oauthAccount":{"accountUuid":"u-2","emailAddress":"two@example.com",`+
		`"displayName":"Enriched By Claude Code"}}`)
	before, err := os.ReadFile(globalConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyOn {
		t.Fatalf("outcome = %v, want %v", res.Outcome, AlreadyOn)
	}

	after, err := os.ReadFile(globalConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("an already-correct oauthAccount was rewritten:\nbefore: %s\nafter:  %s", before, after)
	}
}
