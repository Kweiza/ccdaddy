//go:build !windows

package cli

import "testing"

// Off Windows the answer is an unconditional yes, and it is asserted rather
// than left obvious because of what a false would do: it would put the ASCII
// glyph set -- `#` gauges and `+---+` frames -- on every macOS and Linux
// terminal there is, on the machines where the Unicode set was never in doubt.
// One line of test is cheap insurance against a platform file that got a
// stray `!`.
func TestConsoleUTF8IsTrueWhereThereIsNoCodePageToRead(t *testing.T) {
	if !consoleUTF8() {
		t.Fatal("consoleUTF8() = false on a platform with no console output code page: the Unicode glyph " +
			"set would never be chosen on macOS or Linux")
	}
}
