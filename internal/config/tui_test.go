package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/theme"
)

// The two keys travel as WORDS and the loader has to read them as such. A
// display key that parsed into its default whatever the file said would be a
// setting that silently does nothing, which is the failure the closed namespace
// exists to prevent, reached from the other side.
func TestTheDisplayKeysAreReadFromTheFile(t *testing.T) {
	cfg, err := Parse([]byte("[tui]\ntheme = \"light\"\nglyphs = \"ascii\"\n"))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if cfg.TUITheme != "light" {
		t.Errorf("TUITheme = %q, want light", cfg.TUITheme)
	}
	if cfg.TUIGlyphs != "ascii" {
		t.Errorf("TUIGlyphs = %q, want ascii", cfg.TUIGlyphs)
	}
}

// Half a table is the ordinary document: a user sets the theme and never thinks
// about glyphs. The key they did not write keeps its own default rather than
// the empty string the decoder would otherwise hand it, and an empty string is
// a name no picker matches.
func TestATuiTableWithOneKeyLeavesTheOtherAtItsDefault(t *testing.T) {
	cfg, err := Parse([]byte("[tui]\nglyphs = \"unicode\"\n"))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if cfg.TUITheme != defaultTUITheme {
		t.Errorf("TUITheme = %q, want the default %q", cfg.TUITheme, defaultTUITheme)
	}
	if cfg.TUIGlyphs != "unicode" {
		t.Errorf("TUIGlyphs = %q, want unicode", cfg.TUIGlyphs)
	}
}

// A name outside the set is refused by the LOADER, not only by `config set`, so
// a hand edit cannot leave the engine running on a word nothing can paint. The
// zero config is the other half of the promise Parse makes everywhere else:
// half a config file is not a configuration.
func TestANameOutsideItsSetIsRefusedByTheLoader(t *testing.T) {
	for _, body := range []string{
		"[tui]\ntheme = \"puce\"\n",
		"[tui]\nglyphs = \"emoji\"\n",
	} {
		cfg, err := Parse([]byte(body))
		if err == nil {
			t.Fatalf("Parse(%q) = %+v, want a refusal", body, cfg)
		}
		if !cfg.Equal(Config{}) {
			t.Errorf("Parse(%q) returned the partly applied %+v; a rejected document yields the zero Config", body, cfg)
		}
		if !strings.Contains(err.Error(), FileName) {
			t.Errorf("the refusal %q does not name the file the bad value is in", err)
		}
	}
}

// The refusal has to NAME the spellings that would have worked: the message
// telling a user their word is wrong is the only place they find the right one.
// This is the shape parseStrategy already uses, for the same reason.
func TestARefusedNameListsTheOnesThatWouldHaveWorked(t *testing.T) {
	err := validTheme("puce")
	if err == nil {
		t.Fatal("validTheme(puce) = nil, want a refusal")
	}
	for _, n := range theme.Names() {
		if !strings.Contains(err.Error(), string(n)) {
			t.Errorf("the theme refusal %q does not name %q", err, n)
		}
	}
	err = validGlyphs("emoji")
	if err == nil {
		t.Fatal("validGlyphs(emoji) = nil, want a refusal")
	}
	for _, n := range glyphNames() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the glyph refusal %q does not name %q", err, n)
		}
	}
}

// `ccdad config set` refuses the same words the loader refuses, and it is a
// separate assertion because the two run through separate code. A set that
// succeeded on a word the loader then rejected would write a file that switches
// the WHOLE configuration off, not just the key that was typed.
func TestSettingAnUnpaintableNameIsRefusedAndIsNotAnUnknownKey(t *testing.T) {
	d := newDocument()
	for key, value := range map[string]string{keyTUITheme: "puce", keyTUIGlyphs: "emoji"} {
		err := d.Set(key, value)
		if err == nil {
			t.Fatalf("Set(%s, %s) = nil, want a refusal", key, value)
		}
		if errors.Is(err, ErrUnknownKey) {
			t.Errorf("Set(%s, %s) = %v; the key is real and the VALUE is not, and those are two different sentences with the same exit code",
				key, value, err)
		}
	}
}

// The whole point of a [tui] table: what `config set` writes comes back through
// the file, and comes back through the EFFECTIVE config too. Without the table
// in fileShape the value would round-trip as text and the loader would still
// report the default, so `config list` would print `auto` beside the word
// `file`.
func TestTheDisplayKeysRoundTripThroughTheFile(t *testing.T) {
	d := newDocument()
	if err := d.Set(keyTUITheme, "ansi"); err != nil {
		t.Fatalf("Set(%s) = %v", keyTUITheme, err)
	}
	if err := d.Set(keyTUIGlyphs, "ascii"); err != nil {
		t.Fatalf("Set(%s) = %v", keyTUIGlyphs, err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "[tui]") {
		t.Errorf("the encoded document has no [tui] table:\n%s", encoded)
	}
	reread, err := ParseDocument(encoded)
	if err != nil {
		t.Fatalf("ParseDocument() = %v", err)
	}
	if got, set, err := reread.Value(keyTUITheme); err != nil || !set || got != "ansi" {
		t.Errorf("Value(%s) = %q, set %v, err %v; want ansi read back from the file", keyTUITheme, got, set, err)
	}
	cfg, err := reread.Config()
	if err != nil {
		t.Fatalf("Config() = %v", err)
	}
	if cfg.TUITheme != "ansi" || cfg.TUIGlyphs != "ascii" {
		t.Errorf("Config() = %q/%q, want ansi/ascii; the loader is not reading the table it round-trips", cfg.TUITheme, cfg.TUIGlyphs)
	}
	// The table goes when its last key does, so unsetting both does not leave a
	// bare header for the next reader to wonder about. [credit] already answers
	// this way and a second table answering differently would be a surprise.
	if _, err := reread.Unset(keyTUITheme); err != nil {
		t.Fatal(err)
	}
	if _, err := reread.Unset(keyTUIGlyphs); err != nil {
		t.Fatal(err)
	}
	encoded, err = reread.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "tui") {
		t.Errorf("unsetting both keys left the table behind:\n%s", encoded)
	}
}

// A table this release KNOWS is not an unknown key, and a key inside it that it
// does not know is reported one level down rather than as the whole table --
// the answer [credit] already gives. Without the section arm the note would
// tell a user their theme is being ignored.
func TestAnUnknownKeyInsideTheTuiTableIsReportedByItsDottedName(t *testing.T) {
	d, err := ParseDocument([]byte("[tui]\ntheme = \"dark\"\nfuture = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.UnknownKeys(); !slices.Equal(got, []string{"tui.future"}) {
		t.Errorf("UnknownKeys() = %v, want [tui.future]; a known table is reported key by key", got)
	}
}

// The three glyph names are spelled in this package and matched in the package
// that owns the picker, which this one cannot import. Pinning the literals is
// what makes a rename on this side a deliberate edit rather than a silent one
// that leaves `config set` accepting a word the picker never matches.
func TestTheGlyphSetNamesAreTheThreeLiterals(t *testing.T) {
	if want := []string{"auto", "unicode", "ascii"}; !slices.Equal(glyphNames(), want) {
		t.Errorf("glyphNames() = %v, want %v", glyphNames(), want)
	}
}

// A default the validator refuses would make an untouched config unloadable the
// first time anything validated it, which is the one way a default can be
// wrong that no other test here can see.
func TestTheDisplayDefaultsAreNamesTheValidatorsAccept(t *testing.T) {
	d := Defaults()
	if err := validTheme(d.TUITheme); err != nil {
		t.Errorf("validTheme(%q) = %v", d.TUITheme, err)
	}
	if err := validGlyphs(d.TUIGlyphs); err != nil {
		t.Errorf("validGlyphs(%q) = %v", d.TUIGlyphs, err)
	}
	if d.TUITheme != string(theme.Auto) {
		t.Errorf("the default theme is %q, want %q -- the word that says measure the terminal rather than guess at it",
			d.TUITheme, theme.Auto)
	}
	if d.TUIGlyphs != glyphsAuto {
		t.Errorf("the default glyph set is %q, want %q", d.TUIGlyphs, glyphsAuto)
	}
}
