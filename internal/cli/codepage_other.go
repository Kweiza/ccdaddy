//go:build !windows

package cli

// consoleUTF8 reports whether this process's console can carry UTF-8, and here
// the answer is yes without asking anybody.
//
// There is no question to ask. A terminal on these platforms decodes the byte
// stream by the locale its emulator was configured with, and no process can
// interrogate that the way GetConsoleOutputCP interrogates a Windows console:
// the decoder lives in the emulator, not in a table the kernel keeps for the
// process attached to it. The honest answer to an unanswerable question would
// be "unknown", and "unknown" here would mean shipping ASCII box-drawing to
// every terminal emulator written in the last twenty years to hedge against a
// `LANG=C` xterm -- for which `tui.glyphs=ascii` is already a name the user
// can type, and typing it is a smaller cost than the hedge.
//
// It is a var rather than a func so the seam has the identical shape on both
// platforms. The policy test stubs THIS one, and a seam that existed only on
// Windows would pin the policy on the one leg of CI that runs least and would
// leave the other two proving nothing.
var consoleUTF8 = func() bool { return true }
