package codexproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

const (
	// rateLimitEventType is the event codex's own client reads its quota from,
	// measured against codex 0.151.0.
	rateLimitEventType = "codex.rate_limits"

	primaryHeaderPrefix   = "x-codex-primary"
	secondaryHeaderPrefix = "x-codex-secondary"

	// maxSSELine caps one buffered line. A stream is unbounded and the daemon
	// is long-lived, so a line that never ends must cost a fixed amount of
	// memory and then nothing.
	maxSSELine = 1 << 16

	dataPrefix = "data:"
)

// harvest hands one reading to the daemon.
//
// These readings are worth more than a poll's: they were taken by a request the
// user actually made, so they cost nothing and they are exactly current. The
// lane polls on a fifteen-minute floor; a busy hour can spend an account
// between two polls, and this is what closes that window.
func (s *Server) harvest(uuid string, snap *usage.Snapshot) {
	if s.cfg.Harvest == nil || uuid == "" || snap == nil {
		return
	}
	s.cfg.Harvest(uuid, snap)
}

// harvestHeaders reads the rate-limit header family off any answer, whatever
// its status. The family on a 429 is the most informative one there is.
func harvestHeaders(h http.Header) (*usage.Snapshot, bool) {
	primary, okPrimary := windowFromHeaders(h, primaryHeaderPrefix)
	secondary, okSecondary := windowFromHeaders(h, secondaryHeaderPrefix)
	if !okPrimary && !okSecondary {
		return nil, false
	}
	snap := &usage.Snapshot{}
	if okPrimary {
		snap.CodexPrimary = primary
	}
	if okSecondary {
		snap.CodexSecondary = secondary
	}
	return snap, true
}

func windowFromHeaders(h http.Header, prefix string) (usage.Window, bool) {
	pct, hasPct := headerFloat(h, prefix+"-used-percent")
	minutes, hasMinutes := headerInt(h, prefix+"-window-minutes")
	resetAt, hasReset := headerInt(h, prefix+"-reset-at")
	if !hasPct && !hasMinutes && !hasReset {
		return usage.Window{}, false
	}
	return buildWindow(pct, hasPct, minutes, hasMinutes, resetAt, hasReset), true
}

func headerFloat(h http.Header, name string) (float64, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func headerInt(h http.Header, name string) (int64, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// buildWindow turns the three numbers into a window, leaving out whatever was
// not stated. The window LENGTH travels with the reading because codex's own
// windows are data-driven -- a plan can meter on five hours, a day, a week, a
// month or a year -- so a name derived from a plan would be a guess.
func buildWindow(pct float64, hasPct bool, minutes int64, hasMinutes bool, resetAt int64, hasReset bool) usage.Window {
	var pctPtr *float64
	if hasPct {
		pctPtr = &pct
	}
	var resetPtr *time.Time
	if hasReset && resetAt > 0 {
		at := time.Unix(resetAt, 0).UTC()
		resetPtr = &at
	}
	var length time.Duration
	if hasMinutes && minutes > 0 {
		length = time.Duration(minutes) * time.Minute
	}
	return usage.NewWindowWithLength(pctPtr, resetPtr, length)
}

// rateLimitEvent is the in-stream form of the same reading.
type rateLimitEvent struct {
	Type       string `json:"type"`
	RateLimits *struct {
		Primary   *eventWindow `json:"primary"`
		Secondary *eventWindow `json:"secondary"`
	} `json:"rate_limits"`
}

type eventWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowMinutes *int64   `json:"window_minutes"`
	ResetAt       *int64   `json:"reset_at"`
}

// harvestEvent reads one SSE payload. Anything that is not a rate-limit event
// with at least one window in it is not a reading.
func harvestEvent(payload []byte) (*usage.Snapshot, bool) {
	var ev rateLimitEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, false
	}
	if ev.Type != rateLimitEventType || ev.RateLimits == nil {
		return nil, false
	}
	snap := &usage.Snapshot{}
	found := false
	if w, ok := eventWindowOf(ev.RateLimits.Primary); ok {
		snap.CodexPrimary, found = w, true
	}
	if w, ok := eventWindowOf(ev.RateLimits.Secondary); ok {
		snap.CodexSecondary, found = w, true
	}
	if !found {
		return nil, false
	}
	return snap, true
}

func eventWindowOf(in *eventWindow) (usage.Window, bool) {
	if in == nil {
		return usage.Window{}, false
	}
	var (
		pct        float64
		minutes    int64
		resetAt    int64
		hasPct     = in.UsedPercent != nil
		hasMinutes = in.WindowMinutes != nil
		hasReset   = in.ResetAt != nil
	)
	if hasPct {
		pct = *in.UsedPercent
	}
	if hasMinutes {
		minutes = *in.WindowMinutes
	}
	if hasReset {
		resetAt = *in.ResetAt
	}
	if !hasPct && !hasMinutes && !hasReset {
		return usage.Window{}, false
	}
	return buildWindow(pct, hasPct, minutes, hasMinutes, resetAt, hasReset), true
}

// sseScanner finds rate-limit events in a stream the proxy is forwarding,
// without ever holding the stream.
//
// The bytes arrive in whatever chunks the network produced, so a line can be
// split across two of them and a chunk can hold several lines. What this keeps
// is one partial line, capped.
type sseScanner struct {
	buf  []byte
	over bool
	on   func(snap *usage.Snapshot)
}

// streamHarvester is the scanner one streamed answer is read through.
func (s *Server) streamHarvester(uuid string) *sseScanner {
	return &sseScanner{on: func(snap *usage.Snapshot) { s.harvest(uuid, snap) }}
}

func (sc *sseScanner) write(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			sc.append(p)
			return
		}
		sc.append(p[:i])
		sc.line()
		p = p[i+1:]
	}
}

func (sc *sseScanner) append(p []byte) {
	if sc.over {
		return
	}
	if len(sc.buf)+len(p) > maxSSELine {
		// The line is longer than any event this reads. Drop it and wait for
		// the next newline rather than growing.
		sc.over = true
		sc.buf = sc.buf[:0]
		return
	}
	sc.buf = append(sc.buf, p...)
}

func (sc *sseScanner) line() {
	line := append([]byte(nil), bytes.TrimSpace(sc.buf)...)
	over := sc.over
	sc.buf, sc.over = sc.buf[:0], false
	if over || sc.on == nil {
		return
	}
	if !bytes.HasPrefix(line, []byte(dataPrefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(dataPrefix):])
	if snap, ok := harvestEvent(payload); ok {
		sc.on(snap)
	}
}
