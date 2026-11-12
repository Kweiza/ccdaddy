// Package ccactivity says when Claude Code last did anything on this machine.
//
// Claude Code appends to a session's transcript, projects/<project>/<session>.jsonl
// under its config root, on every prompt, every reply and every tool result, so
// the newest write to any transcript is the last moment some session was
// working. Hover reads it for one decision only -- whether the work is resting
// (strategy.Options.LastActivity) -- and asks only when that decision could
// move the session, so the walk is rare.
package ccactivity

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// cacheFor is how long one walk's answer stands in this process. The daemon
// asks once a second while the rest return is armed, and a walk over a few
// thousand transcripts costs about a fifth of a second; an answer this old can
// read a session that resumed inside it as still resting, which costs that
// session a warm cache and nothing else.
const cacheFor = 15 * time.Second

// LastWrite is the newest modification time of any transcript under configHome,
// and whether there was one. An unreadable entry is skipped: the answer is
// about what can be seen, and nothing seen is not rest (strategy reads false as
// "not resting").
func LastWrite(configHome string) (time.Time, bool) {
	var newest time.Time
	root := filepath.Join(configHome, "projects")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr == nil && info.Mode().IsRegular() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, !newest.IsZero()
}

var cache struct {
	sync.Mutex
	home   string
	at     time.Time
	newest time.Time
	found  bool
}

// Last is LastWrite on Claude Code's own config root, cached for cacheFor in
// this process. now is the caller's clock, so the cache ages with it.
func Last(now time.Time) (time.Time, bool) {
	home, err := ccpath.ConfigHome()
	if err != nil {
		return time.Time{}, false
	}
	cache.Lock()
	defer cache.Unlock()
	if cache.home == home && !cache.at.IsZero() && now.Sub(cache.at) >= 0 && now.Sub(cache.at) < cacheFor {
		return cache.newest, cache.found
	}
	cache.newest, cache.found = LastWrite(home)
	cache.home, cache.at = home, now
	return cache.newest, cache.found
}
