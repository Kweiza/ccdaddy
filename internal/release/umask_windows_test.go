//go:build windows

package release

// Windows has no umask: a file's access is inherited from the parent
// directory's ACL. The one test that calls this skips before it does.
func syscallUmask(int) int { return 0 }
