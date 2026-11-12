// Package ccactivity says when Claude Code last did anything on this machine.
//
// Claude Code appends to a session's transcript, projects/<project>/<session>.jsonl
// under its config root, on every prompt, every reply and every tool result, so
// the newest write to any transcript is the last moment some session was
// working. Hover reads it for one decision only -- whether the work is resting
// (strategy.Options.LastActivity) -- and asks only when that decision would
// move the session, so the walk is rare.
//
// Every doubt reads as activity, never as rest: a walk that could not see the
// whole tree finds nothing, and the engine never moves the session on nothing.
package ccactivity

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// cacheFor is how long a walk that found recent activity stands in this process.
// The daemon asks once a second while the return is armed, and a walk over a
// few thousand transcripts costs about a fifth of a second. Only an answer that
// says the work is active is reused (Last): one that would let the session move
// is always a fresh walk.
const cacheFor = 15 * time.Second

// skewAllowed is how far ahead of the clock a transcript's mtime may be and
// still be read. Further ahead is a clock that moved, not a write, and one such
// file would otherwise stand as the newest write until the clock caught up.
const skewAllowed = time.Minute

// LastWrite is the newest modification time of any transcript under configHome
// as of now, and whether the walk saw the whole tree and found one. Symlinked
// directories are followed once each; any entry it cannot read makes the
// answer "nothing found", since a write it could not see is still a write.
func LastWrite(configHome string, now time.Time) (time.Time, bool) {
	root, err := filepath.EvalSymlinks(filepath.Join(configHome, "projects"))
	if err != nil {
		return time.Time{}, false
	}
	w := walker{now: now, seen: map[string]bool{}}
	if !w.walk(root) || w.newest.IsZero() {
		return time.Time{}, false
	}
	return w.newest, true
}

type walker struct {
	now    time.Time
	newest time.Time
	seen   map[string]bool
}

// walk visits dir and reports whether every entry under it could be read.
func (w *walker) walk(dir string) bool {
	if w.seen[dir] {
		return true
	}
	w.seen[dir] = true
	complete := true
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			return fs.SkipAll
		}
		if d.Type()&fs.ModeSymlink != 0 {
			real, rerr := filepath.EvalSymlinks(path)
			info, serr := os.Stat(path)
			switch {
			case rerr != nil || serr != nil:
				complete = false
				return fs.SkipAll
			case info.IsDir():
				if !w.walk(real) {
					complete = false
					return fs.SkipAll
				}
			default:
				w.note(path, info)
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			complete = false
			return fs.SkipAll
		}
		w.note(path, info)
		return nil
	})
	return complete && err == nil
}

// note counts one file if it is a transcript written no later than the clock
// allows.
func (w *walker) note(path string, info fs.FileInfo) {
	if !strings.HasSuffix(path, ".jsonl") || !info.Mode().IsRegular() {
		return
	}
	at := info.ModTime()
	if at.After(w.now.Add(skewAllowed)) {
		return
	}
	if at.After(w.newest) {
		w.newest = at
	}
}

var cache struct {
	sync.Mutex
	home   string
	at     time.Time
	newest time.Time
}

// Last is LastWrite on Claude Code's own config root. A walk that found
// activity younger than quiet is reused for cacheFor; every other answer --
// one that would read as rest, or nothing found -- is walked again, so a
// session that resumed since the cached walk is never read as resting.
func Last(now time.Time, quiet time.Duration) (time.Time, bool) {
	home, err := ccpath.ConfigHome()
	if err != nil {
		return time.Time{}, false
	}
	cache.Lock()
	defer cache.Unlock()
	if age := now.Sub(cache.at); cache.home == home && !cache.at.IsZero() && age >= 0 && age < cacheFor &&
		now.Sub(cache.newest) < quiet {
		return cache.newest, true
	}
	newest, found := LastWrite(home, now)
	cache.home, cache.at, cache.newest = home, now, time.Time{}
	if found {
		cache.newest = newest
	} else {
		cache.at = time.Time{}
	}
	return newest, found
}
