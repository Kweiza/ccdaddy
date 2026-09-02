//go:build windows

package atomicfile

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// readSharedDelete reads path the way the real concurrent reader of the
// credential store does. Claude Code reads it through libuv, which opens with
// FILE_SHARE_DELETE; os.ReadFile does not (syscall.Open passes only
// FILE_SHARE_READ|FILE_SHARE_WRITE), and any handle lacking it makes
// MoveFileEx(..., MOVEFILE_REPLACE_EXISTING) -- which is os.Rename -- fail
// with ERROR_SHARING_VIOLATION. A tight os.ReadFile loop would therefore
// block the very replace the test is trying to observe.
func readSharedDelete(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}
