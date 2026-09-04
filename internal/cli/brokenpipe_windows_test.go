//go:build windows

package cli

import "golang.org/x/sys/windows"

// errBrokenPipeForTest is what a closed reader answers with on this platform.
// ERROR_NO_DATA is the one `ccdad status --json | head -1` produces; isBrokenPipe
// accepts ERROR_BROKEN_PIPE as well.
var errBrokenPipeForTest error = windows.ERROR_NO_DATA
