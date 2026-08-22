package cli

// mustPath unwraps a ccdad path resolver in a test.
//
// Every resolver returns an error only when the home directory cannot be
// resolved, and each of these tests either sets the environment that decides it
// or runs under the shared isolate helper that does. An error here is therefore
// a broken fixture rather than a case under test, and failing loudly on it beats
// carrying an empty path into an assertion that would then compare "" to "".
func mustPath(path string, err error) string {
	if err != nil {
		panic("test fixture: a ccdad path could not be resolved: " + err.Error())
	}
	return path
}
