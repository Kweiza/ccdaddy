package config

import (
	"testing"
	"time"
)

// The mode reaches the engine as a bit and nothing else. A config that parsed
// `hover = true` and then handed the ranking a pass with hover off would be a
// setting that silently does nothing, which is the one thing this package
// promises never to ship.
func TestHoverReachesTheRankingPass(t *testing.T) {
	write(t, "hover = true\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RankOptions(time.Now()).Hover {
		t.Error("RankOptions().Hover = false with hover = true in the file")
	}
	if Defaults().RankOptions(time.Now()).Hover {
		t.Error("RankOptions().Hover = true by default; hover is opt-in")
	}
}

// probe_unknown is one of the keys hover overrides, and it is the only one whose
// override has to survive as far as the daemon: hover cannot pace a window that
// has never reported a reset, and a probe is the only thing that puts one there.
func TestHoverForcesTheProbeOnInTheEffectiveConfig(t *testing.T) {
	off, err := Parse([]byte("hover = true\nprobe_unknown = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if off.ProbeUnknown {
		t.Fatal("Parse() applied the override; the listing reads the parsed value and must see the file's own")
	}
	if !off.Effective().ProbeUnknown {
		t.Error("Effective().ProbeUnknown = false under hover; the window with no reset stays un-paced forever")
	}

	kept, err := Parse([]byte("probe_unknown = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if kept.Effective().ProbeUnknown {
		t.Error("Effective() forced the probe on with hover off; the key is the user's answer there")
	}
}

// The daemon reads its config through the reloader and nothing else, so that is
// where the override has to be applied for the probe path to see it.
func TestTheReloaderHandsBackTheEffectiveConfig(t *testing.T) {
	write(t, "hover = true\nprobe_unknown = false\n")

	cfg, err := NewReloader().Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProbeUnknown {
		t.Error("Reload().ProbeUnknown = false under hover; the engine's probe path never sees the override")
	}
}

// Every key is classified against hover, the same way every command in the tree
// is classified against a `ccdad run` session: a key added later has NO verdict
// rather than a permissive one, and this fails until someone writes one down.
//
// The permissive default is the dangerous direction. An unclassified key reads
// as honoured, so a tuning value hover silently overrides would be printed as in
// force forever, and a new money key would be printed as overridden when it is
// not.
func TestEveryKeyIsClassifiedAgainstHover(t *testing.T) {
	var missing, both []string
	for _, key := range Keys() {
		over, honoured := HoverOverrides(key), hoverHonours[key]
		switch {
		case over && honoured:
			both = append(both, key)
		case !over && !honoured:
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("no hover verdict for: %v\nAdd each to hoverOverrides (hover derives it) "+
			"or to hoverHonours (hover reads it).", missing)
	}
	if len(both) > 0 {
		t.Errorf("classified twice: %v", both)
	}
	// The free-form section, which Keys() does not enumerate key by key.
	if !HoverOverrides(windowThresholdSection + ".five_hour") {
		t.Error("a per-window threshold is not marked as overridden")
	}
	// Two independent opt-ins, and hover supplies neither of them.
	if HoverOverrides(keyMaxAutoSpend) {
		t.Fatal("hover overrides the credit ceiling; fully automatic must not become fully automatic spending")
	}
	// The same argument reached from a different surface. hover is a policy for
	// the switching ENGINE; this key says whether an MCP client may move the
	// live login without asking the person at the keyboard, and a mode that
	// derived it would be deciding on their behalf that unattended also means
	// unconfirmed. The classification is what the test above cannot see: it
	// asks only that every key is in exactly one of the two tables.
	if HoverOverrides(keyMCPSwitchWithoutElicitation) {
		t.Fatal("hover overrides the MCP confirm permission; a mode must not grant a permission on the person's behalf")
	}
}
