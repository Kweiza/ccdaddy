package zone_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/zone"
)

var (
	kst  = time.FixedZone("KST", 9*60*60)
	west = time.FixedZone("XYZ", -7*60*60)
	when = time.Date(2026, 8, 22, 5, 0, 0, 0, time.UTC)
)

// The instant is what must not move. A zone is a rendering, and a fix that
// changed the moment would turn an unreadable document into a wrong one.
func TestInKeepsTheInstantAndChangesOnlyTheZone(t *testing.T) {
	got := zone.In(when, kst)
	if !got.Equal(when) {
		t.Errorf("In moved the instant: %s, want %s", got, when)
	}
	if got.Location() != kst {
		t.Errorf("location = %v, want %v", got.Location(), kst)
	}
	if want := "2026-08-22T14:00:00+09:00"; got.Format(time.RFC3339) != want {
		t.Errorf("rendered %q, want %q", got.Format(time.RFC3339), want)
	}
}

// A zero time.Time must stay zero, because `omitzero` is what keeps an unset
// poll time off the wire and it asks IsZero(). A conversion that produced
// 0001-01-01T09:00:00+09:00 would be non-zero to a human reader and still zero
// to IsZero — or, if it were not, would publish year 1 as a poll time, which
// TestStatusOmitsUnsetTimes exists to prevent.
func TestInLeavesTheZeroTimeOmittable(t *testing.T) {
	var unset time.Time
	if got := zone.In(unset, kst); !got.IsZero() {
		t.Errorf("In(zero) = %s, which omitzero would publish", got)
	}
	doc := struct {
		At time.Time `json:"at,omitzero"`
	}{At: zone.In(unset, kst)}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Errorf("marshalled %s, want the field omitted", body)
	}
}

type inner struct {
	At    time.Time
	Label string
}

type outer struct {
	At     time.Time
	Rows   []inner
	ByName map[string]inner
	Ptr    *inner
	// Pair is an ARRAY rather than a slice. Nothing ccdad serialises uses one
	// today, and the walk handles them anyway because it is meant to be total
	// over what reflect can describe — so the branch is executed here rather
	// than left as a claim.
	Pair     [2]time.Time
	Anything any
	// unexported is here because reflect cannot set it. The walk copies the
	// whole struct before rewriting the fields it can, and this is what proves
	// the copy rather than the rewrite is what carries a private field across.
	unexported string
}

func newOuter() outer {
	return outer{
		At:         when,
		Rows:       []inner{{At: when.In(west), Label: "row"}},
		ByName:     map[string]inner{"a": {At: when.In(west)}},
		Ptr:        &inner{At: when},
		Pair:       [2]time.Time{when.In(west), when},
		Anything:   map[string]any{"nested": []any{when.In(west)}},
		unexported: "kept",
	}
}

// Everything, at every depth, and through every container a payload is built
// out of. A walk that stopped at the first container would leave exactly the
// rows this bug was reported on.
func TestInReachesEveryTimeAtEveryDepth(t *testing.T) {
	got := zone.In(newOuter(), kst)

	for _, tc := range []struct {
		where string
		at    time.Time
	}{
		{"the struct's own field", got.At},
		{"a slice element", got.Rows[0].At},
		{"a map value", got.ByName["a"].At},
		{"through a pointer", got.Ptr.At},
		{"an array element", got.Pair[0]},
		{"the other array element", got.Pair[1]},
		{"inside an interface, inside a slice, inside a map",
			got.Anything.(map[string]any)["nested"].([]any)[0].(time.Time)},
	} {
		if tc.at.Location() != kst {
			t.Errorf("%s is in %v, want %v", tc.where, tc.at.Location(), kst)
		}
		if !tc.at.Equal(when) {
			t.Errorf("%s moved to %s, want %s", tc.where, tc.at, when)
		}
	}
	if got.Rows[0].Label != "row" {
		t.Errorf("a neighbouring value was disturbed: %q", got.Rows[0].Label)
	}
}

// The caller's value is still live. daemon.Status is passed to the writer by
// value and the slice header inside it is not, so normalising in place would
// reach back through a field the writer only meant to read and rewrite the
// scheduler's own state.
func TestInDoesNotTouchTheCallersValue(t *testing.T) {
	src := newOuter()
	before := src.Rows[0].At
	zone.In(src, kst)
	if src.Rows[0].At.Location() != before.Location() {
		t.Errorf("the caller's slice was rewritten: %v", src.Rows[0].At.Location())
	}
	if src.At.Location() != time.UTC {
		t.Errorf("the caller's field was rewritten: %v", src.At.Location())
	}
	if src.Ptr.At.Location() != time.UTC {
		t.Errorf("the value behind the caller's pointer was rewritten: %v", src.Ptr.At.Location())
	}
}

func TestInCarriesUnexportedFieldsAcross(t *testing.T) {
	if got := zone.In(newOuter(), kst); got.unexported != "kept" {
		t.Errorf("unexported = %q, want it copied across", got.unexported)
	}
}

// A payload is mostly not timestamps, and none of it may be disturbed. nil is
// its own case: a nil map and an empty one are different documents, because
// encoding/json writes null for one and {} for the other.
func TestInLeavesEverythingElseAlone(t *testing.T) {
	payload := map[string]any{
		"schemaVersion": 1,
		"name":          "ccdad",
		"ok":            true,
		"pct":           41.5,
		"none":          nil,
		"nilMap":        map[string]any(nil),
		"nilSlice":      []string(nil),
		"rows":          []map[string]any{{"uuid": "u"}},
	}
	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(zone.In(payload, kst))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the document changed:\n got %s\nwant %s", got, want)
	}
	if !reflect.DeepEqual(zone.In(payload, kst)["nilMap"], map[string]any(nil)) {
		t.Error("a nil map came back as an empty one, which is a different document")
	}
}

// A nil location is the machine's, and not UTC. It differs from
// view.Timestamp's fallback on purpose: everything reaching this package is
// being written to or read from a file on the machine the reader is at, while
// view is handed moments by arithmetic that must not touch the environment.
func TestInWithNoLocationUsesTheMachine(t *testing.T) {
	if got := zone.In(when, nil); got.Location() != time.Local {
		t.Errorf("In(nil) rendered in %v, want the machine's zone %v", got.Location(), time.Local)
	}
}

// A nil `any` is what a payload key set to nothing decodes as, and reflect has
// no value to walk for it.
func TestInSurvivesANilValue(t *testing.T) {
	var nothing any
	if got := zone.In(nothing, kst); got != nil {
		t.Errorf("In(nil any) = %v, want nil", got)
	}
}
