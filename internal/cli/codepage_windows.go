//go:build windows

package cli

import "golang.org/x/sys/windows"

// cpUTF8 is the Windows code page identifier for UTF-8. golang.org/x/sys/
// windows exports no constant for it, so it is named once here rather than
// appearing as a bare 65001 inside a comparison, where it reads as a magic
// number in a function whose entire body is that comparison.
const cpUTF8 = 65001

// consoleUTF8 reports whether this process's console can carry UTF-8, by
// reading the output code page and asking whether it is UTF-8.
//
// IT READS AND NEVER WRITES, and that is the design rather than caution about
// it. GetConsoleOutputCP takes no handle. setConsoleVT is genuinely
// handle-scoped -- it widens the mode of the one handle it is given -- which
// is why "a redirected stdout has no console mode to widen" is true there and
// false here: `ccdad status > out.txt` launched from a console window is still
// ATTACHED to that console, and the output code page is a property of the
// console, shared with every process attached to it and outliving all of them.
// Writing it would reinterpret the bytes of programs this one never heard of,
// for the sake of a run that puts no glyph on the screen at all.
//
// setConsoleVT's "the mode is deliberately not restored on exit" does NOT
// transfer, and the difference is precisely what makes that safe and this
// unsafe. ENABLE_VIRTUAL_TERMINAL_PROCESSING is additive and reaches one class
// of byte -- escape sequences, which a console without it was printing as
// visible garbage anyway, so leaving it on is a gift to the next program in
// that window. The output code page is not additive: it IS the decoder, for
// every non-ASCII byte, forever. A Korean user on chcp 949 who runs `ccdad
// status` once would get mojibake out of every CP949-emitting program in that
// window for the rest of its life, including whatever `ccdad run` execs. One
// leaves the window better than it found it; the other breaks it and walks
// away.
//
// A successful read proves CAPABILITY, NOT OUTCOME. 65001 says the console
// will decode the bytes as UTF-8. It says nothing about whether the font can
// draw what they decode to, and a legacy conhost with a raster font paints
// boxes at 65001 exactly as it did at 437. That is not a gap to be closed --
// no read of any kind answers the font question -- and it is survivable
// because of what hangs on the answer: a fallback to ASCII glyphs, which
// costs a squarer frame when it guesses wrong and costs nothing else.
//
// A FAILED READ IS FALSE. GetConsoleOutputCP fails when this process has no
// console at all, and the honest reading of a failed read is that nothing was
// proven. ASCII glyphs are legible on every terminal there is; mojibake is
// legible on none; so the unproven case takes the fallback. The cost is real
// and is named rather than hidden: output redirected into a file has no code
// page and would have carried Unicode perfectly well. It is the smaller half
// of the trade, and `tui.glyphs=unicode` overrides it by name.
//
// It is a var for the reason stdoutIsTTY and consoleVT are: a console sitting
// on a particular code page is not something a test on another platform can
// arrange, and the decision hanging on it -- which invocations consult it at
// all -- is exactly the class that has to be exercised.
var consoleUTF8 = func() bool {
	cp, err := windows.GetConsoleOutputCP()
	return err == nil && cp == cpUTF8
}
