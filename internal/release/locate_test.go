package release

import (
	"runtime"
	"testing"
)

// scripts/build-release.sh is the authority for these names: bare binaries, no
// archives, .exe on Windows only, over exactly six targets. A divergence here
// downloads the wrong file or nothing at all.
func TestAssetNameCoversTheSixReleaseTargets(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "ccdad-linux-amd64"},
		{"linux", "arm64", "ccdad-linux-arm64"},
		{"darwin", "amd64", "ccdad-darwin-amd64"},
		{"darwin", "arm64", "ccdad-darwin-arm64"},
		{"windows", "amd64", "ccdad-windows-amd64.exe"},
		{"windows", "arm64", "ccdad-windows-arm64.exe"},
	} {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			if got := assetName(c.goos, c.goarch); got != c.want {
				t.Errorf("assetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
			}
		})
	}
	if got, want := Asset(), assetName(runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("Asset() = %q, want %q — Asset must name the build it is compiled into", got, want)
	}
}

// The variable both installers already read, and which nothing in Go read
// before this package. One seam, because the command and the daemon must be
// pointable at one test origin rather than two.
func TestBaseURLHonoursTheEnvironment(t *testing.T) {
	t.Setenv("CCDAD_BASE_URL", "")
	if got, want := BaseURL(), "https://github.com/Kweiza/ccdaddy/releases"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	t.Setenv("CCDAD_BASE_URL", "http://127.0.0.1:1234/rel")
	if got, want := BaseURL(), "http://127.0.0.1:1234/rel"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	t.Setenv("CCDAD_BASE_URL", "http://127.0.0.1:1234/rel/")
	if got, want := BaseURL(), "http://127.0.0.1:1234/rel"; got != want {
		t.Errorf("BaseURL() = %q, want %q — a trailing slash must not survive into a joined path", got, want)
	}
}

func TestDownloadBase(t *testing.T) {
	for _, c := range []struct{ base, tag, want string }{
		{"https://github.com/Kweiza/ccdaddy/releases", "v0.7.0", "https://github.com/Kweiza/ccdaddy/releases/download/v0.7.0"},
		{"http://127.0.0.1:9/rel/", "v0.7.0", "http://127.0.0.1:9/rel/download/v0.7.0"},
	} {
		if got := DownloadBase(c.base, c.tag); got != c.want {
			t.Errorf("DownloadBase(%q, %q) = %q, want %q", c.base, c.tag, got, c.want)
		}
	}
}
