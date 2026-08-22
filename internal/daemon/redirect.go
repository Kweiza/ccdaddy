package daemon

import "os"

// redirectStderr points file descriptor 2 at f. It is a package-level var so a
// test can neutralise it: the real one redirects the TEST BINARY's stderr, and
// a test that forgets loses `go test`'s own output into a temp directory.
//
// Production code never assigns to it.
var redirectStderr = platformRedirectStderr

func stderrFD() uintptr { return os.Stderr.Fd() }
