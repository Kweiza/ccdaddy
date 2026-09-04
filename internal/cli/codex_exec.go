package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Kweiza/ccdaddy/internal/config"
)

// codexProgramName is what PATH is searched for. It carries no extension:
// exec.LookPath adds the ones in PATHEXT on Windows, which is where a codex is
// a .cmd or a .exe, and spelling one here would be a Windows fact written on a
// Linux machine.
const codexProgramName = "codex"

// errNoCodex is a machine with a shim and nothing behind it.
//
// It names the way out, because there are two and neither is guessable: codex
// may simply not be installed, or it may be installed somewhere that is not on
// this shell's PATH -- an editor's bundled copy, a version manager's shim
// directory -- in which case naming it is the answer rather than changing PATH.
var errNoCodex = errors.New("ccdad cannot find a codex on PATH outside its own shim directory. " +
	"Install codex, or name the one to use with `ccdad config set codex.binary <path>`")

// realCodexPath is the codex the shim stands in front of: the first PATH
// component outside shimDir that holds an executable codex.
//
// A hand walk rather than exec.LookPath, and the shim is the whole reason.
// LookPath would find <CCDAD_HOME>/bin/codex, which execs `ccdad codex exec`,
// which would resolve codex again -- an unbounded loop with a process per turn
// of it. Skipping one component is the only difference from LookPath, and it
// cannot be expressed to LookPath.
//
// livePathRules and never path/filepath for the SPLIT: a PATH list is
// ':'-separated here and ';'-separated on Windows, and os.PathListSeparator
// answers ':' for both under `go test` on Linux -- so a Windows bug written
// with it passes on this machine and ships. The JOIN below is filepath's,
// because that half is a real path on the platform this runs on.
//
// A RELATIVE component is skipped rather than joined, and that is not
// tidiness: filepath.Join(".", "codex") is "codex", with no separator in it,
// and exec.LookPath given a bare name searches the whole PATH -- which puts
// the shim straight back into the answer this function exists to keep it out
// of.
func realCodexPath(shimDir string) (string, error) {
	for _, dir := range livePathRules.split(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if shimDir != "" && livePathRules.same(dir, shimDir) {
			continue
		}
		path, err := exec.LookPath(filepath.Join(dir, codexProgramName))
		if err != nil {
			continue
		}
		return path, nil
	}
	return "", errNoCodex
}

// resolveCodex is the walk behind a seam, so a test can describe a machine
// whose codex is somewhere the test put it. Production never reassigns it.
var resolveCodex = realCodexPath

// codexBinary is the codex this launch will run.
//
// The configured path WINS rather than being a fallback. It is the escape
// hatch for a machine the walk cannot get right -- a codex that is not on PATH
// at all, or two of them where the first is not the one wanted -- and a
// setting that only applied when the walk failed would be a setting that
// silently did nothing on exactly the machines it was set on.
//
// A configured path that is not there is a usage error rather than a quiet
// fallback to PATH. The user said which codex to run; running a different one
// would bill a session through a binary they did not choose.
func codexBinary() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.Codex.Binary == "" {
		return resolveCodex(shimDir())
	}
	if _, err := os.Stat(cfg.Codex.Binary); err != nil {
		return "", UsageError("codex.binary in %s names %s, which ccdad cannot use (%v). "+
			"Correct it with `ccdad config set codex.binary <path>`, or clear it with "+
			"`ccdad config unset codex.binary` to use the first codex on PATH",
			config.FileName, cfg.Codex.Binary, err)
	}
	return cfg.Codex.Binary, nil
}

// codexKeyEnv carries the per-launch secret to the child. codex reads it
// because the launch declares `env_key = "CCDAD_CODEX_KEY"` for its model
// provider, so the value becomes the bearer on every request codex makes.
//
// It is treated as PUBLIC within the session, which is why the launch also
// excludes it from the environment codex hands to agent commands: the boundary
// here is the uid, a same-uid process can read the launcher's environment
// anyway, and what the secret authorises is exactly one route for one launch --
// never an OAuth token, and nothing on any other ccdad surface.
const codexKeyEnv = "CCDAD_CODEX_KEY"
