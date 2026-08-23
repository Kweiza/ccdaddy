//go:build windows

package cclink

import (
	"syscall"
	"testing"
)

// TestReplaceRetryableWindowsErrnos pins the exact errno set a replace retries
// on — winerror 5, 32, 33 — and one neighbouring errno that must NOT be
// retried, so a future edit cannot silently widen or narrow the retry set.
func TestReplaceRetryableWindowsErrnos(t *testing.T) {
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
			if got := replaceRetryable(tt.err); got != tt.want {
				t.Errorf("replaceRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
