package mcpsrv

import (
	"slices"
	"strings"
	"testing"
)

// The mirror of the class gate's own test, and the earliest commit it can be
// written in: it needs every one of the four groups filled.
//
// TestEveryRegisteredToolHasAClassVerdict says every REGISTERED tool has a
// class. This one says every CLASSIFIED tool is registered -- so a name left in
// the map after its tool was renamed is a lie the map tells about what this
// server exposes. Without it, that lie is discovered by a caller getting
// "ccdad has no gate verdict for the tool ..." for a tool it can see, or worse,
// by nobody: an entry for a tool that no longer exists costs nothing at runtime
// and quietly makes the classification a document about the past.
func TestEveryClassifiedToolIsActuallyRegistered(t *testing.T) {
	offered := offeredTools(t)

	var missing []string
	for name := range toolClass {
		if _, ok := offered[name]; !ok {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)

	if len(missing) > 0 {
		t.Errorf("classified in toolClass and offered by no tool: %s\n"+
			"Either the tool was renamed and the map still names the old one, or a group's "+
			"add function stopped registering it. The map is the boundary this server is "+
			"reviewed against, so it may not describe tools that are not there.",
			strings.Join(missing, ", "))
	}
}

// The count, asserted from both sides, so that a whole group quietly failing to
// register is not read as a smaller surface.
//
// The two tests above and beside this one are each one-directional and both
// pass on a server offering only the reads, as long as the map named only
// those: they check that the two sets AGREE, not how big they are. The count is
// the number the four classes were argued for as a whole, and a change to it is
// a change to what this server may do.
//
// It was fifteen and is sixteen: `runway` joined the reads, deliberately, and
// this line is where that was declared rather than absorbed.
func TestTheServerOffersExactlyTheSixteenToolsTheClassMapNames(t *testing.T) {
	const want = 16

	if len(toolClass) != want {
		t.Errorf("toolClass classifies %d tools, want %d; the classification IS the boundary, "+
			"and changing its size is a change to what this server may do", len(toolClass), want)
	}
	if got := len(offeredTools(t)); got != want {
		t.Errorf("tools/list offers %d tools, want %d", got, want)
	}
}
