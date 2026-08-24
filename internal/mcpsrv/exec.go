package mcpsrv

import (
	"strings"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// ReadOut is what every read tool returns.
//
// It deliberately does NOT describe the document it carries. ccdad assembles
// its --json payloads in one place, and that place is the single authority for
// every number in them. A Go struct here mirroring that shape would be a SECOND
// authority, and the test that checks the JSON contract walks the command tree
// only -- so the two could drift with nothing in the suite noticing. The bytes
// therefore travel verbatim and this struct describes only the call.
type ReadOut struct {
	Command  string `json:"command" jsonschema:"the ccdad command line that produced this, exactly as the command line would take it"`
	ExitCode int    `json:"exitCode" jsonschema:"0 the action was taken, 3 the world was already as asked, 4 wanted but blocked, 5 a negative answer whose document is still complete, 2 a usage error, 1 a runtime failure"`
	Meaning  string `json:"meaning" jsonschema:"what that exit code means for this call, in one sentence"`
	Document string `json:"document" jsonschema:"the exact JSON document the command wrote on standard output; parse it as JSON"`
	Notices  string `json:"notices,omitempty" jsonschema:"anything the command wrote on standard error; prose for a person, never part of the document"`
}

// ActionOut is what every tool that CHANGES something returns.
//
// Changed exists because ccdad spends exit 3 for "the world was already as you
// asked", and each store mutator reports a changed flag of its own for exactly
// that. A caller that reads success for both cases reports a change that never
// happened.
type ActionOut struct {
	Command  string `json:"command" jsonschema:"the ccdad command line that ran"`
	ExitCode int    `json:"exitCode" jsonschema:"0 the action was taken, 3 the world was already as asked, 4 wanted but blocked, 2 a usage error, 1 a runtime failure"`
	Meaning  string `json:"meaning" jsonschema:"what that exit code means for this call, in one sentence"`
	Changed  bool   `json:"changed" jsonschema:"true when this call changed something, false when the world was already as asked"`
	Notices  string `json:"notices,omitempty" jsonschema:"what the command said on standard error"`
}

// meaning renders one exit code as the sentence a model reads.
//
// The five arms are ccdad's whole exit contract, and the fall-through is
// mandatory: an unrecognised code is a failure rather than a silent success,
// because the alternative -- returning a zero-valued answer -- reads as "the
// action was taken", which is the worst possible wrong answer for a surface
// that can move a credential.
func meaning(code int) string {
	switch code {
	case 0:
		return "the action was taken"
	case 3:
		return "nothing to do: the world was already as you asked"
	case 4:
		return "blocked: the action was wanted and there is no viable target for it"
	case 5:
		return "a negative answer, complete and not a failure"
	case 2:
		return "usage: the arguments were wrong, and running it again will not help"
	}
	return "ccdad failed"
}

// isToolError says whether a code becomes an error result.
//
// 3 and 5 are answers rather than failures and stay clean. 4 is an error: it
// means the action was wanted and could not be taken, and a caller that reads
// it as success reports something that did not happen.
func isToolError(code int) bool {
	switch code {
	case 0, 3, 5:
		return false
	}
	return true
}

// changedBy is the exit-3 contract as a boolean. Only exit 0 changed anything.
func changedBy(code int) bool { return code == 0 }

// line renders an argument vector the way a person would type it, for the
// envelope's Command field. It is display only and nothing parses it back.
func line(argv []string) string {
	return "ccdad " + strings.Join(argv, " ")
}

// readResult runs a read verb and builds its envelope.
//
// The argument vector is copied before it is handed over, so a caller that
// built it with append over a shared array cannot have it rewritten underneath
// by whatever the executor does with it.
func readResult(e view.Exec, argv ...string) (ReadOut, bool) {
	code, stdout, stderr := e(append([]string{}, argv...))
	return ReadOut{
		Command:  line(argv),
		ExitCode: code,
		Meaning:  meaning(code),
		Document: stdout,
		Notices:  stderr,
	}, isToolError(code)
}

// actionResult runs a verb that changes something and builds its envelope.
func actionResult(e view.Exec, argv ...string) (ActionOut, bool) {
	code, stdout, stderr := e(append([]string{}, argv...))
	// A verb in this class writes prose, not a document. Anything it did put on
	// stdout still belongs to the caller, so it is joined onto the notices
	// rather than dropped: a silent drop here would hide the one line a future
	// verb starts printing.
	notices := stderr
	if stdout != "" {
		notices = stdout + stderr
	}
	return ActionOut{
		Command:  line(argv),
		ExitCode: code,
		Meaning:  meaning(code),
		Changed:  changedBy(code),
		Notices:  notices,
	}, isToolError(code)
}
