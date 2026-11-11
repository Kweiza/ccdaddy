package scripts

import (
	"os/exec"
	"path"
	"strings"
	"testing"
)

// A test-support package -- one named for the package it supports plus
// "test", the way net/http/httptest supports net/http -- is imported by
// _test.go files only, so nothing in it ships. internal/cclink/cclinktest is
// the reason this exists: it holds a fake keychain that, imported by production
// code, would let the binary answer for the macOS Keychain from memory. It used
// to live in internal/cclink itself and was linked into every build.
//
// The question is asked of the linker's own input, `go list -deps` of the
// command, rather than of import lines read by hand: a production package
// reaching the fake through another package is the same defect, and only the
// dependency graph sees it.
func TestNoTestSupportPackageIsLinkedIntoTheBinary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "../cmd/ccdad").Output()
	if err != nil {
		t.Fatalf("go list -deps ../cmd/ccdad: %v", err)
	}
	const module = "github.com/Kweiza/ccdaddy/"
	for _, pkg := range strings.Fields(string(out)) {
		if !strings.HasPrefix(pkg, module) {
			continue
		}
		dir, name := path.Split(pkg)
		if parent := path.Base(dir); name == parent+"test" {
			t.Errorf("%s is a test-support package and is linked into the ccdad binary", pkg)
		}
	}
}
