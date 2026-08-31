package cli

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// EVERY Outcome MUST HAVE AN ARM, and the compiler will not say so.
//
// `case switcher.Switched:` used to carry a comment claiming "this switch has
// no default, so an Outcome added later would break the build here". It has a
// default. An Outcome added later compiles, falls into it, and exits 1 telling
// the operator the switch "reported an outcome this ccdad does not know" — a
// correct, deliberate stand-down reported to cron as a bug in the tool. That is
// what Unattributed did for as long as it existed. This is the check the
// comment believed the compiler was doing.
func TestEverySwitchOutcomeHasAnArmInAuto(t *testing.T) {
	declared := outcomeConstants(t)
	if len(declared) < 5 {
		t.Fatalf("only found %v in switcher.Outcome; the parse is wrong, not the code", declared)
	}
	auto := readSource(t, "auto.go")
	var missing []string
	for _, name := range declared {
		// NotSwitched is the zero value and means "Execute returned an error",
		// which auto reports before it ever reaches this switch. The default
		// arm IS the right answer for it, and giving it one of its own would
		// claim a stand-down for a call that failed.
		if name == "NotSwitched" {
			continue
		}
		if !strings.Contains(auto, "case switcher."+name+":") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("switcher.Outcome values with no arm in auto.go's outcome switch: %s\n"+
			"each would reach the default arm and exit 1 as \"an outcome this ccdad does not know\"",
			strings.Join(missing, ", "))
	}
}

// outcomeConstants reads the names declared in switcher.Outcome's const block.
func outcomeConstants(t *testing.T) []string {
	t.Helper()
	src := readSourceAt(t, "../switcher/switcher.go")
	start := strings.Index(src, "const (\n\t// Switched")
	if start < 0 {
		// Fall back to the first const block after the Outcome type.
		typ := strings.Index(src, "type Outcome ")
		if typ < 0 {
			t.Fatal("switcher.Outcome's declaration was not found")
		}
		start = strings.Index(src[typ:], "const (")
		if start < 0 {
			t.Fatal("switcher.Outcome's const block was not found")
		}
		start += typ
	}
	end := strings.Index(src[start:], "\n)")
	if end < 0 {
		t.Fatal("switcher.Outcome's const block is unterminated")
	}
	block := src[start : start+end]
	name := regexp.MustCompile(`(?m)^\t([A-Z][A-Za-z0-9]*)(?: Outcome = iota)?$`)
	var out []string
	for _, m := range name.FindAllStringSubmatch(block, -1) {
		out = append(out, m[1])
	}
	return out
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	return readSourceAt(t, name)
}

func readSourceAt(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
