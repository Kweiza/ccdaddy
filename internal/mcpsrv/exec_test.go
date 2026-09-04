package mcpsrv

import (
	"strings"
	"testing"
)

// ccdad's exit codes are a contract, not a status: 3 means the world was
// already as the caller asked and 5 means the answer is negative and complete.
// A model that reads either as a failure retries something that needs no retry;
// a model that reads 4 as success reports a switch that never happened.
func TestEveryExitCodeMapsToOneAnswerAndNotToAGuess(t *testing.T) {
	for _, tc := range []struct {
		code    int
		isError bool
		changed bool
		says    string
	}{
		{0, false, true, "taken"},
		{3, false, false, "already"},
		{4, true, false, "blocked"},
		{5, false, false, "negative"},
		{2, true, false, "usage"},
		{1, true, false, "failed"},
		{99, true, false, "failed"},
	} {
		if got := isToolError(tc.code); got != tc.isError {
			t.Errorf("isToolError(%d) = %v, want %v", tc.code, got, tc.isError)
		}
		if got := changedBy(tc.code); got != tc.changed {
			t.Errorf("changedBy(%d) = %v, want %v", tc.code, got, tc.changed)
		}
		if m := meaning(tc.code); !strings.Contains(m, tc.says) {
			t.Errorf("meaning(%d) = %q, want it to contain %q", tc.code, m, tc.says)
		}
	}
}

// The envelope carries the document as BYTES. If it ever grows a field per
// payload key, the numbers in it have a second source and the JSON contract
// test -- which only walks the command tree -- would never see them drift.
func TestTheReadEnvelopeCarriesTheDocumentVerbatim(t *testing.T) {
	doc := "{\n  \"schemaVersion\": 1,\n  \"accounts\": []\n}\n"
	e := func(argv []string) (int, string, string) { return 0, doc, "" }

	out, isErr := readResult(e, "status", "--json")
	if isErr {
		t.Fatalf("a clean status reported an error: %+v", out)
	}
	if out.Document != doc {
		t.Errorf("Document = %q, want the exact bytes the command wrote", out.Document)
	}
	if out.Command != "ccdad status --json" {
		t.Errorf("Command = %q, want ccdad status --json", out.Command)
	}
}

// Exit 5 is the case that catches a naive mapping: which, config get and daemon
// status all write their FULL payload and exit 5. Treating non-zero as failure
// throws the answer away.
func TestANegativeProbeAnswerKeepsItsDocumentAndIsNotAnError(t *testing.T) {
	e := func(argv []string) (int, string, string) {
		return 5, "{\"schemaVersion\":1,\"attributed\":false}\n", "not one ccdad manages\n"
	}
	out, isErr := readResult(e, "which", "--json")
	if isErr {
		t.Error("exit 5 was reported as a tool error; it is a complete negative answer")
	}
	if !strings.Contains(out.Document, "attributed") {
		t.Errorf("Document = %q, want the full payload", out.Document)
	}
	if !strings.Contains(out.Notices, "not one ccdad manages") {
		t.Errorf("Notices = %q, want the command's own stderr line", out.Notices)
	}
}

// The action envelope reports the world's movement, not the command's noise.
// A store mutator that found nothing to do exits 3 and prints a line saying so;
// a caller that read that line as evidence of a change would report one that
// never happened, and Changed is the field that stops it.
func TestTheActionEnvelopeSeparatesWhatMovedFromWhatWasSaid(t *testing.T) {
	e := func(argv []string) (int, string, string) {
		return 3, "already enabled\n", "nothing to do\n"
	}
	out, isErr := actionResult(e, "enable", "work@example.com")
	if isErr {
		t.Error("exit 3 was reported as a tool error; it is an answer, not a failure")
	}
	if out.Changed {
		t.Error("Changed is true on exit 3; nothing moved")
	}
	if out.Command != "ccdad enable work@example.com" {
		t.Errorf("Command = %q, want the command line as the CLI would take it", out.Command)
	}
	// A verb in this class writes prose rather than a document, and dropping
	// its stdout would hide the one line a future verb starts printing there.
	if !strings.Contains(out.Notices, "already enabled") || !strings.Contains(out.Notices, "nothing to do") {
		t.Errorf("Notices = %q, want both what it printed and what it warned", out.Notices)
	}
}
