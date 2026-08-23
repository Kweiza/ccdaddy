package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The assertion .github/workflows/install-smoke.yml makes about every binary it
// installs. It cannot be exercised by publishing a release — that is the thing
// it exists to check — so the binary is replaced by a stand-in that prints a
// canned line, which is the only part of a ccdad this script reads.

// fakeCcdad writes an executable that prints line on stdout and exits with
// code, and returns its path. The line is passed through a file rather than
// through the script body so that a \r or a \n in it survives verbatim.
func fakeCcdad(t *testing.T, line string, code int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "version.txt")
	if err := os.WriteFile(out, []byte(line), 0o644); err != nil {
		t.Fatalf("writing the canned version line: %v", err)
	}
	body := "#!/usr/bin/env bash\ncat " + strconv.Quote(out) + "\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "ccdad")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing the ccdad stand-in: %v", err)
	}
	return path
}

// smokeVersion runs the script and returns everything it said plus its exit
// code.
func smokeVersion(t *testing.T, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is an extensionless shebang script, which only the bash layer resolves")
	}
	script, err := filepath.Abs("smoke-version.sh")
	if err != nil {
		t.Fatalf("resolving smoke-version.sh: %v", err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running smoke-version.sh: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

func TestSmokeVersionAcceptsTheTemplateARealBinaryPrints(t *testing.T) {
	// Spelled the way scripts/build_release_test.go spells it, from the same
	// constants: that test runs a freshly built asset and asserts this exact
	// string, so if Cobra's template ever changes the two fail together and
	// neither can be "fixed" without the other noticing.
	line := "ccdad version " + stampedVersion + " (" + stampedCommit[:12] + ")"

	for _, expected := range []string{stampedVersion, "v" + stampedVersion} {
		t.Run(expected, func(t *testing.T) {
			out, code := smokeVersion(t, expected, fakeCcdad(t, line+"\n", 0))
			if code != 0 {
				t.Fatalf("smoke-version.sh exited %d on %q:\n%s", code, line, out)
			}
			if !strings.Contains(out, "as the release claims") {
				t.Errorf("smoke-version.sh said:\n%s\nwant it to name the version it accepted", out)
			}
		})
	}
}

// Git Bash hands back a Windows binary's output with the carriage return still
// on it, and the pattern is anchored. Without the strip, the windows leg fails
// with a message about Cobra's template on a binary that printed exactly the
// right thing.
func TestSmokeVersionAcceptsWindowsLineEndings(t *testing.T) {
	line := "ccdad version 1.2.3 (c24609320a6a)\r\n"
	if out, code := smokeVersion(t, "v1.2.3", fakeCcdad(t, line, 0)); code != 0 {
		t.Fatalf("smoke-version.sh exited %d on a CRLF line:\n%s", code, out)
	}
}

// A daemon note or a warning ahead of the version would otherwise be matched
// instead of the version.
func TestSmokeVersionReadsOnlyTheFirstLine(t *testing.T) {
	line := "ccdad version 1.2.3 (c24609320a6a)\nsomething else entirely\n"
	if out, code := smokeVersion(t, "1.2.3", fakeCcdad(t, line, 0)); code != 0 {
		t.Fatalf("smoke-version.sh exited %d on a multi-line answer:\n%s", code, out)
	}
}

func TestSmokeVersionRejects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		line     string
		code     int
		want     string
	}{
		{
			// The whole reason this is not a string comparison against the tag.
			name:     "the version is not the release's",
			expected: "v1.2.4",
			line:     "ccdad version 1.2.3 (c24609320a6a)\n",
			want:     "reports version 1.2.3; the release says 1.2.4",
		},
		{
			// §11.5's silent defect: `-X main.version=…` names a symbol that
			// does not exist, the linker says nothing, and every binary in the
			// release answers this.
			name:     "the binary was never stamped",
			expected: "v1.2.3",
			line:     "ccdad version dev (c24609320a6a)\n",
			want:     "reports version dev",
		},
		{
			name:     "a bare version, as if the template were the version",
			expected: "v1.2.3",
			line:     "1.2.3\n",
			want:     "expected Cobra's template",
		},
		{
			// buildinfo.String() drops the suffix when Commit is empty and
			// debug.ReadBuildInfo has nothing either, which is a release built
			// outside scripts/build-release.sh.
			name:     "no commit suffix at all",
			expected: "v1.2.3",
			line:     "ccdad version 1.2.3\n",
			want:     "expected Cobra's template",
		},
		{
			name:     "a commit that is not twelve hex digits",
			expected: "v1.2.3",
			line:     "ccdad version 1.2.3 (HEAD)\n",
			want:     "expected Cobra's template",
		},
		{
			name:     "nothing at all",
			expected: "v1.2.3",
			line:     "",
			want:     "expected Cobra's template",
		},
		{
			// An installed binary that cannot run is the failure this whole
			// workflow exists to catch — §11.5's stripped darwin/arm64 asset is
			// SIGKILLed with no diagnostic, and this is where that surfaces.
			name:     "the binary will not run",
			expected: "v1.2.3",
			line:     "",
			code:     1,
			want:     "--version failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := smokeVersion(t, tc.expected, fakeCcdad(t, tc.line, tc.code))
			if code == 0 {
				t.Fatalf("smoke-version.sh accepted %q:\n%s", tc.line, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("smoke-version.sh said:\n%s\nwant a message containing %q", out, tc.want)
			}
		})
	}
}

func TestSmokeVersionRefusesWhatItCannotCheck(t *testing.T) {
	t.Run("no arguments", func(t *testing.T) {
		out, code := smokeVersion(t)
		if code == 0 {
			t.Fatalf("smoke-version.sh exited 0 with nothing to check:\n%s", out)
		}
		if !strings.Contains(out, "usage:") {
			t.Errorf("smoke-version.sh said:\n%s\nwant a usage line", out)
		}
	})

	t.Run("nothing was installed", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "ccdad")
		out, code := smokeVersion(t, "v1.2.3", missing)
		if code == 0 {
			t.Fatalf("smoke-version.sh exited 0 on a binary that is not there:\n%s", out)
		}
		if !strings.Contains(out, "did not leave one there") {
			t.Errorf("smoke-version.sh said:\n%s\nwant it to blame the installer", out)
		}
	})
}
