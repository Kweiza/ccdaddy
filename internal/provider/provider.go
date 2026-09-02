// Package provider names the two account providers ccdad manages.
//
// It is a leaf on purpose: a store row, a switcher option, a proxy request and
// an export document all have to say which provider an account belongs to, and
// a type any of them can import without dragging a credential package behind
// it is what makes the never-cross checks cheap enough to put everywhere.
//
// The two strings are the WIRE form. They are written into accounts.toml, into
// every --json payload and into export documents, so renaming one would make a
// stored account unreadable by the build that wrote it.
package provider

import "fmt"

// ID is a provider's name.
//
// It is a named string rather than a uint8 with a separate name field, and the
// difference decides the store's shape: go-toml round-trips a named string
// type directly, so one field carries both the value and its serialized form.
// identity.Kind needs the KindName pair only because Kind is a uint8, which
// TOML cannot spell.
type ID string

const (
	// Claude is a Claude Code account: file-based, switched by rewriting
	// Claude Code's own credentials file.
	Claude ID = "claude"
	// Codex is a Codex account: served through ccdad's local reverse proxy,
	// which holds the token and rewrites the bearer per request.
	Codex ID = "codex"
)

// Parse accepts exactly the two wire names.
//
// The empty string is an ERROR rather than a default, and that is the whole
// value of this function. The zero value of a provider field is "", so a Parse
// that answered Claude for it would turn a row whose provider went missing
// into a working Claude account -- and the account whose provider went missing
// is exactly the one nothing may hand to the Claude switch path.
func Parse(s string) (ID, error) {
	switch ID(s) {
	case Claude:
		return Claude, nil
	case Codex:
		return Codex, nil
	}
	return "", fmt.Errorf("%q is not a provider ccdad knows; it accepts claude and codex", s)
}

// String is the raw value, which is also the wire form.
func (p ID) String() string { return string(p) }

// Valid reports whether p is one of the two providers. It is the predicate a
// caller uses when it holds an ID already and has nothing to parse.
func (p ID) Valid() bool { return p == Claude || p == Codex }
