package codexproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePortPrefersTheConfiguredPort(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	if err := RecordPort(root, 24242); err != nil {
		t.Fatal(err)
	}
	port, source, err := ResolvePort(root, 31000)
	if err != nil {
		t.Fatalf("ResolvePort() error = %v, want nil", err)
	}
	if port != 31000 || source != "config" {
		t.Fatalf("ResolvePort() = (%d, %q), want (31000, \"config\")", port, source)
	}
}

func TestResolvePortFallsBackToTheRecordedPort(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	if err := RecordPort(root, 24242); err != nil {
		t.Fatalf("RecordPort() = %v, want nil", err)
	}
	port, source, err := ResolvePort(root, 0)
	if err != nil {
		t.Fatalf("ResolvePort() error = %v, want nil", err)
	}
	if port != 24242 || source != "recorded" {
		t.Fatalf("ResolvePort() = (%d, %q), want (24242, \"recorded\")", port, source)
	}
}

func TestADerivedPortIsInTheBandAndIsStable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	port, source, err := ResolvePort(root, 0)
	if err != nil {
		t.Fatalf("ResolvePort() error = %v, want nil", err)
	}
	if source != "derived" {
		t.Fatalf("source = %q, want derived for a store that has recorded nothing", source)
	}
	// The band the derivation has to land in, checked against the literals the
	// two constants are supposed to hold rather than against the constants
	// themselves. 20000-31999 sits below every operating system's ephemeral
	// range -- Linux starts at 32768, the BSDs and Windows at 49152 -- so a port
	// derived into it cannot be handed by the kernel to somebody else's outbound
	// socket while the daemon is down. Asserting that band against derivedBase
	// and derivedSpan would be a tautology, and measured on this file: written
	// that way, all eight tests stayed green with derivedBase moved to 49152,
	// inside the BSD and Windows ephemeral range, and green again with it moved
	// to 200000, which is not a port number at all.
	if derivedBase < 20000 || derivedBase+derivedSpan > 32000 {
		t.Fatalf("the derivation band is %d-%d, want it inside 20000-31999", derivedBase, derivedBase+derivedSpan-1)
	}
	// And the port this root actually derived, which is what catches a
	// derivation that leaves its own band -- an offset added after the modulo,
	// or the modulo dropped -- while the two constants above stay right. It is
	// one sample, so it is the constant check above and not this one that has to
	// carry a widened span: at derivedSpan 12001 a single derived port escapes
	// 20000-31999 about one run in twelve thousand.
	if port < 20000 || port > 31999 {
		t.Fatalf("derived port %d is outside 20000-31999", port)
	}
	again, _, err := ResolvePort(root, 0)
	if err != nil || again != port {
		t.Fatalf("ResolvePort() = (%d, %v) on a second call, want (%d, nil)", again, err, port)
	}
	// The same directory spelled two ways is one store, so it derives one port.
	messy, _, err := ResolvePort(root+string(filepath.Separator)+"."+string(filepath.Separator), 0)
	if err != nil || messy != port {
		t.Fatalf("a redundant spelling derived %d, want %d", messy, port)
	}
}

// A SPREAD rather than "these two differ". Two random temp-dir paths collide
// modulo the band once in twelve thousand runs, which over a CI matrix is a
// flake that would be read as a real defect; eight roots all landing on one
// port is not something a working derivation can do.
func TestDifferentStoresSpreadAcrossTheBand(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 8; i++ {
		p, _, err := ResolvePort(filepath.Join(t.TempDir(), "store"), 0)
		if err != nil {
			t.Fatal(err)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("eight different store roots all derived port %v", seen)
	}
}

func TestResolvePortRefusesAConfiguredPortThatIsNotOne(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	for _, configured := range []int{-1, 70000} {
		if _, _, err := ResolvePort(root, configured); err == nil {
			t.Errorf("ResolvePort(root, %d) = nil error, want a refusal", configured)
		}
	}
}

func TestRecordPortRefusesAPortNobodyIsListeningOn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	// A :0 bind resolves to a real port before anything records it; 0 reaching
	// this function means the caller recorded the REQUEST rather than the
	// answer, and a store that remembers 0 asks the kernel for a random port on
	// every restart, which is the one thing the recorded port exists to stop.
	if err := RecordPort(root, 0); err == nil {
		t.Fatal("RecordPort(root, 0) = nil, want a refusal")
	}
	if _, err := os.Stat(PortPath(root)); !os.IsNotExist(err) {
		t.Error("a refused RecordPort still wrote the file")
	}
}

func TestAnUnreadablePortRecordFallsThroughToTheDerivedPort(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	if err := os.MkdirAll(filepath.Dir(PortPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "not a port", "0", "99999"} {
		if err := os.WriteFile(PortPath(root), []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		_, source, err := ResolvePort(root, 0)
		if err != nil {
			t.Fatalf("ResolvePort() error = %v for record %q, want nil", err, junk)
		}
		if source != "derived" {
			t.Errorf("record %q resolved as %q, want derived", junk, source)
		}
	}
}

func TestRecordPortWritesOneLineUnderTheCodexDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccdad")
	if err := RecordPort(root, 24242); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "codex", "port")
	if PortPath(root) != want {
		t.Fatalf("PortPath() = %q, want %q", PortPath(root), want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "24242" {
		t.Fatalf("the record holds %q, want 24242", string(data))
	}
}
