package cclink

import (
	"os"

	"github.com/Kweiza/ccdaddy/internal/atomicfile"
)

// WriteFileAtomic writes data to path through a sibling temp file and a rename.
//
// The body moved to internal/atomicfile, which is a leaf: a package holding a
// switch has to be able to take an atomic write without taking the package
// that reads and rewrites Claude Code's login. The name stayed here because a
// dozen callers spell it and the move is not their business.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicfile.WriteFile(path, data, perm)
}

// TempPattern names the sibling temp file WriteFileAtomic writes beside path,
// as a pattern that is simultaneously an os.CreateTemp pattern and a
// filepath.Glob pattern. daemon.SweepStatusTemps globs it. It is re-exported
// rather than respelled so the writer and the sweeper cannot drift apart.
func TempPattern(path string) string { return atomicfile.TempPattern(path) }
