//go:build !windows

package atomicfile

import "os"

// readSharedDelete is plain os.ReadFile off Windows: a Unix rename cannot be
// blocked by an open handle on the destination.
func readSharedDelete(path string) ([]byte, error) { return os.ReadFile(path) }
