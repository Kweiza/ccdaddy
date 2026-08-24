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
