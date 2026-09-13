package codexproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/codexlaunch"
)

// authTTL is how long a launch that answered valid is trusted without asking
// the filesystem again.
//
// It is short because the answer it caches can go stale in one direction that
// matters: a launcher that dies frees its lock, and until the entry expires the
// proxy would still serve the codex process that outlived it. Two seconds
// bounds that to two seconds, and it takes the per-turn cost of a burst of
// requests from one stat and one try-lock EACH to one for the burst.
const authTTL = 2 * time.Second

// authHit is one cached launch record.
type authHit struct {
	rec       codexlaunch.Record
	checkedAt time.Time
}

// authorize validates the request's bearer and returns the launch record.
func (s *Server) authorize(r *http.Request) (codexlaunch.Record, bool) {
	bearer := bearerOf(r.Header.Get("Authorization"))
	if bearer == "" {
		return codexlaunch.Record{}, false
	}
	h := hashOf(bearer)
	now := s.now()
	if rec, ok := s.cachedHit(h, now); ok {
		return rec, true
	}

	// The gate is held only across the filesystem check, which is a stat and a
	// try-lock. A caller that finds it full is refused rather than queued: a
	// queue would let an unauthenticated flood delay the one codex that IS
	// authenticated, which is the outcome the cap exists to prevent.
	select {
	case s.unauth <- struct{}{}:
	default:
		s.logf("the codex proxy refused a request: %d launch checks are already in flight", MaxUnauthenticated)
		return codexlaunch.Record{}, false
	}
	rec, res, err := s.cfg.Lookup(s.cfg.Root, bearer)
	<-s.unauth

	if err != nil {
		// Never the bearer, and never the error's own text if it could carry
		// one: the path is enough to debug this from the daemon log.
		s.logf("the codex proxy could not read a launch record: %v", err)
		s.forgetHit(h)
		return codexlaunch.Record{}, false
	}
	if res != codexlaunch.Valid {
		s.forgetHit(h)
		return codexlaunch.Record{}, false
	}
	s.rememberHit(h, rec, now)
	return rec, true
}

func (s *Server) cachedHit(h string, now time.Time) (codexlaunch.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hit, ok := s.auth[h]
	if !ok || now.Sub(hit.checkedAt) >= authTTL {
		return codexlaunch.Record{}, false
	}
	return hit.rec, true
}

func (s *Server) rememberHit(h string, rec codexlaunch.Record, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth[h] = authHit{rec: rec, checkedAt: now}
}

func (s *Server) forgetHit(h string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.auth, h)
}

// bodyTooLarge describes the request's declared size, or the observed lower
// bound when it has no usable Content-Length. Never drain an unbounded stream
// merely to report a total size after it has already exceeded the limit.
type bodyTooLarge struct {
	LimitBytes         int64 `json:"limit_bytes"`
	ActualBytes        int64 `json:"actual_bytes"`
	ActualBytesAtLeast bool  `json:"actual_bytes_at_least"`
}

func (e *bodyTooLarge) Error() string {
	qualifier := ""
	if e.ActualBytesAtLeast {
		qualifier = "at least "
	}
	return fmt.Sprintf("ccdad: Payload Too Large: request body is %s%d bytes; limit is %d bytes (codex.max_body_mib)", qualifier, e.ActualBytes, e.LimitBytes)
}

// readBody buffers only up to the configured cap plus one overflow byte.
// A declared oversized body is refused before allocation; a missing or smaller
// Content-Length cannot bypass the bound on bytes actually read.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, &bodyTooLarge{LimitBytes: limit, ActualBytes: r.ContentLength}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, &bodyTooLarge{LimitBytes: limit, ActualBytes: int64(len(data)), ActualBytesAtLeast: true}
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func writeBodyTooLarge(w http.ResponseWriter, err *bodyTooLarge) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": map[string]any{
		"type": "ccdad_payload_too_large", "message": err.Error(),
		"limit_bytes": err.LimitBytes, "actual_bytes": err.ActualBytes,
		"actual_bytes_at_least": err.ActualBytesAtLeast,
	}})
}

// bearerOf reads the token out of an Authorization header, and only ever a
// bearer one.
func bearerOf(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// hashOf names a launch record from its secret. It is this package's own rather
// than codexlaunch's because the cache is keyed on it, and a secret must not sit
// in a long-lived map in the clear.
func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
