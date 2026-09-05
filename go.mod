module github.com/Kweiza/ccdaddy

go 1.26.4

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 v2.0.6
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/gofrs/flock v0.13.1
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.45.0
	golang.org/x/text v0.41.0
)

require (
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

// ultraviolet is the whole v2 rendering core and it has no tags and no
// releases: bubbletea depends on it by pseudo-version, so MVS resolves it
// silently and `go get -u ./...` can swap the renderer for an unreviewed
// commit. That is not a build break -- it is visual corruption on somebody
// else's terminal, which nothing in this repository can observe. Pinning
// bubbletea does not pin this; only naming it here does. Move it only in a
// commit that also moves bubbletea.
require github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
