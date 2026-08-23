// Package winerr classifies the Windows file errors that mean another process
// is momentarily holding the file, rather than that the operation is
// impossible.
//
// It is its own package because two unrelated callers need the same answer:
// cclink retries a rename that hit one of these, and `daemon logs --follow`
// waits for its next poll rather than ending. Keeping one copy of the errno set
// is the point -- two copies drift, and the second one is always the one nobody
// remembers to widen.
package winerr
