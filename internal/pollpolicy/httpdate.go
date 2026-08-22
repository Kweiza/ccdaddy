package pollpolicy

import (
	"net/http"
	"time"
)

// parseHTTPDate is net/http's own date parser, wrapped so the policy's single
// standard-library dependency is visible in one place. http.ParseTime accepts
// all three formats RFC 9110 permits — IMF-fixdate, RFC 850 and ANSI C asctime
// — and getting that set wrong means silently discarding a legal header.
func parseHTTPDate(v string) (time.Time, error) { return http.ParseTime(v) }
