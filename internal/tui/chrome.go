// Package tui is ccdad's terminal dashboard.
package tui

// wordmark, tagline and figures are the page's decorative chrome: hand-written
// Go string constants, one entry per row, and nothing else. There is no
// generator — no figlet dependency, no font file — because a generator would
// put a build step in front of a decoration.
//
// Every one of them is 7-bit ASCII, and that is now what they are FOR. Since
// the pixel chrome landed these three blocks are the fallback the page draws
// when Glyphs.Art is false -- a console that cannot carry UTF-8, or a process
// whose width engine is in its east-asian mode, where a block character
// measures two columns and a drawing changes shape with an environment
// variable. They are ART, measured in columns by a transcription somebody did
// by hand, and they are not a vocabulary with two spellings: they are one
// drawing, which is why the fallback is these rows rather than a degraded
// version of the other ones.
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

// figures is the hand-approved 7-bit transcription of the composed pixel
// family. The one-column seams preserve the result of the babies' overlap;
// Claude and Codex share the README creature's block body, and Codex gets only
// a small >_ face cue rather than a separate robot shell. The babies occupy
// four rows while Daddy occupies six, preserving the adult/child scale even
// when block art is unavailable.
var figures = []string{
	`                                     ________   `,
	`        .____.        .______.     __/______\__ `,
	`._____. | >_ | .____. |  >_  |    /| _      _ |\`,
	`| | | | [|__|] | || | [|____|]    | |  _~~_  | |`,
	`[|___|]  | ||  [|__|]  | | | |    |_|________|_|`,
	` | | |          | ||               || || || ||  `,
}

// titleLine is the one-row replacement the height ladder swaps the wordmark
// for once there is no room left for five rows of it.
func titleLine(version string) string {
	return "ccdad " + version
}
