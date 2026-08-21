//go:build windows

package cclink

import (
	"fmt"
	"os"
)

// openCredentialsFile opens path for reading, refusing a symlink via an
// Lstat check first. Go has no portable O_NOFOLLOW on Windows, so a
// check-then-use gap remains between the Lstat and the Open below -- narrower
// than the gap a separate Lstat-then-ReadFile would leave (see the Unix
// counterpart for the race-free version), but not eliminated.
func openCredentialsFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrSymlink)
	}
	return os.Open(path)
}
