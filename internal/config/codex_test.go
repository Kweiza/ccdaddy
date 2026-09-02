package config

import (
	"slices"
	"strings"
	"testing"
)

// The four Codex keys are in the closed set, so `ccdad config set` accepts
// them and the loader reads them. A key in one and not the other is the drift
// keys.go exists to prevent -- it would print a key `config set` then refuses.
func TestTheCodexKeysAreInTheClosedSet(t *testing.T) {
	want := []string{"codex.threshold", "codex.binary", "codex.proxy_port", "codex.cross_account_replay"}
	keys := Keys()
	for _, k := range want {
		if !slices.Contains(keys, k) {
			t.Errorf("Keys() does not list %q: %v", k, keys)
		}
		if !isKnownKey(k) {
			t.Errorf("isKnownKey(%q) = false", k)
		}
	}
	if !isKnownSection("codex") {
		t.Error(`isKnownSection("codex") = false, so a [codex] table is reported as a whole unknown section`)
	}
}

// The defaults, each an answer rather than an omission.
func TestTheCodexDefaults(t *testing.T) {
	d := Defaults().Codex
	if d.Threshold != 80 {
		t.Errorf("Codex.Threshold = %v, want 80", d.Threshold)
	}
	if d.Binary != "" {
		t.Errorf("Codex.Binary = %q, want the empty string, which means the first codex on PATH", d.Binary)
	}
	if d.ProxyPort != 0 {
		t.Errorf("Codex.ProxyPort = %d, want 0, which means resolve one", d.ProxyPort)
	}
	if d.CrossAccountReplay {
		t.Error("Codex.CrossAccountReplay is on by default; it bills a second account for a thread the first started")
	}
}

func TestParseReadsTheCodexTable(t *testing.T) {
	cfg, err := Parse([]byte("[codex]\nthreshold = 65\nbinary = \"/opt/codex/bin/codex\"\n" +
		"proxy_port = 24680\ncross_account_replay = true\n"))
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if cfg.Codex.Threshold != 65 {
		t.Errorf("Codex.Threshold = %v, want 65", cfg.Codex.Threshold)
	}
	if cfg.Codex.Binary != "/opt/codex/bin/codex" {
		t.Errorf("Codex.Binary = %q", cfg.Codex.Binary)
	}
	if cfg.Codex.ProxyPort != 24680 {
		t.Errorf("Codex.ProxyPort = %d, want 24680", cfg.Codex.ProxyPort)
	}
	if !cfg.Codex.CrossAccountReplay {
		t.Error("Codex.CrossAccountReplay = false after the file said true")
	}
}

// A [codex] table naming ONE key leaves the other three at their defaults. It
// is the reason the table is a pointer to a struct of pointers rather than a
// value: without it, writing `binary` would reset the threshold to zero.
func TestACodexTableWithOneKeyLeavesTheRestAlone(t *testing.T) {
	cfg, err := Parse([]byte("[codex]\nbinary = \"/usr/local/bin/codex\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.Threshold != Defaults().Codex.Threshold {
		t.Errorf("Codex.Threshold = %v, want the default %v", cfg.Codex.Threshold, Defaults().Codex.Threshold)
	}
	if cfg.Codex.CrossAccountReplay {
		t.Error("Codex.CrossAccountReplay changed for a file that never mentioned it")
	}
}

// The port is held to the range the proxy actually resolves within, and 0 is
// the way to say "resolve one". A privileged or out-of-range port written by
// hand is refused at load rather than at bind, where the failure is a daemon
// that will not start.
func TestTheProxyPortIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"zero means resolve one", "[codex]\nproxy_port = 0\n", false},
		{"in range", "[codex]\nproxy_port = 20000\n", false},
		{"privileged", "[codex]\nproxy_port = 80\n", true},
		{"above the port space", "[codex]\nproxy_port = 70000\n", true},
		{"negative", "[codex]\nproxy_port = -1\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if tc.wantErr && err == nil {
				t.Fatalf("Parse(%q) = nil, want an error", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tc.body, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "codex.proxy_port") {
				t.Errorf("Parse error = %q, want it to name the key", err)
			}
		})
	}
}

// `ccdad config set` and `ccdad config get` answer for all four.
func TestTheCodexKeysRoundTripThroughSetAndValue(t *testing.T) {
	for _, tc := range []struct{ key, set, want string }{
		{"codex.threshold", "65", "65"},
		{"codex.binary", "/opt/codex", "/opt/codex"},
		{"codex.proxy_port", "24680", "24680"},
		{"codex.cross_account_replay", "true", "true"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			d := newDocument()
			if err := d.Set(tc.key, tc.set); err != nil {
				t.Fatalf("Set(%q, %q) = %v", tc.key, tc.set, err)
			}
			cfg, err := d.Config()
			if err != nil {
				encoded, _ := d.Encode()
				t.Fatalf("the document Set wrote does not parse: %v\n%s", err, encoded)
			}
			got, err := cfg.Value(tc.key)
			if err != nil {
				t.Fatalf("Value(%q) = %v", tc.key, err)
			}
			if got != tc.want {
				t.Errorf("Value(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}
