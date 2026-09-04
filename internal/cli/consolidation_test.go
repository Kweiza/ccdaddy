package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/config"
)

func TestThePublicCommandTreeHasOneStrategyAndOneStatusSurface(t *testing.T) {
	root := NewRootCmd()
	var names []string
	for _, cmd := range root.Commands() {
		if !cmd.Hidden {
			names = append(names, cmd.Name())
		}
	}
	for _, removed := range []string{"hover", "list", "manual", "tui"} {
		if slices.Contains(names, removed) {
			t.Errorf("public command tree still contains %q: %v", removed, names)
		}
	}
	if !slices.Contains(names, "strategy") {
		t.Errorf("public command tree has no strategy command: %v", names)
	}
}

func TestStrategySelectsExactlyOnePolicy(t *testing.T) {
	for _, name := range []string{"hover", "manual", "headroom", "consume-first"} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if code, _, stderr, top := runRoot(t, "strategy", name); code != ExitOK && code != ExitNothingToDo {
				t.Fatalf("strategy %s exited %d: %s%s", name, code, stderr, top)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := selectedStrategy(cfg); got != name {
				t.Errorf("selected strategy = %q, want %q", got, name)
			}
			if cfg.Hover && cfg.Manual {
				t.Fatal("hover and manual were both enabled")
			}
		})
	}
}

func TestStrategyRejectsANameOutsideTheFourChoices(t *testing.T) {
	isolate(t)
	code, _, stderr, top := runRoot(t, "strategy", "recovery")
	if code != ExitUsage {
		t.Fatalf("strategy recovery exited %d, want %d", code, ExitUsage)
	}
	message := stderr + top
	for _, want := range []string{"hover", "manual", "headroom", "consume-first"} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not name %q: %s", want, message)
		}
	}
}
