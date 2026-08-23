package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Resolving past an npm shim, so that a Windows session gets the same argument
// handling every other platform gets.
//
// The problem, measured rather than assumed. Go 1.26 has ZERO special-casing of
// .bat/.cmd in argument building: syscall.makeCmdLine implements
// CommandLineToArgvW's rules, which cmd.exe does not follow. An argument with
// no space, quote or backslash is emitted RAW, so `-p fix&whoami` reaches
// cmd.exe as two commands. Quoting does not save it either — cmd.exe toggles
// quoting state on every bare `"`, and `%VAR%` expands even inside quotes.
//
// The fix is to stop invoking cmd.exe. An npm shim's whole job is to run
// `node <cli.js>`; doing that directly makes lpApplicationName a real PE, and
// Go's escaping is then exactly correct rather than approximately wrong.
//
// What the shim says is not guesswork. npm generates it with `cmd-shim`, whose
// writeShim_ never reads process.platform — so the generator produces the same
// bytes on any host, and testdata/cmdshim holds real output from it rather than
// a transcription of one machine's file. See that directory's README.

// maxShimSize caps what is read from a .cmd before giving up. A shim is a few
// hundred bytes; anything approaching this is not one, and reading an
// arbitrarily large file that a PATH entry happened to name is not something a
// launcher should do.
const maxShimSize = 64 << 10

// npmShim is what an npm-generated .cmd would have run, with %dp0% expanded.
type npmShim struct {
	// prog is the interpreter the shim prefers — `%dp0%\node.exe`, the copy
	// npm puts beside the shim — or, when the target had no shebang, the
	// target itself.
	prog string
	// fallback is the bare name the shim falls back to when prog is missing,
	// which has to be resolved on PATH. Empty for a shim with no interpreter.
	fallback string
	// args go before the caller's: the shebang's own flags, then the script.
	args []string
	// env is NAME=VALUE from a `#!/usr/bin/env FOO=bar node` shebang, which
	// the shim exports as `@SET` lines before running anything.
	env []string
}

// shimHeadLines are the generator's fixed preamble. All of them must be
// present, and requiring the whole preamble rather than one recognisable line
// is the point: a .cmd on PATH that ccdad did not recognise must fall back to
// refusing, never to a half-parse of a file written by something else.
var shimHeadLines = []string{
	"@ECHO off",
	"GOTO start",
	":find_dp0",
	"SET dp0=%~dp0",
	":start",
	"CALL :find_dp0",
}

// parseNpmShim reads an npm shim's text. shimDir is the directory holding the
// shim, which is what %dp0% expands to.
//
// It reports false for anything it does not fully understand. That is the
// safety property: the caller's fallback is the refusal that shipped before
// this existed, so a shim shape ccdad has never seen costs a refused argument
// rather than a wrong launch.
func parseNpmShim(text, shimDir string) (npmShim, bool) {
	// Split on \n and trim the \r rather than splitting on \r\n. The generator
	// writes CRLF and the fixtures keep it, but a file that has been through a
	// checkout with autocrlf off, or an editor, is still the same shim.
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, strings.TrimSuffix(line, "\r"))
	}
	joined := "\n" + strings.Join(lines, "\n") + "\n"
	for _, want := range shimHeadLines {
		if !strings.Contains(joined, "\n"+want+"\n") {
			return npmShim{}, false
		}
	}

	var shim npmShim
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "@SET "); ok {
			shim.env = append(shim.env, rest)
		}
	}

	// %dp0% is `%~dp0`, which ends with a separator already; the template then
	// writes a second one, and Windows collapses the pair. Expanding to
	// exactly what cmd would have produced keeps this a substitution rather
	// than a path computation — filepath would use the WRONG separator here,
	// because it picks one at build time and this logic is tested on Linux.
	expand := func(s string) (string, bool) {
		s = strings.ReplaceAll(s, "%dp0%", shimDir+`\`)
		// A variable left over is one this does not model, and a quote left
		// over means the token was not the single wrapped word the generator
		// writes. Guessing at either is how a launcher runs the wrong program,
		// so both come back false and the caller refuses.
		return s, !strings.ContainsAny(s, `%"`)
	}

	tail, long := cutShimTail(lines)
	if !long {
		return npmShim{}, false
	}
	fields := splitShimArgs(tail)
	if len(fields) == 0 {
		return npmShim{}, false
	}

	for i, f := range fields {
		v, ok := expand(f)
		if !ok {
			return npmShim{}, false
		}
		fields[i] = v
	}

	if progs := shimProgs(lines); len(progs) == 2 {
		// The interpreter form. fields is `<shebang flags…> <script>`.
		p, pOK := expand(progs[0])
		f, fOK := expand(progs[1])
		if !pOK || !fOK || p == "" || f == "" {
			return npmShim{}, false
		}
		shim.prog, shim.fallback, shim.args = p, f, fields
		return shim, true
	}
	// No interpreter: the shim invokes the target itself, which is what the
	// generator writes when the target has no shebang. fields[0] is that
	// target.
	shim.prog, shim.args = fields[0], fields[1:]
	return shim, true
}

// cutShimTail returns the arguments the shim passes, and whether the line that
// carries them was found.
//
// Two shapes, both from the generator. With an interpreter the last line is
//
//	endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%" <args> <target> %*
//
// and without one it is simply `<target> <args> %*`. The interpreter form is
// anchored on the exact `& "%_prog%" ` rather than on the last `&`, because a
// shebang flag containing an ampersand would otherwise split the line in the
// wrong place.
func cutShimTail(lines []string) (string, bool) {
	const anchor = `& "%_prog%" `
	for _, line := range lines {
		if i := strings.LastIndex(line, anchor); i >= 0 {
			return strings.TrimSuffix(line[i+len(anchor):], " %*"), true
		}
	}
	// The no-interpreter form. It is the last line that ends in ` %*` and is
	// not part of the preamble.
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, ok := strings.CutSuffix(lines[i], " %*"); ok {
			return rest, true
		}
	}
	return "", false
}

// shimProgs returns the two `SET "_prog=…"` values, preferred first.
//
// The `IF EXIST` line above them is deliberately not read. It holds the same
// path as the first SET, and what it expresses is cmd.exe's version of the
// existence check resolvePastShim makes for itself — so parsing the two SETs
// is a model of what the shim would RUN, rather than of how it decides.
func shimProgs(lines []string) []string {
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, `SET "_prog=`); ok {
			out = append(out, strings.TrimSuffix(rest, `"`))
		}
	}
	return out
}

// splitShimArgs tokenises the generator's argument tail.
//
// The grammar is small because the generator's output is: space-separated
// tokens, each either bare or wholly wrapped in double quotes, and runs of
// spaces are collapsed — an empty shebang-args slot leaves two spaces where
// one would do.
//
// It only STRIPS the wrapper; it does not judge what is inside one. The rule
// that a token must carry no quote of its own belongs to parseNpmShim's expand
// step, which sees every token including the two interpreters, and having it
// in both places was a guard mutation testing showed could be deleted with
// nothing failing. One rule, one place.
func splitShimArgs(tail string) []string {
	var out []string
	for _, f := range strings.Fields(tail) {
		if strings.HasPrefix(f, `"`) && strings.HasSuffix(f, `"`) && len(f) >= 2 {
			out = append(out, f[1:len(f)-1])
			continue
		}
		out = append(out, f)
	}
	return out
}

// pastShim is a launch that goes straight to the interpreter.
type pastShim struct {
	path string
	args []string
	env  []string
}

// resolvePastShim decides what to run instead of the shim.
//
// exists and look are injected because both are Windows facts — a node.exe
// beside the shim, and a PATHEXT search — that a test on any other host has to
// be able to describe.
func resolvePastShim(shim npmShim, exists func(string) bool, look func(string) (string, error)) (pastShim, error) {
	if shim.fallback == "" {
		// The target had no shebang, so the shim leans on cmd.exe's own
		// association to decide how to run it. That association is not
		// something ccdad can reproduce — a .js there goes to the Windows
		// Script Host, not to node — so this shape keeps the refusal.
		return pastShim{}, fmt.Errorf("%s runs its target through cmd.exe's file association rather than an interpreter", shim.prog)
	}
	// The shim's own preference, and it is a real one: npm puts a node.exe
	// beside the shim in some installs precisely so the shim does not depend
	// on PATH.
	if shim.prog != "" && exists(shim.prog) {
		return pastShim{path: shim.prog, args: shim.args, env: shim.env}, nil
	}
	resolved, err := look(shim.fallback)
	if err != nil {
		return pastShim{}, fmt.Errorf("resolving %s, the interpreter it runs: %w", shim.fallback, err)
	}
	// Landing on another shim would put cmd.exe straight back in the picture,
	// with one more layer of quoting between the argument and the program.
	// PATHEXT is why this is a real possibility rather than a theoretical one:
	// it is searched per directory, so a .cmd earlier on PATH beats a .exe
	// later.
	if ext := strings.ToLower(extOf(resolved)); ext == ".cmd" || ext == ".bat" || ext == ".ps1" || ext == ".js" {
		return pastShim{}, fmt.Errorf("%s resolves to %s, which cmd.exe would run for us again", shim.fallback, resolved)
	}
	return pastShim{path: resolved, args: shim.args, env: shim.env}, nil
}

// shimDirOf and shimBaseOf are filepath.Dir and filepath.Base for a path in
// the OTHER platform's spelling.
//
// filepath picks its separator at BUILD time, so filepath.Dir of
// `C:\\npm\\claude.cmd` answers "." on Linux — and every line in this file is
// written on Linux and only ever runs on Windows, which is precisely the shape
// that ships having never been executed correctly. Splitting on both
// separators is right on both platforms.
func shimDirOf(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		return path[:i]
	}
	return "."
}

func shimBaseOf(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// extOf is filepath.Ext for a path in the OTHER platform's spelling.
//
// filepath.Ext is safe here — it stops at the last dot and never touches a
// separator — but it is spelled out anyway, because the next reader will
// reasonably wonder, and because filepath.Base beside it would NOT be safe:
// on Linux it treats a whole `C:\a\b.exe` as one name.
func extOf(path string) string {
	if i := strings.LastIndexAny(path, `.\/`); i >= 0 && path[i] == '.' {
		return path[i:]
	}
	return ""
}

// readShim reads a .cmd, refusing anything too large to be one.
//
// Read through a LimitReader rather than stat-then-read, the way cclink reads
// the global config: a size checked before the read is a size that can change
// before the read.
func readShim(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxShimSize+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxShimSize {
		return "", fmt.Errorf("%s is over %d bytes, which is not a shim ccdad will read", path, maxShimSize)
	}
	return string(data), nil
}
