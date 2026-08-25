// Package tui is ccdad's terminal dashboard.
package tui

// wordmark, tagline and figures are the page's decorative chrome: hand-written
// Go string constants, one entry per row, and nothing else. There is no
// generator — no figlet dependency, no font file — because a generator would
// put a build step in front of a decoration.
//
// Every one of them is 7-bit ASCII, and it stays that way even now that the
// page around it draws box-drawing and block characters on purpose. This is
// ART, measured in columns by a transcription somebody did by hand, and the
// height ladder's row budget and the frame's width are both arithmetic over the
// sizes below. A block character inside one of these rows would measure one
// column on most machines and two on a machine whose width engine was started
// in east-asian mode, so the drawing would change shape with an environment
// variable — and the glyph set that fixes that for the frame and the gauge
// cannot fix it here, because these rows are not a vocabulary with two
// spellings. They are one drawing.
var wordmark = []string{
	`  ___ ___ ___          _    _        `,
	` / __/ __|   \ __ _ __| |__| |_  _   `,
	`| (_| (__| |) / _' / _' / _' | || |  `,
	` \___\___|___/\__,_\__,_\__,_|\_, |  `,
	`                              |__/   `,
}

// tagline is English, and this is the one place the mockup is overruled on
// taste rather than on fact: repository artifacts, CLI strings included, are
// English, and a Korean string constant in the binary breaks that rule.
var tagline = []string{
	`Quota down again? You were 'Yap'-ping. `,
	`                      -- 'Daddy' Daemon`,
}

// figures measures 48 columns wide, not the 46 the plan's own prose states.
// The block's left anchor is its leftmost real content across all six rows,
// not column 1 of the fixture render -- three of the six rows (the brows,
// mouth and chin-box rows) start their content at that anchor with no
// leading space at all, so nothing here is shifted, only right-padded where
// a row's content falls short of the widest one. The mouth row is the widest:
// it is the one row that fills the full 48 columns on its own, with no
// padding at either end.
var figures = []string{
	`  ____     ____     ____             ________   `,
	` / oo \   / oo \   / oo \           | o    o |  `,
	`|  __  | |  __  | |  __  |         _|__ >< __|_ `,
	`| |  | | | |  | | | |  | |        |   ~~~~~~~  |`,
	`|_|  |_| |_|  |_| |_|  |_|        |___________| `,
	`  ||||     ||||     ||||             ||     ||  `,
}

// titleLine is the one-row replacement the height ladder swaps the wordmark
// for once there is no room left for five rows of it.
func titleLine(version string) string {
	return "ccdad " + version
}
