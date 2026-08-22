//go:build !unix && !windows

package daemon

import (
	"context"
	"errors"
)

// This file keeps `go build ./...` honest on every GOOS rather than only on the
// six ccdad releases for — wasip1 in particular compiles the rest of this
// repository today, and it has neither syscall.Kill nor a Win32 event.
//
// Refusing is the point. Returning nil would make `ccdad daemon stop` announce
// a stop that never happened and then wait out its whole timeout on a singleton
// nobody was going to release.
func requestShutdown(int) error { return errors.ErrUnsupported }

func forceShutdown(shutdownTarget) error { return errors.ErrUnsupported }

func watchShutdownRequest(context.Context, func(), *Logger) {}
