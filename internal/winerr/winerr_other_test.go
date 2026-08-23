//go:build !windows

package winerr

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// Off Windows the answer is no to everything, and that is load-bearing rather
// than a placeholder. Callers wrap this around a BOUNDED retry loop, so a yes
// here would send every ordinary failure -- a missing file above all -- through
// the whole loop and its backoff before reporting what the first attempt
// already knew.
//
// The three Windows errnos are in the table for the same reason. As numbers off
// Windows they mean entirely unrelated things, so recognising them by value
// rather than by platform is a mistake that would look correct in a diff.
func TestNothingIsRetryableOffWindows(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"not exist", os.ErrNotExist},
		{"a rename that found nothing", &fs.PathError{Op: "rename", Path: "x", Err: syscall.ENOENT}},
		{"permission denied", syscall.EACCES},
		{"errno 5, which is winerror ACCESS_DENIED elsewhere", syscall.Errno(5)},
		{"errno 32, which is winerror SHARING_VIOLATION elsewhere", syscall.Errno(32)},
		{"errno 33, which is winerror LOCK_VIOLATION elsewhere", syscall.Errno(33)},
		{"anything at all", errors.New("anything at all")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if Retryable(tt.err) {
				t.Errorf("Retryable(%v) = true, want false off Windows", tt.err)
			}
		})
	}
}
