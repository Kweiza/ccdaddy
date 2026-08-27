// Package zone renders every timestamp inside a value in one time zone.
//
// It exists because encoding/json writes a time.Time as RFC 3339 INCLUDING the
// offset it happens to carry, and nothing in Go carries that offset on purpose.
// A moment computed as now.Add(interval) is in the machine's zone; the same
// moment parsed off the wire, where Claude's usage endpoint ends every
// timestamp in Z, is in UTC. Put both in one document and the document has two
// zones — same instants, and a reader comparing the rows against one wall clock
// reads half of them as hours out. That is not a hypothetical: a live ccdad
// store carried five poll times at +09:00 beside one at Z, and the Z row, read
// as though it were local, looked nine hours overdue while in fact being four
// minutes in the future.
//
// The rule this package makes executable is that the ZONE BELONGS TO THE
// DOCUMENT AND NOT TO THE FIELD. It is applied on the way out, at the one place
// each document is serialised, rather than at every writer that computes a
// moment: a writer's job is to choose an instant, and an instant has no zone —
// only its rendering does.
//
// This is the machine-side counterpart of view.Timestamp, which is the one
// absolute-time rendering for a person. Neither reads the environment: the
// location arrives as a parameter, so the caller nearest the reader chooses it.
package zone

import (
	"reflect"
	"time"
)

// timeType is compared against rather than switched on, because time.Time is
// the one struct here that must NOT be descended into: its fields are
// unexported, its zone lives in one of them, and In is the only supported way
// to change it.
var timeType = reflect.TypeOf(time.Time{})

// In returns v with every time.Time reachable inside it rendered in loc, and
// everything else unchanged. The instants are untouched — only the zone they
// will be written in changes.
//
// v is not modified. Maps, slices and structs on the way to a timestamp are
// rebuilt rather than written through, because the caller's value is usually
// still live: daemon.Status is passed by value but its Accounts slice is shared
// with the engine's own, and normalising in place would reach back into the
// scheduler's state through a field the writer only meant to read.
//
// A nil location means the machine's zone. That differs from view.Timestamp,
// which falls back to UTC, and the difference is deliberate: view is handed
// moments by arithmetic that must not touch the environment, while everything
// that reaches this function is already being written to or read from a file on
// the machine the reader is sitting at.
//
// The input must be a tree. Every document ccdad serialises is built fresh out
// of maps, slices and plain structs, and encoding/json would refuse a cycle a
// step later anyway.
func In[T any](v T, loc *time.Location) T {
	if loc == nil {
		loc = time.Local
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return v
	}
	out, ok := walk(rv, loc).Interface().(T)
	if !ok {
		// Unreachable for any v: walk preserves the dynamic type it was given.
		// Returning the original rather than panicking keeps a serialiser that
		// is only trying to be tidy from taking a document down with it.
		return v
	}
	return out
}

func walk(v reflect.Value, loc *time.Location) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		// Rebuilt through a value of the INTERFACE's type rather than returned
		// as the concrete one, so a caller assigning this into a map or field
		// of that interface type gets something assignable.
		out := reflect.New(v.Type()).Elem()
		out.Set(walk(v.Elem(), loc))
		return out

	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(walk(v.Elem(), loc))
		return out

	case reflect.Struct:
		if v.Type() == timeType {
			return reflect.ValueOf(v.Interface().(time.Time).In(loc))
		}
		out := reflect.New(v.Type()).Elem()
		// The whole struct is copied first and the exported fields are then
		// rewritten over the top. The bulk copy is what carries the UNEXPORTED
		// fields across: reflect cannot set one, and rebuilding field by field
		// would silently drop every private field a payload struct has.
		out.Set(v)
		for i := range v.NumField() {
			f := out.Field(i)
			if !f.CanSet() {
				continue
			}
			f.Set(walk(v.Field(i), loc))
		}
		return out

	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(), walk(iter.Value(), loc))
		}
		return out

	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(walk(v.Index(i), loc))
		}
		return out

	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(walk(v.Index(i), loc))
		}
		return out

	default:
		return v
	}
}
