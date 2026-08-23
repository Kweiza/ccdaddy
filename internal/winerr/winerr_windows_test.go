//go:build windows

package winerr

import (
	"syscall"
	"testing"
)

// TestRetryableWindowsErrnos pins the exact errno set a caller retries on --
// winerror 5, 32, 33 -- and one neighbouring errno that must NOT be retried, so
// a future edit cannot silently widen or narrow the set. Widening it is the
// dangerous direction: file-not-found is a real answer, and a follower that
// retried it would wait forever for a log that is never coming back.
func TestRetryableWindowsErrnos(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"access denied (5)", syscall.Errno(5), true},
		{"sharing violation (32)", syscall.Errno(32), true},
		{"lock violation (33)", syscall.Errno(33), true},
		{"file not found (2) is not retryable", syscall.Errno(2), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Retryable(tt.err); got != tt.want {
				t.Errorf("Retryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
