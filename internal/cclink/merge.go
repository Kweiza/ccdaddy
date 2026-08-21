package cclink

import (
	"bytes"
	"encoding/json"
)

// Extract returns what ccdad snapshots when it captures an account: the
// account-scoped subset of live, plus — when orgUUID is non-empty — that
// organization's own entry under coworkRemoteDevice.
//
// coworkRemoteDevice is otherwise machine-scoped, but spec 4.2.1 requires an
// account's own device key to survive a switch away and back: without
// carrying it here, Merge's per-organization union would have nothing of the
// incoming account's to union in, and a wholesale live replace on the next
// machine would destroy it outright. This is the one documented exception;
// every other machine-scoped key is deliberately excluded, because live is
// authoritative for machine state and nothing else from a snapshot should
// ever be read back into it.
func Extract(live Blob, orgUUID string) Blob {
	out := Blob{}
	for _, k := range AccountScopedKeys {
		if v, ok := live[k]; ok {
			out[k] = append(json.RawMessage(nil), v...)
		}
	}

	if orgUUID == "" {
		return out
	}
	obj, ok := decodeObject(live["coworkRemoteDevice"])
	if !ok {
		return out
	}
	entry, ok := obj[orgUUID]
	if !ok {
		return out
	}
	encoded, err := marshalNoEscape(map[string]json.RawMessage{orgUUID: entry})
	if err != nil {
		return out
	}
	out["coworkRemoteDevice"] = encoded
	return out
}

// Merge produces the credentials file to write when activating an account.
//
// live is the file as it exists right now and is authoritative for everything
// machine-scoped, including keys ccdad has never heard of. incoming is the
// target account's stored snapshot, as produced by Extract — the account-
// scoped keys plus, if present, the account's own coworkRemoteDevice entry.
//
// An account-scoped key missing from incoming is REMOVED rather than inherited:
// carrying account A's trustedDeviceToken into a session running as account B
// presents A's device under B's identity.
//
// coworkRemoteDevice is the one key read from incoming despite being
// machine-scoped: its entries are keyed per organization (spec 4.2.1), so the
// live and incoming objects are unioned sub-key by sub-key with live winning
// on any collision. Absence or malformation in live always wins outright —
// see unionObjects.
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
// silently replace a key that live does not have in usable form; that would
// let a snapshot captured on a different machine plant a foreign device
// identity into the live file.
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
// json.Marshal. It is used only to re-encode the small coworkRemoteDevice
// sub-object: every other value in this package passes through untouched as
// RawMessage, so this is the sole place re-encoding can alter bytes at all.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
