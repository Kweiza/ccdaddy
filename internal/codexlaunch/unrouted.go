package codexlaunch

import (
	"bytes"
	"os"
	"path/filepath"
)

// UnroutedName is the tally's basename, beside the launch records.
const UnroutedName = "unrouted"

// UnroutedPath is <root>/codex/unrouted: how many codex sessions a launcher had
// to start WITHOUT ccdad's proxy in front of them.
//
// A FILE and not a message, because there is nothing to send a message to. The
// launcher is a short-lived CLI process; the daemon it would tell is the very
// thing that is not running, which is why the launch was unrouted at all; and
// by the time a daemon exists the launcher is long gone. The daemon reads this
// when it publishes, so `ccdad doctor` and `ccdad status` can say that some
// codex sessions are spending whatever the user's own codex home holds -- an
// account ccdad neither chose nor can see.
//
// Beside the launch records rather than in a directory of its own, so that
// `ccdad uninstall` takes it with a name it already recognises.
func UnroutedPath(root string) string {
	return filepath.Join(filepath.Dir(Dir(root)), UnroutedName)
}

// NoteUnrouted records one unrouted launch.
//
// ONE APPENDED BYTE, never read-modify-write. Two launchers can start in the
// same second -- two terminals, a shell script, an editor task -- and a
// read-add-write from each would lose one of the two silently. An O_APPEND
// write of a single byte is delivered whole, so the count is simply how many
// newlines the file holds, and nothing has to be parsed.
//
// It is cumulative for the life of the store and is never reset. That is the
// honest shape: the question a reader has is "has this machine ever run codex
// outside ccdad", and a counter that cleared itself would answer no for a
// machine where it happens every morning.
func NoteUnrouted(root string) error {
	path := UnroutedPath(root)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// UnroutedCount is how many unrouted launches the tally records.
//
// A file that is not there is none, which is the ordinary state of a machine
// whose codex has always been routed. A file that holds something else is a
// wrong count and never an error: the only consumer is a status document, and
// refusing to publish one over a byte in a tally would take the whole document
// down with it.
func UnroutedCount(root string) int {
	raw, err := os.ReadFile(UnroutedPath(root))
	if err != nil {
		return 0
	}
	return bytes.Count(raw, []byte{'\n'})
}
