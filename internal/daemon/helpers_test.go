package daemon

import "testing"

// isolate points the store at a directory of this test's own, the way every
// other package in this tree does. CCDAD_HOME is read by ccpath.StoreHome, so
// unlike HOME it is honoured on every platform — the HOME trap has already
// escaped a suite into a real profile once, on Windows.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCDAD_HOME", dir)
	return dir
}
