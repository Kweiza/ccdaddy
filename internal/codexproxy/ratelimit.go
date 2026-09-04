package codexproxy

import (
	"encoding/json"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
)

// unknownLimitFor is how long an account is held out when a 429 said nothing
// about when its window resets. It is short on purpose: it is a guess, and the
// cost of guessing long is an account nobody tries again for an hour.
const unknownLimitFor = 60 * time.Second

// recordLimit writes a 429 into the book the lane also reads.
//
// The order is the endpoint's own words first -- Retry-After if it sent one,
// then the resets_at in the body -- and only then a minute of ccdad's own
// invention. The invented one is recorded as UNKNOWN, so nothing ever quotes it
// back to codex as a reset time: codex renders that as a precise clock, and a
// precise wrong clock is worse than "try again later".
func (s *Server) recordLimit(uuid string, a *attempt) {
	now := s.now()
	if d, ok := pollpolicy.ParseRetryAfter(a.header.Get("Retry-After"), now); ok && d > 0 {
		s.book.MarkLimitedFor(uuid, now.Add(d), true)
		return
	}
	if at, ok := resetsAtOf(a.body); ok && at.After(now) {
		s.book.MarkLimitedFor(uuid, at, true)
		return
	}
	s.book.MarkLimitedFor(uuid, now.Add(unknownLimitFor), false)
	s.logf("codex account %s is rate limited and the endpoint did not say until when", short(uuid))
}

// resetsAtOf reads the reset instant out of a rate-limit body. The field is
// epoch seconds, and the body is upstream text: anything that does not parse
// reads as absent rather than as zero.
func resetsAtOf(body []byte) (time.Time, bool) {
	var doc struct {
		Error struct {
			ResetsAt *int64 `json:"resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return time.Time{}, false
	}
	if doc.Error.ResetsAt == nil || *doc.Error.ResetsAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(*doc.Error.ResetsAt, 0).UTC(), true
}
