//go:build !windows

package cli

import (
	"fmt"
	"os"
)

// replaceBinary puts staged at target. It receives a closed, already-synced,
// already-chmodded file, because the download step owns those — it is the code
// holding the open handle — so this does renames and nothing else.
//
// One os.Rename, and it is a rename rather than a copy because the staged file
// is a SIBLING of the target by construction: the staging directory is
// filepath.Dir(target). install.sh records why that matters — /tmp is a
// different filesystem on most distributions, a cross-device mv degrades to a
// copy, and a copy over a running binary is ETXTBSY.
//
// Renaming over a running binary is fine here: the directory entry is replaced
// and the running process keeps executing out of the old inode.
func replaceBinary(staged, target string) error {
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("moving %s into place at %s: %w", staged, target, err)
	}
	return nil
}
