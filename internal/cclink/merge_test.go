package cclink

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func blob(t *testing.T, pairs map[string]string) Blob {
	t.Helper()
	b := Blob{}
	for k, v := range pairs {
		b[k] = json.RawMessage(v)
	}
	return b
}

func keysOf(b Blob) []string {
	out := make([]string, 0, len(b))
	for k := range b {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Claude Code's own re-login prune deletes exactly these five keys and leaves
// everything else standing. That set is our swap set.
func TestAccountScopedSetMatchesClaudeCodePrune(t *testing.T) {
	want := []string{
		"claudeAiOauth", "designOauth", "enterpriseGateway",
		"organizationUuid", "trustedDeviceToken",
	}
	got := append([]string(nil), AccountScopedKeys...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountScopedKeys = %v, want %v", got, want)
	}
}

// The drift-detection list deserves the same pin as AccountScopedKeys: it is
// the sole input to the unknown-key probe spec 4.3 requires on every switch,
// so a typo here makes ccdad warn about legitimate keys forever.
func TestKnownMachineKeysMatchesSpec(t *testing.T) {
	want := []string{
		"coworkRemoteDevice", "gatewayTrust", "mcpOAuth", "mcpOAuthClientConfig",
		"mcpXaaIdp", "mcpXaaIdpConfig", "pluginSecrets",
	}
	got := append([]string(nil), KnownMachineKeys...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KnownMachineKeys = %v, want %v", got, want)
	}
}

// Spec 4.1's own headline claim: twelve top-level keys, five account-scoped
// and seven machine-scoped.
func TestKeyListsCoverAllTwelveSpecKeys(t *testing.T) {
	if got := len(AccountScopedKeys) + len(KnownMachineKeys); got != 12 {
		t.Fatalf("spec 4.1 names 12 top-level keys; have %d", got)
	}
}

func TestIsAccountScopedCoversAllKnownKeys(t *testing.T) {
	cases := map[string]bool{
		"claudeAiOauth":        true,
		"organizationUuid":     true,
		"trustedDeviceToken":   true,
		"enterpriseGateway":    true,
		"designOauth":          true,
		"mcpOAuth":             false,
		"mcpOAuthClientConfig": false,
		"mcpXaaIdp":            false,
		"mcpXaaIdpConfig":      false,
		"pluginSecrets":        false,
		"gatewayTrust":         false,
		"coworkRemoteDevice":   false,
		"somethingUnknown":     false,
	}
	for key, want := range cases {
		if got := IsAccountScoped(key); got != want {
			t.Errorf("IsAccountScoped(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestExtractTakesOnlyAccountScoped(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"a"}`,
		"trustedDeviceToken": `"tok"`,
		"mcpOAuth":           `{"sentry|1":{"accessToken":"m"}}`,
		"gatewayTrust":       `{"gw.example":"fp"}`,
	})

	got := Extract(live)
	if want := []string{"claudeAiOauth", "trustedDeviceToken"}; !reflect.DeepEqual(keysOf(got), want) {
		t.Fatalf("Extract keys = %v, want %v", keysOf(got), want)
	}
}

func TestExtractOutputIsIndependentOfInput(t *testing.T) {
	live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`})

	got := Extract(live)
	got["claudeAiOauth"][0] = 'X'

	if string(live["claudeAiOauth"]) != `{"accessToken":"old"}` {
		t.Fatalf("mutating Extract's output corrupted live: %s", live["claudeAiOauth"])
	}
}

// The central correction of this round: coworkRemoteDevice must NEVER enter a
// snapshot. A snapshot is not just an in-process value — `export` writes it
// to a file and `import` can restore it on a DIFFERENT machine, where this
// machine's device private key and hostname-scoped registration would be
// invalid and would collide with that machine's own.
func TestExtractNeverCarriesCoworkRemoteDevice(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"a"}`,
		"coworkRemoteDevice": `{"org-a":{"privateKeyPkcs8B64":"KEY-A"}}`,
	})

	got := Extract(live)
	if _, ok := got["coworkRemoteDevice"]; ok {
		t.Fatal("coworkRemoteDevice must never enter a snapshot; it can be exported to another machine")
	}
}

// The headline behaviour: machine-scoped keys present in the LIVE file survive
// a switch untouched, and the incoming account's account-scoped keys replace
// the live ones.
func TestMergePreservesMachineScopedKeys(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":        `{"accessToken":"old"}`,
		"mcpOAuth":             `{"sentry|1":{"accessToken":"live"}}`,
		"mcpOAuthClientConfig": `{"sentry|1":{"clientSecret":"s"}}`,
		"pluginSecrets":        `{"p":"x"}`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth": `{"accessToken":"new"}`,
	})

	got := Merge(live, incoming)

	if string(got["claudeAiOauth"]) != `{"accessToken":"new"}` {
		t.Fatalf("claudeAiOauth = %s, want the incoming value", got["claudeAiOauth"])
	}
	for _, k := range []string{"mcpOAuth", "mcpOAuthClientConfig", "pluginSecrets"} {
		if string(got[k]) != string(live[k]) {
			t.Fatalf("%s = %s, want the live value %s", k, got[k], live[k])
		}
	}
}

// An account-scoped key the incoming account does not have must be REMOVED,
// not inherited from the previous account. Otherwise account A's device token
// is presented under account B.
func TestMergeDropsAccountScopedKeysAbsentFromIncoming(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"old"}`,
		"trustedDeviceToken": `"device-of-account-a"`,
		"enterpriseGateway":  `{"jwt":"j"}`,
		"mcpOAuth":           `{"s|1":{}}`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth": `{"accessToken":"new"}`,
	})

	got := Merge(live, incoming)

	if _, ok := got["trustedDeviceToken"]; ok {
		t.Fatal("trustedDeviceToken survived a switch to an account that has none")
	}
	if _, ok := got["enterpriseGateway"]; ok {
		t.Fatal("enterpriseGateway survived a switch to an account that has none")
	}
	if _, ok := got["mcpOAuth"]; !ok {
		t.Fatal("mcpOAuth was dropped; it is machine-scoped")
	}
}

// Unknown top-level keys are machine-scoped by default. This is the whole point
// of expressing the rule as a deny-list: a key Anthropic adds later is
// preserved automatically rather than destroyed on every switch.
func TestMergePreservesUnknownKeys(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":     `{"accessToken":"old"}`,
		"somethingBrandNew": `{"a":1}`,
	})
	incoming := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"new"}`})

	got := Merge(live, incoming)
	if string(got["somethingBrandNew"]) != `{"a":1}` {
		t.Fatalf("unknown key was not preserved: %v", keysOf(got))
	}
}

func TestUnknownKeysReportsOnlyUnrecognized(t *testing.T) {
	pairs := map[string]string{
		"somethingBrandNew": `{}`,
		"anotherNewOne":     `{}`,
	}
	for _, k := range AccountScopedKeys {
		pairs[k] = `{}`
	}
	for _, k := range KnownMachineKeys {
		pairs[k] = `{}`
	}
	b := blob(t, pairs)

	got := UnknownKeys(b)
	sort.Strings(got)
	if want := []string{"anotherNewOne", "somethingBrandNew"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownKeys = %v, want %v", got, want)
	}
}

// coworkRemoteDevice is keyed by organization uuid and holds a device private
// key minted on THIS machine. Merge unions rather than replaces (spec 4.2.1)
// as defence-in-depth, not because the shipped pipeline needs it: Extract
// never puts this key in a snapshot (see its doc comment), so incoming here
// is hand-built, not something Capture can produce. It guards a source of
// incoming that does not occur in the pipeline today — a hand-built call, or
// an export written by some future version of ccdad — so that even then, a
// wholesale replace cannot destroy the live machine's other-organization
// entries. This is a unit-level guard on unionObjects, not an end-to-end one.
func TestMergeUnionsCoworkRemoteDevicePerOrg(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"old"}`,
		"coworkRemoteDevice": `{"org-a":{"privateKeyPkcs8B64":"KEY-A"}}`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"new"}`,
		"coworkRemoteDevice": `{"org-b":{"privateKeyPkcs8B64":"KEY-B"}}`,
	})

	got := Merge(live, incoming)

	var merged map[string]map[string]string
	if err := json.Unmarshal(got["coworkRemoteDevice"], &merged); err != nil {
		t.Fatal(err)
	}
	if merged["org-a"]["privateKeyPkcs8B64"] != "KEY-A" {
		t.Fatalf("org-a device key lost: %v", merged)
	}
	if merged["org-b"]["privateKeyPkcs8B64"] != "KEY-B" {
		t.Fatalf("org-b device key missing: %v", merged)
	}
}

// The whole reason the union loops are ordered incoming-then-live: on a
// colliding sub-key, the live machine's own entry must win.
func TestMergeCoworkRemoteDeviceLiveWinsOnCollision(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"old"}`,
		"coworkRemoteDevice": `{"org-a":{"privateKeyPkcs8B64":"LIVE-A"},"org-shared":{"privateKeyPkcs8B64":"LIVE-SHARED"}}`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"new"}`,
		"coworkRemoteDevice": `{"org-b":{"privateKeyPkcs8B64":"KEY-B"},"org-shared":{"privateKeyPkcs8B64":"SNAPSHOT-SHARED"}}`,
	})

	got := Merge(live, incoming)

	var merged map[string]map[string]string
	if err := json.Unmarshal(got["coworkRemoteDevice"], &merged); err != nil {
		t.Fatal(err)
	}
	if merged["org-shared"]["privateKeyPkcs8B64"] != "LIVE-SHARED" {
		t.Fatalf("org-shared = %v, want the live entry to win", merged["org-shared"])
	}
	if merged["org-a"]["privateKeyPkcs8B64"] != "LIVE-A" {
		t.Fatalf("org-a device key lost: %v", merged)
	}
	if merged["org-b"]["privateKeyPkcs8B64"] != "KEY-B" {
		t.Fatalf("org-b device key missing: %v", merged)
	}
}

// Absence in live must win outright: a snapshot taken on a different machine
// must never plant a foreign device identity into a live file that has none.
func TestMergeDropsCoworkRemoteDeviceWhenLiveHasNone(t *testing.T) {
	live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`})
	incoming := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"new"}`,
		"coworkRemoteDevice": `{"org-a":{"privateKeyPkcs8B64":"FOREIGN-KEY"}}`,
	})

	got := Merge(live, incoming)
	if _, ok := got["coworkRemoteDevice"]; ok {
		t.Fatal("coworkRemoteDevice resurrected from incoming though live had none")
	}
}

// A live value that fails to decode as a JSON object must pass through
// unmodified rather than being silently replaced by incoming's object.
func TestMergePreservesMalformedCoworkRemoteDeviceInLive(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"old"}`,
		"coworkRemoteDevice": `"not-an-object"`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"new"}`,
		"coworkRemoteDevice": `{"org-a":{"privateKeyPkcs8B64":"FOREIGN-KEY"}}`,
	})

	got := Merge(live, incoming)
	if string(got["coworkRemoteDevice"]) != `"not-an-object"` {
		t.Fatalf("coworkRemoteDevice = %s, want the live value preserved unmodified", got["coworkRemoteDevice"])
	}
}

// unionObjects re-encodes through encoding/json, which HTML-escapes by
// default. That must not happen here: gatewayTrust fingerprints and
// coworkRemoteDevice payloads can contain '&' and must survive unchanged.
func TestMergeCoworkRemoteDeviceDoesNotHTMLEscape(t *testing.T) {
	live := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"old"}`,
		"coworkRemoteDevice": `{"org-a":{"url":"https://x?a=1&b=2"}}`,
	})
	incoming := blob(t, map[string]string{
		"claudeAiOauth":      `{"accessToken":"new"}`,
		"coworkRemoteDevice": `{"org-b":{"url":"y"}}`,
	})

	got := Merge(live, incoming)
	if !bytes.Contains(got["coworkRemoteDevice"], []byte("a=1&b=2")) {
		t.Fatalf("coworkRemoteDevice was HTML-escaped: %s", got["coworkRemoteDevice"])
	}
}

// gatewayTrust is a pinned TLS fingerprint. Absence in the live file must be
// propagated: an add-only carry could resurrect a fingerprint the operator
// deliberately removed. This behaviour is emergent from the deny-list itself
// (already exercised by TestMergePreservesMachineScopedKeys and
// TestMergeDropsAccountScopedKeysAbsentFromIncoming); this is a named
// regression guard for the specific key spec 4.2.2 calls out, not a test of a
// distinct code path.
func TestMergePropagatesGatewayTrustAbsence(t *testing.T) {
	live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`})
	incoming := blob(t, map[string]string{
		"claudeAiOauth": `{"accessToken":"new"}`,
		"gatewayTrust":  `{"gw.example":"REJECTED-FINGERPRINT"}`,
	})

	got := Merge(live, incoming)
	if _, ok := got["gatewayTrust"]; ok {
		t.Fatal("gatewayTrust was resurrected from the incoming snapshot; absence must win")
	}
}

// The general invariant behind gatewayTrust's rule, checked for every
// machine-scoped key, not just one: nothing machine-scoped that live lacks
// can ever be conjured from incoming. This would also catch coworkRemoteDevice
// being wrongly resurrected from incoming.
func TestMergeNeverConjuresMachineScopedKeysAbsentFromLive(t *testing.T) {
	for _, k := range KnownMachineKeys {
		t.Run(k, func(t *testing.T) {
			live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`})
			incoming := blob(t, map[string]string{
				"claudeAiOauth": `{"accessToken":"new"}`,
				k:               `{"anything":"here"}`,
			})
			got := Merge(live, incoming)
			if _, ok := got[k]; ok {
				t.Fatalf("%s: machine-scoped key conjured from incoming though live had none", k)
			}
		})
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`, "mcpOAuth": `{}`})
	incoming := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"new"}`})
	liveBefore, incomingBefore := keysOf(live), keysOf(incoming)

	_ = Merge(live, incoming)

	if !reflect.DeepEqual(keysOf(live), liveBefore) {
		t.Fatal("Merge mutated live")
	}
	if !reflect.DeepEqual(keysOf(incoming), incomingBefore) {
		t.Fatal("Merge mutated incoming")
	}
	if string(live["claudeAiOauth"]) != `{"accessToken":"old"}` {
		t.Fatal("Merge mutated a live value")
	}
}

func TestMergeOutputIsIndependentOfInputs(t *testing.T) {
	live := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"old"}`, "mcpOAuth": `{"a":1}`})
	incoming := blob(t, map[string]string{"claudeAiOauth": `{"accessToken":"new"}`})

	got := Merge(live, incoming)
	got["mcpOAuth"][0] = 'X'

	if string(live["mcpOAuth"]) != `{"a":1}` {
		t.Fatalf("mutating Merge's output corrupted live: %s", live["mcpOAuth"])
	}
}
