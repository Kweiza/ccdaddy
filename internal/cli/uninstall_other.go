//go:build !windows

package cli

import "os"

// removeSelf deletes the running binary, which every platform but Windows
// allows: the inode stays alive for as long as this process holds it open, and
// the name is gone immediately.
func removeSelf(path string) (scheduled bool, err error) {
	return false, os.Remove(path)
}
