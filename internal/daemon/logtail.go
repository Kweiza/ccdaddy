package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// tailWindow is how much of the end of the log a bounded read looks at.
//
// 8 KB is sized against what it is for: a dashboard refreshing every ten
// seconds wants the last handful of lines, and this daemon's own lines run to
// well under a hundred bytes each, so eight kilobytes holds far more than the
// ten anybody asks for. It is also small enough that reading it on every
// refresh costs a seek and one page or two — which is the whole point, because
// the alternative already in this binary reads the entire file.
const tailWindow = 8 << 10

// TailLog is the last n lines of the daemon log, read from a bounded window at
// the end of the file.
//
// This is a SECOND reader beside the one `ccdad daemon logs` uses, and that is
// deliberate rather than an oversight. That one reads the whole file, which is
// exactly right for a command that takes a line count of zero meaning "all of
// it", and exactly wrong for a screen that refreshes on a timer. Neither of
// them publishes a number the other publishes — they answer "show me the log"
// and "show me the tail of it right now" — so the rule that keeps one figure
// in one place is not engaged here.
//
// It opens and closes on every call and never holds the handle across
// refreshes. The daemon rotates by RENAMING, so a reader that kept its handle
// would go on reading the renamed inode with every line after the first
// rotation silently lost, forever. On Windows a held handle is worse than
// that: Go's open asks for FILE_SHARE_READ and FILE_SHARE_WRITE and not
// FILE_SHARE_DELETE, so the handle BLOCKS the rename outright and wedges
// rotation for as long as the screen is up.
//
// A missing file is "no log yet" and not an error: a machine where no daemon
// has ever run is the ordinary state of a fresh install.
func TailLog(n int) ([]string, error) {
	if n < 1 {
		return nil, nil
	}
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", LogFileName, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", LogFileName, err)
	}

	from := info.Size() - tailWindow
	if from < 0 {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, fmt.Errorf("reading %s: %w", LogFileName, err)
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", LogFileName, err)
	}

	// A window that starts mid-file almost always starts mid-LINE, and half a
	// line presented as a whole one is worse than one line fewer: it reads as
	// a log entry that begins with a fragment of a timestamp. Dropping it is
	// only correct because the window did not start at the beginning of the
	// file — at offset zero the first line is a real first line.
	if from > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}

	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
