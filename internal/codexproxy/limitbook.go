// Package codexproxy is ccdad's loopback reverse proxy for codex.
//
// This file is the one piece of it the daemon's Codex lane needs before the
// proxy itself exists: the rate-limit record the two share.
package codexproxy

import (
	"sync"
	"time"
)

// LimitBook is what ccdad knows about which Codex accounts are currently
// throttled, shared by the daemon's Codex lane and the proxy handler.
//
// It is in-memory and deliberately not persisted. A 429 from chatgpt.com is a
// fact about a window that is measured in minutes to hours, and a daemon that
// restarts has to re-learn it from the endpoint anyway -- whereas a persisted
// record would outlive the throttle it describes and hold an account out on
// evidence nobody could re-check.
//
// The zero value is usable, and so is a nil pointer: a nil book answers "no
// limit" to everything, which is what an attended `ccdad switch` -- a process
// with no proxy in it -- has to be able to hand in.
type LimitBook struct {
	mu      sync.Mutex
	entries map[string]limitEntry
}

// limitEntry is one throttle: when it lifts, and whether the endpoint said so.
//
// `known` is a separate fact from the deadline, and the difference is
// user-visible: an account held out for a deadline ccdad chose must not be
// reported back to codex as resetting at that instant, because codex renders
// what it is told as a precise clock.
type limitEntry struct {
	until time.Time
	known bool
}

// MarkLimited records a deadline the endpoint stated.
func (b *LimitBook) MarkLimited(uuid string, until time.Time) { b.MarkLimitedFor(uuid, until, true) }

// MarkLimitedFor records a deadline and whether the endpoint stated it.
//
// The LATER instant always wins. Two writers reach this -- the lane's poll,
// which learns a limit from /wham/usage, and the proxy, which learns one from a
// 429 on a real turn -- and the two see different Retry-After values for the
// same window. Taking the shorter one would send the next request straight back
// into the throttle.
//
// A nil book records nothing, which is the whole of what a process with no
// proxy in it needs from this type.
func (b *LimitBook) MarkLimitedFor(uuid string, until time.Time, resetKnown bool) {
	if b == nil || uuid == "" || until.IsZero() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]limitEntry{}
	}
	if cur, ok := b.entries[uuid]; ok && cur.until.After(until) {
		return
	}
	b.entries[uuid] = limitEntry{until: until, known: resetKnown}
}

// LimitedUntil is when an account's throttle lifts, and whether one is in force
// at all right now.
//
// A record whose instant has passed answers no rather than being swept: there
// is no sweeper, and expiring on read is what keeps a book that nobody prunes
// from holding an account out forever on one 429.
func (b *LimitBook) LimitedUntil(uuid string, now time.Time) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[uuid]
	if !ok || !now.Before(e.until) {
		return time.Time{}, false
	}
	return e.until, true
}

// ResetsAt is LimitedUntil restricted to deadlines the endpoint stated, and it
// is the only one an answer to codex may quote.
func (b *LimitBook) ResetsAt(uuid string, now time.Time) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[uuid]
	if !ok || !e.known || !now.Before(e.until) {
		return time.Time{}, false
	}
	return e.until, true
}
