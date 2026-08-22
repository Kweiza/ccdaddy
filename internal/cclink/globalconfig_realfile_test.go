package cclink

import (
	"os"
	"testing"
)

// Round-trips a REAL ~/.claude.json through decode and encode and requires the
// bytes back unchanged.
//
// This is gate 3 -- the check against the source of truth -- expressed as a
// test. Every unit test in this file's neighbour is written against fixtures
// this package invented, so all of them would pass just as happily against a
// codec that agreed with itself and disagreed with Claude Code. Only a file
// Claude Code actually wrote can settle that, and the properties it settles are
// the ones no fixture would think to include: 90-odd top-level keys, deeply
// nested project history, unicode, and whatever Claude Code's stringify does
// with an empty object three levels down.
//
// It is opt-in by environment variable rather than defaulting to the developer's
// home directory, so reaching outside the sandbox is something a run asks for by
// name:
//
//	CCDAD_REAL_CLAUDE_JSON=~/.claude.json go test ./internal/cclink/
//
// It only ever READS the file.
func TestRealClaudeConfigRoundTripsByteIdentically(t *testing.T) {
	path := os.Getenv("CCDAD_REAL_CLAUDE_JSON")
	if path == "" {
		t.Skip("set CCDAD_REAL_CLAUDE_JSON to a real ~/.claude.json to run this")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	g, err := decodeGlobalConfig(data)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	out, err := g.encode()
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if string(out) == string(data) {
		t.Logf("round-tripped %d bytes and %d top-level keys unchanged", len(data), len(g.Keys()))
		return
	}
	// The file holds the user's project history and may hold an API key, so the
	// failure reports WHERE it diverged and never what was there.
	at := len(data)
	for i := 0; i < len(data) && i < len(out); i++ {
		if data[i] != out[i] {
			at = i
			break
		}
	}
	t.Fatalf("round trip is not byte-identical: %d bytes in, %d out, first difference at offset %d",
		len(data), len(out), at)
}
