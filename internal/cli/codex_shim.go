package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The shim is how `codex`, typed bare at a prompt, reaches ccdad. It is a
// two-line POSIX script in a directory of ccdad's own, put ahead of the real
// codex on PATH -- and it exists because codex holds its credential in memory
// for the life of the process and re-reads it on exactly one condition, an
// HTTP 401. Quota exhaustion is a 429, so the two never meet: no file swap can
// move a running codex to another account, and the honest capability from the
// file layer is a launch-time chooser. The shim is that chooser, and what it
// launches is a codex whose API base is ccdad's own loopback proxy.
//
// The alternative was editing the user's ~/.codex/config.toml, and it was
// refused: that file is theirs, a merge of ccdad's settings into it survives
// `ccdad uninstall` only if uninstall can un-merge it exactly, and any codex
// the user starts from an editor or a desktop app would pick the settings up
// with no ccdad in sight to serve them.

// shimDirName is the directory under CCDAD_HOME that goes on PATH ahead of the
// real codex. It holds the shim and nothing else.
const shimDirName = "bin"

// shimRecordName is the install record's basename directly under CCDAD_HOME.
//
// The record is what `setup-path` keys the derived directory set on, so it is
// the thing that says "this machine wants <CCDAD_HOME>/bin on PATH". The shim
// script's own existence is deliberately NOT that signal: a user who deleted
// the script by hand has said something, and a PATH entry pointing at an empty
// directory is harmless where a re-created script would be a surprise.
const shimRecordName = "codex-shim.json"

// shimProgramName is what the shim is called. It is the name codex is invoked
// by, which is the whole mechanism.
const shimProgramName = "codex"

// shimOS is the platform the shim decision is made for.
//
// A var rather than runtime.GOOS inline, so the Windows refusal can be
// exercised on the machine this package's tests run on. Windows gets no shim in
// v1: the shim would have to be a .cmd, which puts cmd.exe in front of every
// prompt codex is handed, and this tree refuses that launch rather than
// mangling the arguments -- so a Windows shim would turn a working prompt into
// a usage error. Production never reassigns this.
var shimOS = runtime.GOOS

// codexShimRecord is what ccdad writes down about the shim it installed.
//
// The PATH is recorded rather than recomputed because CCDAD_HOME can move: a
// record naming a directory that is no longer the one ccdad would choose is
// exactly what `ccdad doctor` has to be able to see.
type codexShimRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	Path          string `json:"path"`
	InstalledAt   string `json:"installedAt"`
}

// shimDir is <CCDAD_HOME>/bin, or "" when the store cannot be resolved.
//
// An empty string rather than an error, because every caller's answer to an
// unresolvable home is the same -- there is no shim directory -- and threading
// an error through the PATH walk and the doctor row would make three of them
// say the same thing three different ways. The one caller that must not treat
// "" as a directory is registeredDirs, which skips it by name.
func shimDir() string {
	root, err := ccpath.StoreHome()
	if err != nil {
		return ""
	}
	return filepath.Join(root, shimDirName)
}

// shimPath is the shim script itself.
func shimPath() string {
	dir := shimDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, shimProgramName)
}

// shimRecordPath is <CCDAD_HOME>/codex-shim.json.
func shimRecordPath() string {
	root, err := ccpath.StoreHome()
	if err != nil {
		return ""
	}
	return filepath.Join(root, shimRecordName)
}

// shimRecord reads the install record. A record that is absent, unreadable or
// unparseable all answer the same: this machine has not asked for a shim.
//
// Unparseable is folded in with absent deliberately. The only consumer is a
// directory set, and refusing to register a directory because a small JSON
// document is damaged would take codex off ccdad's proxy over a file the user
// can neither see nor be told about from here -- `ccdad codex shim install`
// rewrites it, which is the repair.
func shimRecord() (codexShimRecord, bool) {
	path := shimRecordPath()
	if path == "" {
		return codexShimRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return codexShimRecord{}, false
	}
	var rec codexShimRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return codexShimRecord{}, false
	}
	return rec, true
}
