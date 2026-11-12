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

// cacheFor is how long one walk's answer stands in this process. The daemon asks
// once a second while the return is armed, and a walk can go on being asked for
// days when no move follows -- a cooldown, a switch that keeps failing, a
// directory it cannot read. An answer this old can read a session that resumed
// inside it as still resting, and that costs at most the one cache write that
// session's first turn made: the rest it read was an hour long
// (strategy.RestQuiet), so the cache from before it had already expired.
const cacheFor = 15 * time.Second

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

// note counts one file if it is a transcript. One dated past the clock counts
// as written now: a clock stepped back makes the live session's own writes
// look like the future, and passing them over would read its older ones as
// rest in the middle of work. The cost is a file dated far ahead, which holds
// the return off until it is written again or removed.
func (w *walker) note(path string, info fs.FileInfo) {
	if !strings.HasSuffix(path, ".jsonl") || !info.Mode().IsRegular() {
		return
	}
	at := info.ModTime()
	if at.After(w.now) {
		at = w.now
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
	found  bool
}

// Last is LastWrite on Claude Code's own config root, every answer -- nothing
// found included -- reused for cacheFor in this process.
func Last(now time.Time) (time.Time, bool) {
	home, err := ccpath.ConfigHome()
	if err != nil {
		return time.Time{}, false
	}
	cache.Lock()
	defer cache.Unlock()
	if age := now.Sub(cache.at); cache.home == home && !cache.at.IsZero() && age >= 0 && age < cacheFor {
		return cache.newest, cache.found
	}
	cache.newest, cache.found = LastWrite(home, now)
	cache.home, cache.at = home, now
	return cache.newest, cache.found
}
