//go:build !windows

package cclink

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openCredentialsFile opens path for reading without following a trailing
// symlink. Doing the symlink check via a separate Lstat before a plain
// ReadFile leaves a check-then-use gap: the path can be replaced with a
// symlink between the two calls, and whatever Load reads gets merged and
// WRITTEN BACK by Activate, so that gap would let a planted symlink copy an
// arbitrary user-readable file's contents into the credential store.
// O_NOFOLLOW closes it by refusing atomically, in the same open call that
// does the read.
//
// ELOOP -- the symlink case -- is mapped to ErrSymlink. Every other error,
// including ENOENT, passes through unchanged so callers can keep using the
// standard errors.Is(err, os.ErrNotExist) check for "no credentials yet".
func openCredentialsFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s: %w", path, ErrSymlink)
		}
		return nil, err
	}
	return f, nil
}
