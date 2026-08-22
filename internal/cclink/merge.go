package cclink

import (
	"bytes"
	"encoding/json"
)

// Extract returns the account-scoped subset of live — what ccdad snapshots
// when it captures an account.
//
// Machine-scoped keys are excluded, with no exception: live is authoritative
// for machine state, and a snapshot must be safe to carry to another machine,
// because `export` writes snapshots to a file and `import` can restore them
// there. Two of the seven machine keys make that concrete:
//
//   - trustedDeviceToken is device-bound; the spec requires it "stripped from
//     any cross-machine export."
//   - coworkRemoteDevice is the stronger form of the same hazard. It holds a
//     P-256 private key minted on THIS machine plus a server-side device
//     registration named after THIS machine's hostname (Claude Code's own
//     device display name is "Claude Code on <hostname> · <platform>") — it
//     is scoped to the machine, not the account. Claude Code's own full
//     logout path proves it: logout deletes the entire credential store and
//     then writes back exactly this one key, because every other key belongs
//     to the account being logged out of and this one does not. Carrying it
//     in a snapshot would let `import` plant one machine's device identity
//     onto another, colliding with that machine's own registration and
//     burning the account's device-registration cap.
func Extract(live Blob) Blob {
	out := Blob{}
	for _, k := range AccountScopedKeys {
		if v, ok := live[k]; ok {
			out[k] = append(json.RawMessage(nil), v...)
		}
	}
	return out
}

// Merge produces the credentials file to write when activating an account.
//
// live is the file as it exists right now and is authoritative for everything
// machine-scoped, including keys ccdad has never heard of. incoming is the
// target account's stored snapshot, as produced by Extract — the five
// account-scoped keys only.
//
// An account-scoped key missing from incoming is REMOVED rather than inherited:
// carrying account A's trustedDeviceToken into a session running as account B
// presents A's device under B's identity.
//
// coworkRemoteDevice is unioned per organization sub-key rather than replaced
// wholesale (spec 4.2.1), with live winning on any collision — see
// unionObjects. This is defence-in-depth, not a path Extract can exercise:
// Extract deliberately never puts coworkRemoteDevice in a snapshot (see its
// doc comment), so an incoming blob built by this package from a live file can
// never carry the key. The union guards against any OTHER source of incoming
// doing so — a hand-built call, or an export file written by some future
// version of ccdad — so that even then, a wholesale replace cannot destroy
// the live machine's other-organization entries.
//
// Neither argument is mutated.
func Merge(live, incoming Blob) Blob {
	out := Blob{}

	// Start from the live file, minus everything account-scoped.
	for k, v := range live {
		if IsAccountScoped(k) {
			continue
		}
		out[k] = append(json.RawMessage(nil), v...)
	}

	// Layer the incoming account's own keys on top.
	for _, k := range AccountScopedKeys {
		if v, ok := incoming[k]; ok {
			out[k] = append(json.RawMessage(nil), v...)
		}
	}

	// coworkRemoteDevice is keyed by organization uuid and each entry holds a
	// device private key minted on this machine. A wholesale replace destroys
	// the other organization's entry and burns its registration cap, so union
	// the two objects per sub-key with the live copy winning.
	if merged, ok := unionObjects(live["coworkRemoteDevice"], incoming["coworkRemoteDevice"]); ok {
		out["coworkRemoteDevice"] = merged
	}

	return out
}

// unionObjects merges two JSON objects sub-key by sub-key, preferring live.
//
// live is authoritative: if live holds no usable object — the key is absent,
// empty, or not a JSON object — the function reports false, and the caller
// leaves the key exactly as the main merge already left it (copied unmodified
// if present in live, absent otherwise). incoming can never conjure or
// silently replace a key that live does not have in usable form. This matters
// even though Extract never puts coworkRemoteDevice in a snapshot, because
// incoming is not guaranteed to have come from Extract — a hand-built call or
// a future export format could still carry the key, and a wholesale replace
// would destroy the live machine's other-organization entries.
func unionObjects(live, incoming json.RawMessage) (json.RawMessage, bool) {
	liveMap, liveOK := decodeObject(live)
	if !liveOK {
		return nil, false
	}
	incomingMap, _ := decodeObject(incoming)

	merged := make(map[string]json.RawMessage, len(liveMap)+len(incomingMap))
	for k, v := range incomingMap {
		merged[k] = v
	}
	for k, v := range liveMap {
		merged[k] = v // the live machine's own entries win
	}
	encoded, err := marshalNoEscape(merged)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// marshalNoEscape encodes v without HTML-escaping '<', '>' and '&', unlike
// json.Marshal. It re-encodes the small coworkRemoteDevice sub-object.
//
// The escaping matters wherever this package writes JSON, not only here: Go's
// encoder rewrites those three characters even inside a json.RawMessage it is
// merely copying through, so an unescaped encoder is needed at the top level
// too -- see marshalIndentNoEscape.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// marshalIndentNoEscape renders the credentials file the way Claude Code does:
// two-space indent, no HTML escaping.
func marshalIndentNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
