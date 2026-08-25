package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/release"
	"github.com/Kweiza/ccdaddy/internal/relsign"
)

// ------------------------------------------------------------- the fixtures
//
// The fake release is a COPY of the one in scripts/install_sh_test.go, and the
// copy is deliberate rather than lazy: that fixture lives in package scripts'
// TEST files, which no other package can import. Two copies is the price of
// Go's test-file visibility rule, and the alternative — a non-test package
// existing only so a test can import it — is worse than the duplication.

// signFunc signs content for a tag with the key releaseKeys was pointed at.
type signFunc func(content []byte, tag string) []byte

type fakeRelease struct {
	mu       sync.Mutex
	requests []string

	// tag is what /latest redirects to. Empty answers 200 with no redirect at
	// all, which is one of the ways discovery produces no usable tag.
	tag string
	// location overrides the Location header outright, and it exists because tag
	// alone cannot express every shape an origin can send: a redirect whose path
	// has no /tag/ segment at all is one of them. Empty means "derive it from
	// tag", which is what every test but that one wants.
	location string
	// files, by base name. A name that is not here answers 404.
	files map[string][]byte
	// status overrides the answer for one base name, so "the signature is not
	// published" and "the origin is broken" can be told apart.
	status map[string]int
	// after, when a name is armed, is the body every request for it AFTER the
	// first is answered with. Nil until oneShot arms something, and a read of a
	// nil map is a miss — so an origin nobody armed answers exactly as it did
	// before this field existed.
	after map[string][]byte
}

// Everything under the one mutex, including the reads — which is what get
// below is for. httptest serves on its own goroutine, so a test that reads or
// alters the origin mid-run while reaching into the maps directly would be a
// data race the -race leg finds and nobody can reproduce.
func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)

	if r.URL.Path == "/latest" {
		if f.location != "" {
			w.Header().Set("Location", f.location)
			w.WriteHeader(http.StatusFound)
			return
		}
		if f.tag == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+f.tag, http.StatusFound)
		return
	}
	name := path.Base(r.URL.Path)
	if code, ok := f.status[name]; ok {
		w.WriteHeader(code)
		return
	}
	body, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	// A name armed by oneShot serves what it always served ONCE, and the armed
	// body from then on. body was read above the swap, so the request that
	// springs the trap is still the one that gets the original.
	if second, armed := f.after[name]; armed {
		f.files[name] = second
		delete(f.after, name)
	}
	_, _ = w.Write(body)
}

func (f *fakeRelease) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeRelease) put(name string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = body
}

// oneShot arms name to serve its current body once and second from then on.
//
// It is what makes it observable that a run reads its data out of the SAME
// download it authenticated. While an origin repeats itself, a run that
// verifies one copy of a file and then reads a row out of a second copy is
// indistinguishable from a correct one: the copies are equal, so no assertion
// about content can separate them.
//
// The map is built here rather than in newFakeRelease, so that arming stays
// something a test asks for — every fixture that never calls this keeps a nil
// map and the serving path it always had.
func (f *fakeRelease) oneShot(name string, second []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.after == nil {
		f.after = map[string][]byte{}
	}
	f.after[name] = second
}

// get is the read half, and it is the reason the sentence above is true of the
// reads as well as the writes. Every caller below wants a file the server is
// concurrently able to serve, and reaching into f.files directly would be a
// data race the -race leg finds on a machine nobody can reproduce it on.
func (f *fakeRelease) get(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[name]
}

func (f *fakeRelease) setStatus(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[name] = code
}

func (f *fakeRelease) setTag(tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tag = tag
}

func (f *fakeRelease) setLocation(loc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.location = loc
}

func sumsFor(files map[string][]byte) []byte {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(files)) {
		d := sha256.Sum256(files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(d[:]), name)
	}
	return []byte(b.String())
}

// stagedAssetBody is this test binary's own bytes, served as the release asset.
//
// A synthesised file will not do, because the algorithm EXECUTES what it
// downloaded before it installs it: a padded shell script is not executable on
// Windows, and a copied cmd.exe does not answer --version. A copy of this
// binary answers both on all three operating systems, through a role branch
// TestMain already owns for this package — and it is comfortably over the
// one-megabyte floor, so nothing has to be padded.
func stagedAssetBody(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("reading this test binary to serve as a release asset: %v", err)
	}
	return body
}

// stubReleaseKeys generates a keypair in this process, points releaseKeys at
// it, and returns a signer over the same key.
//
// This is not a hole in the trust root. Production names a compile-time
// constant and never assigns the var; the seam is the shape
// internal/cli/uninstall.go's executablePath established, for the same reason —
// the alternative here is a suite that needs the maintainer's secret key.
func stubReleaseKeys(t *testing.T) signFunc {
	t.Helper()
	pubFile, secFile, err := relsign.GenerateKey()
	if err != nil {
		t.Fatalf("generating a release key: %v", err)
	}
	pub, err := relsign.ParsePublicKey(secondLine(t, pubFile))
	if err != nil {
		t.Fatalf("parsing the generated public key: %v", err)
	}
	sec, err := relsign.ParseSecretKey(secondLine(t, secFile))
	if err != nil {
		t.Fatalf("parsing the generated secret key: %v", err)
	}
	saved := releaseKeys
	t.Cleanup(func() { releaseKeys = saved })
	releaseKeys = func() []relsign.PublicKey { return []relsign.PublicKey{pub} }

	return func(content []byte, tag string) []byte {
		sig, err := sec.Sign(content, relsign.TrustedComment(tag))
		if err != nil {
			t.Fatalf("signing: %v", err)
		}
		return sig
	}
}

// secondLine is the base64 body of a minisign key file, whose first line is an
// untrusted comment signed by nothing.
func secondLine(t *testing.T, file string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(file, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("a key file has %d lines, want 2: %q", len(lines), file)
	}
	return lines[1]
}

func stubVersion(t *testing.T, v string) {
	t.Helper()
	saved := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = saved })
	buildinfo.Version = v
}

// stubUpdateTarget puts the binary this update will replace in a directory of
// the test's own. It moves BOTH the file and the staging directory in one
// assignment, because the staging directory is the target's own directory by
// construction.
func stubUpdateTarget(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ccdad")
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	if err := os.WriteFile(p, []byte("the old ccdad\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := executablePath
	t.Cleanup(func() { executablePath = saved })
	executablePath = func() (string, error) { return p, nil }

	// Resolved, because the command resolves: t.TempDir on macOS hands back
	// /var/folders/…, which is a symlink to /private/var/folders/…, so a test
	// comparing against the unresolved spelling fails on macOS alone.
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// newFakeRelease serves one correctly signed release and points CCDAD_BASE_URL
// at it — the one seam, the same variable both installers read.
func newFakeRelease(t *testing.T, tag string) (*fakeRelease, signFunc) {
	t.Helper()
	sign := stubReleaseKeys(t)

	asset := release.Asset()
	body := stagedAssetBody(t)
	sums := sumsFor(map[string][]byte{
		asset: body,
		// The three notice files the real sums file also covers. They are inert
		// under anchored matching, which is the point of anchoring rather than
		// counting lines, and a fixture without them would never exercise it.
		"LICENSE":                  []byte("license\n"),
		"NOTICE":                   []byte("notice\n"),
		"THIRD-PARTY-LICENSES.txt": []byte("notices\n"),
	})
	f := &fakeRelease{
		tag: tag,
		files: map[string][]byte{
			asset:                    body,
			"sha256sums.txt":         sums,
			"sha256sums.txt.minisig": sign(sums, tag),
		},
		status: map[string]int{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_BASE_URL", srv.URL)
	// What the staged asset answers `--version` with when the command runs it.
	t.Setenv(updateAssetRoleEnv, "ccdad version "+strings.TrimPrefix(tag, "v"))
	return f, sign
}

// signFor re-signs the fixture's checksum file so it names another release, and
// points the staged asset's version at the same tag.
//
// Any test that pins a tag needs it. The trusted comment binds the release, so
// a correctly signed v0.7.0 pair is refused for a `--version v0.9.0` request —
// which is the entire point of the scheme and not something to work around
// quietly by loosening the check.
func signFor(t *testing.T, f *fakeRelease, sign signFunc, tag string) {
	t.Helper()
	f.put("sha256sums.txt.minisig", sign(f.get("sha256sums.txt"), tag))
	t.Setenv(updateAssetRoleEnv, "ccdad version "+strings.TrimPrefix(tag, "v"))
}

// stubUpdateDaemon describes the daemon this update meets, and stubs the
// credential-home probe that startDaemon runs first — a real probe answers
// from whatever is on the machine, which is not something a test can arrange.
//
// updateWorld installs the "no daemon" answer, so a test that reaches the end
// of a run never touches the real singleton. A test that wants one running
// calls this again: stubDaemonWorld saves and restores, so the calls nest.
func stubUpdateDaemon(t *testing.T, running bool) *fakeDaemon {
	t.Helper()
	f := stubDaemonWorld(t, &fakeDaemon{held: running, pid: 4711, pidOK: running})
	saved := credentialHomeClaim
	t.Cleanup(func() { credentialHomeClaim = saved })
	credentialHomeClaim = func() (credhome.Status, error) { return credhome.Status{}, nil }
	return f
}

// updateWorld is the machine every test in this file runs against.
func updateWorld(t *testing.T, running, latest string) (string, *fakeRelease, signFunc) {
	t.Helper()
	isolate(t)
	// packageManagerOwning reads these two, and a contributor's real shell has
	// HOMEBREW_PREFIX set. Cleared so the refusal is something a test asks for
	// rather than something a machine happens to have.
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubVersion(t, running)
	target := stubUpdateTarget(t)
	f, sign := newFakeRelease(t, latest)
	// No daemon, unless a test says otherwise. Without this every run that
	// reaches the end probes the machine's real singleton.
	stubUpdateDaemon(t, false)
	return target, f, sign
}

func decodePayload(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}
	return payload
}

// ----------------------------------------------------------------- the tests

// Exit 2 is the one arm with no reason and no payload, because cobra's
// argument validation runs before RunE and its error is printable by design.
func TestUpdateArgumentErrorsAreUsageErrorsWithNoPayload(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
	}{
		{"a version that is not a tag", []string{"update", "--version", "latest"}},
		{"an empty version", []string{"update", "--version", ""}},
		{"a version with a path in it", []string{"update", "--version", "v1.2.3/../x"}},
		{"a positional argument", []string{"update", "v0.7.0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
			code, stdout, _, top := runRoot(t, append(c.argv, "--json")...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q; exit 2 is the one arm that writes no payload", stdout)
			}
			if top == "" {
				t.Error("nothing was reported; an argument error is printable by design")
			}
			if asked := f.asked(); len(asked) != 0 {
				t.Errorf("origin saw %v, want no request", asked)
			}
		})
	}
}

// Every pre-flight refusal, with the two properties that make it one: no tag in
// the payload, because there is no release yet to have an opinion about, and no
// request at all.
func TestUpdatePreflightRefusalsTouchNoNetwork(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(t *testing.T, target string)
		want    ExitCode
		reason  string
		// human is a fragment of what the SAME refusal says with no --json,
		// and it is the only assertion in this file about the words a person
		// reads. Every other test here passes the flag, so without it the
		// whole human surface is unasserted: the branch that prints at all,
		// the stream it prints on, and — the one that would actually hurt —
		// which package-manager command the message names.
		human string
	}{{
		name: "no key is pinned into this build",
		arrange: func(t *testing.T, _ string) {
			saved := releaseKeys
			t.Cleanup(func() { releaseKeys = saved })
			releaseKeys = func() []relsign.PublicKey { return nil }
		},
		want:   ExitBlocked,
		reason: "no-pinned-key",
		human:  "This ccdad was built with no release key, so it cannot verify an update.",
	}, {
		name:    "a dev build",
		arrange: func(t *testing.T, _ string) { stubVersion(t, "dev") },
		want:    ExitBlocked,
		reason:  "dev-build",
		human:   "This is a development build, so there is no released version to update from.",
	}, {
		name: "a Homebrew-owned binary",
		arrange: func(t *testing.T, target string) {
			t.Setenv("HOMEBREW_PREFIX", filepath.Dir(target))
		},
		want:   ExitBlocked,
		reason: "package-manager",
		// The VERB, not just the manager. upgradeHint sits next to
		// uninstallHint, they differ by one word, and a message that told
		// someone to run `brew uninstall` when they asked to upgrade would be
		// obeyed.
		human: "Run 'brew upgrade ccdad' instead",
	}, {
		// The other half of upgradeHint. It is a row of its own rather than a
		// note on the one above because with a single package manager in the
		// table, deleting the Scoop branch leaves the function answering
		// Homebrew for everything and the suite stays green.
		name: "a Scoop-owned binary",
		arrange: func(t *testing.T, target string) {
			t.Setenv("SCOOP", filepath.Dir(target))
		},
		want:   ExitBlocked,
		reason: "package-manager",
		human:  "Run 'scoop update ccdad' instead",
	}, {
		name: "the binary cannot be located",
		arrange: func(t *testing.T, _ string) {
			saved := executablePath
			t.Cleanup(func() { executablePath = saved })
			executablePath = func() (string, error) { return "", errors.New("no /proc on this machine") }
		},
		want:   ExitFailure,
		reason: "no-executable-path",
		human:  "ccdad cannot tell where its own binary is (no /proc on this machine)",
	}} {
		t.Run(c.name, func(t *testing.T) {
			target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
			c.arrange(t, target)

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != c.want {
				t.Fatalf("exit = %d, want %d", code, c.want)
			}
			payload := decodePayload(t, stdout)
			if got := payload["reason"]; got != c.reason {
				t.Errorf("reason = %v, want %q", got, c.reason)
			}
			if _, ok := payload["tag"]; ok {
				t.Errorf("payload carries a tag (%v); a pre-flight refusal happens before "+
					"there is a release to have an opinion about", payload)
			}
			// The paths are carried only once they are KNOWN, which is why the
			// report holds them behind a flag instead of emitting the zero
			// value: the one refusal that fires before the binary is located
			// must not answer "path": "". Written as a comparison rather than
			// two arms so that neither half can be dropped without the other
			// noticing.
			if _, ok := payload["path"]; ok == (c.reason == "no-executable-path") {
				t.Errorf("path present = %v under reason %q; every refusal that got far enough "+
					"to locate the binary carries it, and no other does", ok, c.reason)
			}
			if got, want := payload["currentVersion"], "0.6.1"; got != want && c.reason != "dev-build" {
				t.Errorf("currentVersion = %v, want %q — it is always present", got, want)
			}
			if got := payload["updated"]; got != false {
				t.Errorf("updated = %v, want false — it is always present", got)
			}

			// The same refusal without the flag. The flag changes the
			// representation and never the answer, so the code repeats — and
			// stdout stays EMPTY, because the human surface belongs on stderr
			// and a payload sharing that stream with a friendly sentence is
			// the one thing the --json contract forbids.
			code, stdout, stderr, _ := runRoot(t, "update")
			if code != c.want {
				t.Fatalf("without --json exit = %d, want %d", code, c.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q; the words go to stderr", stdout)
			}
			if !strings.Contains(stderr, c.human) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, c.human)
			}

			// Last, so it covers BOTH runs: neither representation of a
			// pre-flight refusal is allowed to reach the origin.
			if asked := f.asked(); len(asked) != 0 {
				t.Errorf("origin saw %v, want no request at all", asked)
			}
		})
	}
}

// The writability probe is the staging directory, in one syscall, and it asks
// the right question: the operation being predicted is a rename WITHIN that
// directory, which needs rights on the directory and not on the target file.
func TestUpdateRefusesAnUnwritableInstallDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode is not an ACL: chmod 0500 does not stop a write here, so this arm cannot be arranged")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits, so no chmod can make a directory unwritable for this process")
	}
	target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restored before t.TempDir's own cleanup, which cannot remove a directory
	// it may not write to.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "not-writable" {
		t.Errorf("reason = %v, want %q", got, "not-writable")
	}
	if asked := f.asked(); len(asked) != 0 {
		t.Errorf("origin saw %v; the writability probe is above the network line", asked)
	}
}

// The refusal is ordered AFTER the package-manager check, because a Homebrew
// Cellar is routinely writable by its owner and the reason to refuse there is
// not permission.
func TestUpdateReportsAPackageManagerRatherThanWritability(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	t.Setenv("HOMEBREW_PREFIX", filepath.Dir(target))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "package-manager" {
		t.Errorf("reason = %v, want %q", got, "package-manager")
	}
}

// The alias resolves before the root's unknown-command retag, and every table
// in the tree is keyed on the command PATH, so it costs no row anywhere.
func TestUpgradeIsAnAliasForUpdate(t *testing.T) {
	_, _, _ = updateWorld(t, "dev", "v0.7.0")
	code, stdout, _, _ := runRoot(t, "upgrade", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "dev-build" {
		t.Errorf("reason = %v, want %q — `upgrade` must reach the same RunE", got, "dev-build")
	}
}

// The paths in the payload are two different questions. `path` is the file that
// would be replaced, resolved; `installDir` is the directory the invocation
// came FROM, unresolved, because that is the only correct input to the PATH
// answer beside it — a real path under /opt is not on PATH even when the
// symlink that found it is.
func TestUpdatePayloadCarriesBothPathsOncePathsAreKnown(t *testing.T) {
	target, _, _ := updateWorld(t, "dev", "v0.7.0")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	payload := decodePayload(t, stdout)
	if got := payload["path"]; got != target {
		t.Errorf("path = %v, want %q", got, target)
	}
	if _, ok := payload["onPath"]; !ok {
		t.Error("payload carries no onPath, and the paths are known")
	}
	// installDir by VALUE, and asked of executablePath rather than derived
	// from target. The stub is a package var and is still installed here, so
	// the unresolved spelling is available to this test without the fixture
	// having to hand it back — and it is the only spelling that is correct on
	// a machine whose temp directory is a symlink, where filepath.Dir(target)
	// is the resolved directory and would assert the opposite of the rule.
	invoked, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := payload["installDir"], filepath.Dir(invoked); got != want {
		t.Errorf("installDir = %v, want %q", got, want)
	}
}

func TestUpdateResolvesTheLatestTag(t *testing.T) {
	_, f, _ := updateWorld(t, "0.6.1", "v0.7.0")

	code, stdout, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	payload := decodePayload(t, stdout)
	for k, want := range map[string]any{
		"tag":             "v0.7.0",
		"targetVersion":   "0.7.0",
		"currentVersion":  "0.6.1",
		"resolvedLatest":  true,
		"updateAvailable": true,
	} {
		if got := payload[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if !slices.Contains(f.asked(), "/latest") {
		t.Errorf("origin saw %v, want a request for /latest", f.asked())
	}
}

// A pinned tag is the user's answer, so discovery is not run at all — one fewer
// request, and one fewer thing an origin gets to decide.
func TestUpdateWithAPinnedVersionNeverAsksWhichIsLatest(t *testing.T) {
	_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	// The trusted comment binds the release, so the pair has to name the tag
	// being asked for. Without this the run is refused as `wrong-release`,
	// which is correct behaviour and not what this test is about.
	signFor(t, f, sign, "v0.9.0")

	code, stdout, _, top := runRoot(t, "update", "--version", "v0.9.0", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	payload := decodePayload(t, stdout)
	if got := payload["tag"]; got != "v0.9.0" {
		t.Errorf("tag = %v, want %q", got, "v0.9.0")
	}
	if got := payload["resolvedLatest"]; got != false {
		t.Errorf("resolvedLatest = %v, want false — the tag came from the flag", got)
	}
	if slices.Contains(f.asked(), "/latest") {
		t.Errorf("origin saw %v; a pinned tag must not ask which release is latest", f.asked())
	}
}

// Normalized once, at step 0, so both spellings are one request all the way
// down to the trusted comment the signature binds.
func TestUpdateNormalizesThePinnedVersion(t *testing.T) {
	for _, spelling := range []string{"1.2.3", "v1.2.3", "  v1.2.3  "} {
		t.Run(spelling, func(t *testing.T) {
			_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
			signFor(t, f, sign, "v1.2.3")
			_, stdout, _, _ := runRoot(t, "update", "--version", spelling, "--json")
			if got := decodePayload(t, stdout)["tag"]; got != "v1.2.3" {
				t.Errorf("tag = %v, want %q", got, "v1.2.3")
			}
		})
	}
}

// Exit 3 rather than 5: "the world is already how you asked" is what 3 means in
// this binary, and it is the reading `ccdad update --check && ccdad update`
// depends on.
func TestUpdateIsAlreadyCurrent(t *testing.T) {
	_, f, _ := updateWorld(t, "0.7.0", "v0.7.0")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
	payload := decodePayload(t, stdout)
	if got := payload["reason"]; got != "already-current" {
		t.Errorf("reason = %v, want %q", got, "already-current")
	}
	if got := payload["updateAvailable"]; got != false {
		t.Errorf("updateAvailable = %v, want false", got)
	}
	if slices.Contains(f.asked(), "/download/v0.7.0/sha256sums.txt") {
		t.Errorf("origin saw %v; nothing should be downloaded once the answer is known", f.asked())
	}
}

// A stamp of "v0.6.1" is a real shape: build-release.sh strips the v only in
// the branch that DERIVES a version, so an explicit VERSION=v0.6.1 is stamped
// with it. Compared as strings, that computes "vv0.6.1" and misses.
func TestUpdateComparesParsedVersionsNotStrings(t *testing.T) {
	_, _, _ = updateWorld(t, "v0.6.1", "v0.6.1")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d — a v-prefixed stamp is still the same version", code, ExitNothingToDo)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "already-current" {
		t.Errorf("reason = %v, want %q", got, "already-current")
	}
}

// The second lock on a tag that arrived over an unauthenticated channel: an
// origin that chooses what to serve can answer with an OLD release whose bugs
// are public, and every signature check on that pair would pass.
func TestUpdateRefusesARollbackUnlessTheUserAskedForOne(t *testing.T) {
	target, f, sign := updateWorld(t, "0.7.0", "v0.6.1")
	// The origin is serving a genuine v0.6.1 release, signature and all: the
	// refusal below is about the VERSION being older and nothing else.
	signFor(t, f, sign, "v0.6.1")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "rollback" {
		t.Errorf("reason = %v, want %q", got, "rollback")
	}
	assertBinaryUntouched(t, target)

	// The same tag, named by the user, is not refused. Naming it IS the consent.
	code, _, _, top := runRoot(t, "update", "--version", "v0.6.1", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d with --version, want 0 (%s)", code, top)
	}
}

// --version <what I am on> is how a user re-fetches and re-verifies a binary
// they suspect, which is why step 7 is skipped when the tag is explicit.
func TestUpdateWithTheRunningVersionPinnedRefetches(t *testing.T) {
	_, _, _ = updateWorld(t, "0.7.0", "v0.7.0")

	code, stdout, _, top := runRoot(t, "update", "--version", "v0.7.0", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if got := decodePayload(t, stdout)["updateAvailable"]; got != false {
		t.Errorf("updateAvailable = %v, want false — nothing newer exists, and the run happens anyway", got)
	}
}

// "I cannot compare these" is not "you are up to date".
//
// Four dot-separated fields, which ParseTag refuses outright. A hyphenated
// pre-release such as 2026.08.25-nightly would NOT do: that one parses, and the
// test would then be exercising the ordinary newer-than path.
func TestUpdateWithAnUnparseableRunningVersionDoesNotClaimCurrency(t *testing.T) {
	_, _, _ = updateWorld(t, "2026.08.25.1", "v0.7.0")

	code, stdout, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s) — an unreadable running version falls through to the download", code, top)
	}
	// The exit code alone does not pin this arm. `--check` and every consumer
	// of the payload read updateAvailable, and reporting false for a version
	// nothing could parse IS the claim of currency this test forbids.
	if got := decodePayload(t, stdout)["updateAvailable"]; got != true {
		t.Errorf("updateAvailable = %v, want true — ccdad cannot compare these, and "+
			"\"I cannot compare these\" is not \"you are up to date\"", got)
	}
}

// Discovery that cannot produce a candidate at all is exit 1, not 4: there is
// no release yet to have an opinion about.
func TestUpdateResolveFailures(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(f *fakeRelease)
	}{
		{"a 200 where a redirect was expected", func(f *fakeRelease) { f.setTag("") }},
		{"a location with no tag segment", func(f *fakeRelease) { f.setLocation("/releases/latest") }},
		{"a last segment that is not a version", func(f *fakeRelease) { f.setTag("latest") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
			c.arrange(f)

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != ExitFailure {
				t.Fatalf("exit = %d, want %d", code, ExitFailure)
			}
			payload := decodePayload(t, stdout)
			if got := payload["reason"]; got != "resolve-failed" {
				t.Errorf("reason = %v, want %q", got, "resolve-failed")
			}
			if _, ok := payload["tag"]; ok {
				t.Errorf("payload carries a tag (%v) for a discovery that produced none", payload)
			}
			assertBinaryUntouched(t, target)
		})
	}
}

// assertBinaryUntouched is the property every refusal in this file shares: the
// old binary is exactly as it was, and nothing was staged beside it.
func assertBinaryUntouched(t *testing.T, target string) {
	t.Helper()
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the binary at %s is gone: %v", target, err)
	}
	if string(body) != "the old ccdad\n" {
		t.Errorf("the binary at %s was replaced", target)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".ccdad-update.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("staging directories left behind: %v", leftovers)
	}
}

// A release that publishes no signature is a deliberate choice somewhere, so
// "re-run the installer" is the correct remedy for it. A signature the origin
// could not serve is not a fact about the release at all — and reporting it as
// a tamper verdict would let a flaky origin manufacture a permanent-looking one.
func TestUpdateTellsAnUnsignedReleaseFromABrokenOrigin(t *testing.T) {
	for _, c := range []struct {
		name   string
		file   string
		status int
		want   ExitCode
		reason string
	}{
		{"no signature published", "sha256sums.txt.minisig", http.StatusNotFound, ExitBlocked, "unsigned-release"},
		{"the signature would not serve", "sha256sums.txt.minisig", http.StatusInternalServerError, ExitFailure, "download-sums"},
		{"the signature timed out as a 502", "sha256sums.txt.minisig", http.StatusBadGateway, ExitFailure, "download-sums"},
		{"no sums file at all", "sha256sums.txt", http.StatusNotFound, ExitFailure, "download-sums"},
		{"the sums file would not serve", "sha256sums.txt", http.StatusInternalServerError, ExitFailure, "download-sums"},
	} {
		t.Run(c.name, func(t *testing.T) {
			target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
			f.setStatus(c.file, c.status)

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != c.want {
				t.Fatalf("exit = %d, want %d", code, c.want)
			}
			payload := decodePayload(t, stdout)
			if got := payload["reason"]; got != c.reason {
				t.Errorf("reason = %v, want %q", got, c.reason)
			}
			// A tag IS in hand here: the refusal happened after discovery.
			if got := payload["tag"]; got != "v0.7.0" {
				t.Errorf("tag = %v, want %q — the payload carries the tag once one is resolved", got, "v0.7.0")
			}
			assertBinaryUntouched(t, target)
		})
	}
}

// The unsigned-release remedy names the installer, because an unsigned release
// is a choice somebody made rather than an attack. The tamper remedies must
// not, and that split is tested where each reason is produced.
func TestUnsignedReleaseNamesTheInstaller(t *testing.T) {
	_, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
	f.setStatus("sha256sums.txt.minisig", http.StatusNotFound)

	code, _, stderr, _ := runRoot(t, "update")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if !strings.Contains(stderr, "Re-run the installer") {
		t.Errorf("stderr = %q, want it to send the user to the installer for an unsigned release", stderr)
	}
}

// prehashedSig rewrites a legacy signature's algorithm bytes to the prehashed
// form. The signature itself becomes meaningless, and that is the point: the
// algorithm is compared BEFORE either verification, so a verifier that looked
// at it last would report "does not match" and route the user to the tamper
// remedy for a release that is merely signed a way this build predates.
func prehashedSig(t *testing.T, sig []byte) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(sig), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("a signature has %d lines: %q", len(lines), sig)
	}
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatalf("decoding line 2 of the signature: %v", err)
	}
	raw[1] = 'D' // "Ed" (legacy) becomes "ED" (prehashed)
	lines[1] = base64.StdEncoding.EncodeToString(raw)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// Five sentinels, five reasons, and two remedies. The remedy split is a
// security decision rather than a wording one: NEITHER installer performs a
// signature check, so routing a tamper failure to "re-run the installer" would
// send the user to the one path that will happily accept the altered release,
// on checksums the same attacker controls.
func TestUpdateVerificationFailuresEachHaveTheirOwnReasonAndRemedy(t *testing.T) {
	for _, c := range []struct {
		name           string
		arrange        func(t *testing.T, f *fakeRelease, sign signFunc)
		reason         string
		namesInstaller bool
	}{{
		name: "the signature does not match the file",
		arrange: func(t *testing.T, f *fakeRelease, sign signFunc) {
			f.put("sha256sums.txt.minisig", sign([]byte("some other bytes entirely"), "v0.7.0"))
		},
		reason: "signature",
	}, {
		name: "signed by a key this build does not trust",
		arrange: func(t *testing.T, f *fakeRelease, _ signFunc) {
			// A SECOND keypair, correctly used. stubReleaseKeys points
			// releaseKeys at the new one, so it is restored afterwards and the
			// signature it makes is genuine and simply not ours.
			trusted := releaseKeys
			other := stubReleaseKeys(t)
			releaseKeys = trusted
			f.put("sha256sums.txt.minisig", other(f.get("sha256sums.txt"), "v0.7.0"))
		},
		reason:         "key-id",
		namesInstaller: true,
	}, {
		name: "the trusted comment names another release",
		arrange: func(t *testing.T, f *fakeRelease, sign signFunc) {
			f.put("sha256sums.txt.minisig", sign(f.get("sha256sums.txt"), "v0.6.1"))
		},
		reason: "wrong-release",
	}, {
		name: "a prehashed signature",
		arrange: func(t *testing.T, f *fakeRelease, _ signFunc) {
			f.put("sha256sums.txt.minisig", prehashedSig(t, f.get("sha256sums.txt.minisig")))
		},
		reason:         "algorithm",
		namesInstaller: true,
	}, {
		name: "a body that is not a signature",
		arrange: func(t *testing.T, f *fakeRelease, _ signFunc) {
			f.put("sha256sums.txt.minisig", []byte("this is not a minisign signature\n"))
		},
		reason: "malformed",
	}} {
		t.Run(c.name, func(t *testing.T) {
			target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
			c.arrange(t, f, sign)

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != ExitBlocked {
				t.Fatalf("exit = %d, want %d", code, ExitBlocked)
			}
			if got := decodePayload(t, stdout)["reason"]; got != c.reason {
				t.Errorf("reason = %v, want %q", got, c.reason)
			}
			assertBinaryUntouched(t, target)

			_, _, stderr, _ := runRoot(t, "update")
			// The AFFIRMATIVE instruction, not the bare word. The distrust remedy
			// has to explain why re-running the installer would not help, so it
			// says "installers" in the course of saying not to use one — an
			// assertion on the token alone can never pass for the three rows that
			// take it. What is actually being pinned is whether the user is TOLD
			// to run the installer, which is one sentence with one spelling.
			if got := strings.Contains(stderr, "Re-run the installer"); got != c.namesInstaller {
				t.Errorf("stderr tells the user to re-run the installer = %v, want %v.\nNeither installer "+
					"checks a signature, so a tamper failure must never route the user to it.\n%s",
					got, c.namesInstaller, stderr)
			}
			if !c.namesInstaller && !strings.Contains(stderr, "minisign -Vm") {
				t.Errorf("stderr = %q, want a tamper remedy that names `minisign -Vm`", stderr)
			}
			// The verification line is the one security assurance this command
			// makes to a user, and a refusal must not carry it. Printing it
			// before the check rather than after leaves every verdict correct
			// and every message a lie, which no assertion on a reason or an
			// exit code can see.
			if strings.Contains(stderr, "Verified sha256sums.txt") {
				t.Errorf("stderr = %q, want no verification line on an arm that refused", stderr)
			}
		})
	}
}

// The one line the user is told about the check that actually happened.
func TestUpdateSaysItVerifiedTheChecksumFile(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")

	code, _, stderr, top := runRoot(t, "update")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if !strings.Contains(stderr, "Verified sha256sums.txt against ccdad's release key.") {
		t.Errorf("stderr = %q, want the verification line", stderr)
	}
}

// Two rotations of one key list: a build carrying both keys accepts a signature
// from either, so a rotation strands nobody. A verifier that took only the
// first key of the list would make rotation a permanent stranding of every
// binary already in the field.
func TestUpdateAcceptsASignatureFromEitherPinnedKey(t *testing.T) {
	for _, position := range []string{"first", "second"} {
		t.Run(position, func(t *testing.T) {
			target, f, oldSign := updateWorld(t, "0.6.1", "v0.7.0")
			old := releaseKeys()
			newSign := stubReleaseKeys(t)
			fresh := releaseKeys()

			// The list order is FIXED at [old, fresh]. What the two rows vary
			// is which key signed, and therefore where in the list the
			// verifier has to find it. Reversing the list instead would put
			// the signing key at index 0 in both rows, and a verifier that
			// only ever consults keys[0] would pass the whole test.
			both := append(append([]relsign.PublicKey{}, old...), fresh...)
			releaseKeys = func() []relsign.PublicKey { return both }

			sign := oldSign // old is both[0]
			if position == "second" {
				sign = newSign // fresh is both[1]
			}
			f.put("sha256sums.txt.minisig", sign(f.get("sha256sums.txt"), "v0.7.0"))

			code, _, _, top := runRoot(t, "update")
			if code != ExitOK {
				t.Fatalf("exit = %d, want 0 (%s)", code, top)
			}
			_ = target
		})
	}
}

// A signature says WHO wrote the bytes and never WHAT they are, so a correctly
// signed HTML page is still an HTML page.
func TestUpdateRefusesACorrectlySignedNonSumsFile(t *testing.T) {
	target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	page := []byte("<!doctype html>\n<html><body>502 Bad Gateway</body></html>\n")
	f.put("sha256sums.txt", page)
	f.put("sha256sums.txt.minisig", sign(page, "v0.7.0"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "shape" {
		t.Errorf("reason = %v, want %q", got, "shape")
	}
	assertBinaryUntouched(t, target)
}

// A well-shaped file that does not list this platform is a different refusal
// from one that is not a sums file, and the remedies differ: this one is "the
// release does not carry your target", not "somebody tampered".
func TestUpdateRefusesAReleaseThatDoesNotCarryThisAsset(t *testing.T) {
	target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	// Every notice file and one binary for a platform this is not, correctly
	// signed. The three notice rows are inert under anchored matching, which is
	// what makes this a "not listed" rather than a "shape".
	sums := sumsFor(map[string][]byte{
		"ccdad-plan9-mips":         []byte("not this machine\n"),
		"LICENSE":                  []byte("license\n"),
		"NOTICE":                   []byte("notice\n"),
		"THIRD-PARTY-LICENSES.txt": []byte("notices\n"),
	})
	f.put("sha256sums.txt", sums)
	f.put("sha256sums.txt.minisig", sign(sums, "v0.7.0"))

	code, stdout, _, top := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitBlocked, top)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "not-listed" {
		t.Errorf("reason = %v, want %q", got, "not-listed")
	}
	assertBinaryUntouched(t, target)
}

// The message has to name the asset, because the user's next question is
// "which file did you want?" and the answer is a platform triple they cannot
// derive from anything else on the screen.
func TestNotListedNamesTheAsset(t *testing.T) {
	_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	sums := sumsFor(map[string][]byte{"LICENSE": []byte("license\n")})
	f.put("sha256sums.txt", sums)
	f.put("sha256sums.txt.minisig", sign(sums, "v0.7.0"))

	_, _, stderr, _ := runRoot(t, "update")
	if !strings.Contains(stderr, release.Asset()) {
		t.Errorf("stderr = %q, want it to name %q", stderr, release.Asset())
	}
}

// A bad signature over ENTIRELY CORRECT checksums.
//
// The sums file here is byte-for-byte the real one: it passes the shape check,
// it lists this asset, and its digest is the digest of the asset the origin is
// serving. The ONLY thing wrong with it is that the signature beside it does
// not cover it — which is exactly the state an attacker who can serve bytes but
// cannot sign them produces.
//
// This is the canonical case rather than the ordering proof. It fails ONE gate,
// so every ordering of the three gates reports the same reason for it; the
// fixtures that fail TWO gates at once are the ones that separate the orderings,
// and they are below.
func TestUpdateRefusesABadSignatureOverPerfectlyCorrectChecksums(t *testing.T) {
	target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")

	realSums := f.get("sha256sums.txt")
	if !release.SumsLookLikeSums(realSums) {
		t.Fatal("the fixture's sums file does not pass the shape check, so this test proves nothing")
	}
	if _, ok := release.ExpectedHash(realSums, release.Asset()); !ok {
		t.Fatal("the fixture's sums file does not list this asset, so this test proves nothing")
	}
	// The sums file is left exactly as it is. Only the signature is replaced,
	// with a genuine signature over other bytes.
	f.put("sha256sums.txt.minisig", sign([]byte("bytes this signature really does cover"), "v0.7.0"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	payload := decodePayload(t, stdout)
	if got := payload["reason"]; got != "signature" {
		t.Errorf("reason = %v, want %q", got, "signature")
	}
	if got := payload["updated"]; got != false {
		t.Errorf("updated = %v, want false", got)
	}
	assertBinaryUntouched(t, target)

	// The asset was never even requested, which is the difference between a
	// refusal and a refusal that arrived too late: a run that reached the
	// download had already treated an unverified file as data, and had already
	// spent the transfer finding out.
	for _, p := range f.asked() {
		if strings.HasSuffix(p, "/"+release.Asset()) {
			t.Errorf("the origin was asked for %s; the signature check must come before anything "+
				"in the sums file is believed.\nrequests: %v", release.Asset(), f.asked())
		}
	}
}

// The ordering proof.
//
// A fixture that fails exactly ONE of the three gates cannot tell their
// orderings apart: whichever gate runs first, the single bad thing is the only
// thing any ordering can report, so all of them agree. Every row here fails TWO
// gates at once, and the reason it reports is the NAME OF THE GATE THAT RAN
// FIRST — which is the only way a test in this file can observe the order.
//
// Three gates have five wrong orderings, and what the three rows below pin is
// the three PAIRWISE INVERSIONS — which between them rule out all five, because
// every wrong ordering inverts at least one pair. A run that checks the shape
// before the signature reports `shape` for the first row, one that reads the
// row before the signature reports `not-listed` for the second, and one that
// reads the row before checking the shape reports `not-listed` for the third.
func TestUpdateReportsWhicheverGateRanFirst(t *testing.T) {
	// A proxy's sign-in page: not a checksum file, and it lists nothing.
	page := []byte("<!doctype html>\n<html><body>sign-in required</body></html>\n")
	// Well-shaped and correctly formed, carrying one binary for a platform this
	// is not: it passes the shape check and fails the lookup.
	otherPlatform := sumsFor(map[string][]byte{
		"ccdad-plan9-mips": []byte("not this machine\n"),
		"LICENSE":          []byte("license\n"),
	})
	// The bytes a wrong signature really covers. Anything that is not the sums
	// file will do; naming them makes the fixture readable.
	elsewhere := []byte("bytes this signature really does cover")

	for _, c := range []struct {
		name string
		sums []byte
		// signOver is what the signature beside the sums file actually covers.
		// Passing the sums file itself makes the signature correct.
		signOver []byte
		reason   string
	}{{
		name:     "a page whose signature covers other bytes",
		sums:     page,
		signOver: elsewhere,
		reason:   "signature",
	}, {
		name:     "a sums file for another platform whose signature covers other bytes",
		sums:     otherPlatform,
		signOver: elsewhere,
		reason:   "signature",
	}, {
		// The remedy, not the download, is what this row decides: "this is not
		// a checksum file" is the more useful of the two facts and the one
		// that names a proxy, where `not-listed` would send a user behind a
		// captive portal hunting for a platform the release does not carry.
		name:     "a correctly signed page, which also lists nothing",
		sums:     page,
		signOver: page,
		reason:   "shape",
	}} {
		t.Run(c.name, func(t *testing.T) {
			target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
			f.put("sha256sums.txt", c.sums)
			f.put("sha256sums.txt.minisig", sign(c.signOver, "v0.7.0"))

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != ExitBlocked {
				t.Fatalf("exit = %d, want %d", code, ExitBlocked)
			}
			if got := decodePayload(t, stdout)["reason"]; got != c.reason {
				t.Errorf("reason = %v, want %q — this fixture fails more than one gate, so the "+
					"reason names which gate ran first", got, c.reason)
			}
			assertBinaryUntouched(t, target)
		})
	}
}

// The gates run in the right order, and they all run over the SAME BYTES.
//
// Order and buffer identity are different properties, and every test above pins
// only the first. A run that verified one download of sha256sums.txt and then
// read its row out of a SECOND download would satisfy all of them while
// ExpectedHash ran on bytes nothing had authenticated — a retry, a re-read for
// `--check`, or a helper that took the download base instead of the verified
// bytes would each do it, and each would ship green.
//
// So this origin hands over the real checksum file once and a proxy's sign-in
// page to anyone who asks again. One request is the assertion; the page is what
// makes a second request fail loudly rather than silently.
func TestUpdateReadsTheRowOutOfTheBytesItVerified(t *testing.T) {
	_, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
	f.oneShot("sha256sums.txt", []byte("<!doctype html>\n<html><body>sign-in required</body></html>\n"))

	code, _, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s) — something read the checksum file a second time and got the page", code, top)
	}
	asked := 0
	for _, p := range f.asked() {
		if path.Base(p) == "sha256sums.txt" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("the origin was asked for sha256sums.txt %d times, want exactly 1 — the bytes the "+
			"signature covered are the only bytes anything after it may read.\nrequests: %v",
			asked, f.asked())
	}
}

// The other half of the ordering, and it decides a remedy rather than a
// download: a file that is neither a sums file nor lists this asset must report
// SHAPE, because "this is not a checksum file" is the more useful of the two
// facts and the one that names a proxy.
//
// The WEAKEST of the three tests that say so, and it cannot go red on its own:
// TestUpdateRefusesACorrectlySignedNonSumsFile runs the same fixture and
// asserts the exit code and the binary too, and the third row of the table
// above asserts the same reason with the sharper message. It is kept because it
// is the one of the three that says in its name what the ordering IS, and a
// reader looking for that property finds this before either of the others.
func TestUpdateReportsShapeBeforeListing(t *testing.T) {
	_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	page := []byte("<!doctype html>\n<html><body>sign-in required</body></html>\n")
	f.put("sha256sums.txt", page)
	f.put("sha256sums.txt.minisig", sign(page, "v0.7.0"))

	_, stdout, _, _ := runRoot(t, "update", "--json")
	if got := decodePayload(t, stdout)["reason"]; got != "shape" {
		t.Errorf("reason = %v, want %q — an HTML page also fails to list the asset, and shape is "+
			"the answer that names what actually happened", got, "shape")
	}
}

// The replay: the origin claims a release that does not exist and serves the
// genuine, correctly signed pair from an older one. Every ed25519 check passes.
// The trusted comment is the only thing left, and it must be what refuses —
// with `wrong-release` and not with `signature`, or the refusal was luck.
func TestUpdateRefusesAReplayedOlderPair(t *testing.T) {
	target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	f.setTag("v9.9.9")
	// The pair from v0.6.1: a real sums file with a real signature over it,
	// whose trusted comment names v0.6.1 and nothing else.
	older := sumsFor(map[string][]byte{
		release.Asset(): []byte("an older release's binary\n"),
		"LICENSE":       []byte("license\n"),
	})
	f.put("sha256sums.txt", older)
	f.put("sha256sums.txt.minisig", sign(older, "v0.6.1"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "wrong-release" {
		t.Errorf("reason = %v, want %q — the signature itself is genuine, so anything else here "+
			"means the trusted comment was not what refused", got, "wrong-release")
	}
	assertBinaryUntouched(t, target)
}

// The exact-field rule, which is what makes the trusted comment worth having:
// a substring match is true of ccdaddy:v1.2.30 for a request for v1.2.3.
func TestUpdateDoesNotAcceptATrustedCommentThatMerelyContainsTheTag(t *testing.T) {
	target, f, sign := updateWorld(t, "1.2.2", "v1.2.3")
	f.put("sha256sums.txt.minisig", sign(f.get("sha256sums.txt"), "v1.2.30"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "wrong-release" {
		t.Errorf("reason = %v, want %q", got, "wrong-release")
	}
	assertBinaryUntouched(t, target)
}

// --check is exit 0 for "there is one" and exit 3 for "you are on it". Exit 5
// is deliberately unused: a negative answer here is "the world is already how
// you asked", which is what 3 means, and `ccdad update --check && ccdad update`
// is the idiom that reading exists for.
func TestCheckReportsAvailableWithoutDownloadingTheAsset(t *testing.T) {
	target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")

	code, stdout, _, top := runRoot(t, "update", "--check", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	payload := decodePayload(t, stdout)
	if got := payload["updateAvailable"]; got != true {
		t.Errorf("updateAvailable = %v, want true", got)
	}
	if got := payload["updated"]; got != false {
		t.Errorf("updated = %v, want false — --check replaces nothing", got)
	}
	if got := payload["tag"]; got != "v0.7.0" {
		t.Errorf("tag = %v, want %q", got, "v0.7.0")
	}
	for _, p := range f.asked() {
		if strings.HasSuffix(p, "/"+release.Asset()) {
			t.Errorf("--check downloaded %s; it stops before the megabytes on purpose", release.Asset())
		}
	}
	assertBinaryUntouched(t, target)
}

func TestCheckOnTheCurrentVersionIsExitThree(t *testing.T) {
	_, _, _ = updateWorld(t, "0.7.0", "v0.7.0")

	code, stdout, _, _ := runRoot(t, "update", "--check", "--json")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d", code, ExitNothingToDo)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "already-current" {
		t.Errorf("reason = %v, want %q", got, "already-current")
	}
}

// The position of the return is the point. A --check that answered "available"
// for a release the run would refuse on signature, shape or listing would be
// worse than no --check at all.
func TestCheckFailsEverythingAFullRunFailsBeforeTheDownload(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(t *testing.T, f *fakeRelease, sign signFunc)
		want    ExitCode
		reason  string
	}{{
		name: "a bad signature",
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc) {
			f.put("sha256sums.txt.minisig", sign([]byte("other bytes"), "v0.7.0"))
		},
		want:   ExitBlocked,
		reason: "signature",
	}, {
		name: "a signed page that is not a sums file",
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc) {
			page := []byte("<!doctype html>\n")
			f.put("sha256sums.txt", page)
			f.put("sha256sums.txt.minisig", sign(page, "v0.7.0"))
		},
		want:   ExitBlocked,
		reason: "shape",
	}, {
		name: "a release that does not carry this asset",
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc) {
			sums := sumsFor(map[string][]byte{"LICENSE": []byte("license\n")})
			f.put("sha256sums.txt", sums)
			f.put("sha256sums.txt.minisig", sign(sums, "v0.7.0"))
		},
		want:   ExitBlocked,
		reason: "not-listed",
	}, {
		name: "no signature published",
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc) {
			f.setStatus("sha256sums.txt.minisig", http.StatusNotFound)
		},
		want:   ExitBlocked,
		reason: "unsigned-release",
	}} {
		t.Run(c.name, func(t *testing.T) {
			_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
			c.arrange(t, f, sign)

			code, stdout, _, _ := runRoot(t, "update", "--check", "--json")
			if code != c.want {
				t.Fatalf("exit = %d, want %d", code, c.want)
			}
			if got := decodePayload(t, stdout)["reason"]; got != c.reason {
				t.Errorf("reason = %v, want %q", got, c.reason)
			}
		})
	}
}

// --check is not a read-only command, and the help says so: it creates and
// removes a staging directory, which is what makes its answer about writability
// the real one rather than a guess.
func TestCheckStillProbesWritability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mode is not an ACL: chmod 0500 does not stop a write here, so this arm cannot be arranged")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits, so no chmod can make a directory unwritable for this process")
	}
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	code, stdout, _, _ := runRoot(t, "update", "--check", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "not-writable" {
		t.Errorf("reason = %v, want %q", got, "not-writable")
	}
}

// --version skips step 7, so --check on the tag already running gets here with
// nothing newer to offer. It must not answer "0.7.0 is available; this is
// 0.7.0", which is the sentence an unconditional line produces.
func TestCheckOnAPinnedTagThatIsNotNewerDoesNotClaimOneIsAvailable(t *testing.T) {
	_, _, _ = updateWorld(t, "0.7.0", "v0.7.0")

	code, stdout, _, top := runRoot(t, "update", "--check", "--version", "v0.7.0", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s) — --version is the consent that makes this a legal request", code, top)
	}
	if got := decodePayload(t, stdout)["updateAvailable"]; got != false {
		t.Errorf("updateAvailable = %v, want false", got)
	}

	_, _, stderr, _ := runRoot(t, "update", "--check", "--version", "v0.7.0")
	if strings.Contains(stderr, "is available") {
		t.Errorf("stderr = %q; nothing newer exists, so nothing is available", stderr)
	}
}

// The three things --check cannot know, named on the screen rather than left
// for the user to be surprised by.
//
// "is available" is asserted alongside them, and it is the half that pins the
// DIRECTION. Every other word here appears in the not-newer wording too, so
// without it a --check that told a machine running 0.6.1 that 0.7.0 "verifies,
// and is not newer than the 0.6.1 running here" would satisfy this test.
func TestCheckNamesWhatItCannotAnswer(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")

	code, _, stderr, _ := runRoot(t, "update", "--check")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, word := range []string{"0.7.0", "0.6.1", "size", "checksum", "is available"} {
		if !strings.Contains(stderr, word) {
			t.Errorf("stderr = %q, want it to mention %q", stderr, word)
		}
	}
}

// reseal replaces the asset and re-signs a sums file that matches it, which is
// what an origin serving a different binary honestly would look like. Every
// test below needs it because the digest is checked against a SIGNED row: an
// asset changed on its own fails on the digest, and this helper is for the
// cases that are about something else.
func reseal(t *testing.T, f *fakeRelease, sign signFunc, tag string, body []byte) {
	t.Helper()
	sums := sumsFor(map[string][]byte{
		release.Asset():            body,
		"LICENSE":                  []byte("license\n"),
		"NOTICE":                   []byte("notice\n"),
		"THIRD-PARTY-LICENSES.txt": []byte("notices\n"),
	})
	f.put(release.Asset(), body)
	f.put("sha256sums.txt", sums)
	f.put("sha256sums.txt.minisig", sign(sums, tag))
}

// A signed row and an asset that does not match it. This is the failure the
// checksum exists for, and it must never be reported as a network problem.
func TestUpdateRefusesAChecksumMismatch(t *testing.T) {
	target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
	// The sums file and its signature are left alone; only the served asset
	// changes, so the row is genuine and the bytes are not what it names.
	body := append(stagedAssetBody(t), []byte("\n// tampered\n")...)
	f.put(release.Asset(), body)

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "checksum" {
		t.Errorf("reason = %v, want %q", got, "checksum")
	}
	assertBinaryUntouched(t, target)
}

// Under a megabyte is a proxy or an error page, not a ccdad — and the size gate
// comes BEFORE the digest, so a correctly-checksummed error page is reported as
// what it is.
func TestUpdateRefusesAnUndersizedAsset(t *testing.T) {
	target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	reseal(t, f, sign, "v0.7.0", []byte("<html>sign in to continue</html>\n"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "size" {
		t.Errorf("reason = %v, want %q — the digest of that page is correct, and it is still not a ccdad", got, "size")
	}
	assertBinaryUntouched(t, target)
}

// The ORDER of the two gates, which the test above cannot see.
//
// That one reseals, so its tiny page has a correct digest and the digest gate
// would pass it either way: swapping the two checks leaves it green and still
// reporting `size`. This fixture fails BOTH gates at once — a tiny page the
// signed row does not name — so the reason it reports is the name of the gate
// that ran first, which is the only way a test here can observe the order.
func TestUpdateReportsSizeBeforeChecksumWhenTheAssetFailsBoth(t *testing.T) {
	target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
	// The sums file and its signature are untouched, so the signed row still
	// names the real asset's digest and this page matches nothing.
	f.put(release.Asset(), []byte("<html>sign in to continue</html>\n"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "size" {
		t.Errorf("reason = %v, want %q — a thirty-byte page is an error page whatever its digest says, "+
			"and `checksum` here means the digest gate ran first", got, "size")
	}
	assertBinaryUntouched(t, target)
}

// An asset that will not download at all is something ccdad could not do, not
// something the origin served that ccdad refuses: exit 1.
func TestUpdateReportsAnAssetThatWillNotDownload(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			target, f, _ := updateWorld(t, "0.6.1", "v0.7.0")
			f.setStatus(release.Asset(), status)

			code, stdout, _, _ := runRoot(t, "update", "--json")
			if code != ExitFailure {
				t.Fatalf("exit = %d, want %d", code, ExitFailure)
			}
			if got := decodePayload(t, stdout)["reason"]; got != "download-asset" {
				t.Errorf("reason = %v, want %q", got, "download-asset")
			}
			assertBinaryUntouched(t, target)
		})
	}
}

// The digest is compared case-sensitively, and both sides are lowercase hex by
// construction. This pins that an uppercase row does not become a match: it
// fails to be found at all, which is the anchored-row rule, and the run reports
// not-listed rather than quietly folding.
func TestUpdateDoesNotFoldTheCaseOfADigest(t *testing.T) {
	_, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
	body := stagedAssetBody(t)
	sum := sha256.Sum256(body)
	sums := []byte(strings.ToUpper(hex.EncodeToString(sum[:])) + "  " + release.Asset() + "\n")
	f.put("sha256sums.txt", sums)
	f.put("sha256sums.txt.minisig", sign(sums, "v0.7.0"))

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "shape" {
		t.Errorf("reason = %v, want %q — an uppercase digest is not a checksum row at all", got, "shape")
	}
}

// The staged binary is run before it is installed, because two of the six
// published assets have never been executed by any CI leg: the install smoke
// never runs the darwin/amd64 or the windows/arm64 one, since GitHub's macOS
// runners are arm64 and its Windows runners are amd64. `ccdad update` is the
// first thing that hands those out unattended, and a wrong-architecture asset
// fails to exec — which is exactly what this catches.
func TestUpdateRefusesAStagedBinaryThatWillNotRun(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	t.Setenv(updateAssetRoleEnv, updateAssetRoleFail)

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "smoke" {
		t.Errorf("reason = %v, want %q", got, "smoke")
	}
	assertBinaryUntouched(t, target)
}

// It must exit 0 AND name the release that was asked for. A binary that runs
// and is some other ccdad is not the one that was verified.
func TestUpdateRefusesAStagedBinaryThatNamesAnotherRelease(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	t.Setenv(updateAssetRoleEnv, "ccdad version 9.9.9 (deadbeef)")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d", code, ExitBlocked)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "smoke" {
		t.Errorf("reason = %v, want %q", got, "smoke")
	}
	assertBinaryUntouched(t, target)
}

// smokeRunRecord runs one update and hands back how the staged binary was
// actually invoked, as the child itself saw it.
//
// Nothing in-process can answer that: the smoke run is a real exec, and the two
// things worth pinning about it — the environment it was handed and the
// argument vector it was given — exist only inside that child.
func smokeRunRecord(t *testing.T) updateAssetRecord {
	t.Helper()
	seen := filepath.Join(t.TempDir(), "childrecord")
	t.Setenv(updateAssetRoleEnvFile, seen)

	if code, _, _, top := runRoot(t, "update"); code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the staged binary was never run: %v", err)
	}
	var rec updateAssetRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return rec
}

// The child is marked with the recursion guard, belt to the braces of cobra's
// own short-circuit below.
func TestTheSmokeRunMarksTheChildAsCcdadsOwn(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")

	if got := smokeRunRecord(t).ChildEnv; got != "1" {
		t.Errorf("%s = %q in the child, want %q", daemon.ChildEnvVar, got, "1")
	}
}

// And the argv, which is the half the cobra argument below is only sound FOR.
// `--version` is the one flag cobra answers before it walks up to a persistent
// hook; a smoke run that passed `run` or `daemon start` instead would start
// something on a machine that only asked whether a download executes.
func TestTheSmokeRunPassesNothingButTheVersionFlag(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")

	if got := smokeRunRecord(t).Argv; !slices.Equal(got, []string{"--version"}) {
		t.Errorf("the staged binary was run as %q, want exactly %q", got, []string{"--version"})
	}
}

// The braces themselves: cobra answers --version before it walks up to any
// persistent hook, so the smoke run cannot fire the auto-start hook or the
// scoped-session refusal. This is verified rather than assumed — a smoke run
// that spawned a daemon would be a worse bug than the one it prevents.
func TestCobraAnswersVersionBeforeAnyPersistentHook(t *testing.T) {
	isolate(t)
	root := NewRootCmd()
	inner := root.PersistentPreRunE
	fired := 0
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		fired++
		return inner(cmd, args)
	}

	var out, errOut, topBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"--version"})
	if code := ExecuteWith(root, &topBuf); code != ExitOK {
		t.Fatalf("`ccdad --version` exit = %d (%s)", code, topBuf.String())
	}
	if fired != 0 {
		t.Fatalf("the persistent hook fired %d time(s) for --version. `ccdad update` runs the "+
			"staged binary with exactly that flag on the strength of it NOT firing, and a smoke "+
			"run that spawns a daemon is a worse bug than the one it prevents", fired)
	}
	if !strings.Contains(out.String(), buildinfo.String()) {
		t.Errorf("--version printed %q, want it to name %q", out.String(), buildinfo.String())
	}
}

func TestUpdateStopsTheDaemonAndStartsItFromTheNewBinary(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, true)

	code, stdout, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if len(d.signalled) == 0 {
		t.Error("the daemon was never asked to stop; it would go on running old code and holding the singleton")
	}
	if d.spawns != 1 {
		t.Fatalf("spawns = %d, want 1", d.spawns)
	}
	if got := d.spawnedFrom[0]; got != target {
		t.Errorf("the daemon was started from %q, want %q — the point of the parameter is that the "+
			"process that comes back is the file that was just verified", got, target)
	}
	// The size AT THE MOMENT OF THE SPAWN, which is the only way to see that
	// the restart happened after the replacement rather than before it. The
	// path string alone is the same either way, and so are the target's bytes
	// once the run has finished.
	if got := d.sizeAtSpawn[0]; got < release.MinAssetBytes {
		t.Errorf("the file at %s was %d bytes when the daemon was started from it; the old binary was "+
			"still there, so the daemon came back on the code the update was replacing", target, got)
	}
	payload := decodePayload(t, stdout)
	if got := payload["updated"]; got != true {
		t.Errorf("updated = %v, want true", got)
	}
	if got := payload["daemonRestarted"]; got != true {
		t.Errorf("daemonRestarted = %v, want true", got)
	}
	if got := payload["currentVersion"]; got != "0.6.1" {
		t.Errorf("currentVersion = %v, want %q — it is the RUNNING process's version and does not "+
			"change when updated is true", got, "0.6.1")
	}
	body, err := os.ReadFile(target)
	if err != nil || len(body) < release.MinAssetBytes {
		t.Errorf("the binary at %s was not replaced (%d bytes, %v)", target, len(body), err)
	}
}

func TestUpdateStartsNoDaemonWhenThereWasNone(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, false)

	code, stdout, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if d.spawns != 0 {
		t.Errorf("spawns = %d, want 0 — a machine whose daemon was not up must not gain one from an update", d.spawns)
	}
	if _, ok := decodePayload(t, stdout)["daemonRestarted"]; ok {
		t.Error("payload carries daemonRestarted, and there was no daemon to restart")
	}
}

// A daemon that will not stop is exit 1, and the error is NOT wrapped: CodeFor
// unwraps through fmt.Errorf, so a wrapped sentinel would keep its own code and
// its own silence and the wrapping sentence would never be printed.
func TestUpdateRefusesWhenTheDaemonWillNotStop(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, true)
	d.shutdownErr = errors.New("the shutdown request went nowhere")

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if got := decodePayload(t, stdout)["reason"]; got != "daemon" {
		t.Errorf("reason = %v, want %q", got, "daemon")
	}
	assertBinaryUntouched(t, target)
}

// The binary is already replaced by the time the restart is attempted, so a
// failure to restart is reported and does not fail the command.
func TestUpdateReportsAFailedRestartAndStillSucceeds(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, true)
	d.spawnErr = errors.New("fork failed")

	code, stdout, stderr, _ := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 — the binary is already replaced", code)
	}
	payload := decodePayload(t, stdout)
	if got := payload["updated"]; got != true {
		t.Errorf("updated = %v, want true", got)
	}
	if got := payload["daemonRestarted"]; got != false {
		t.Errorf("daemonRestarted = %v, want false", got)
	}
	_ = stderr
}

// A replacement that cannot happen is exit 1 and leaves the old binary in
// place. The staged file is removed out from under the rename to arrange it,
// which is the only failure the caller can produce on demand.
func TestUpdateReportsAFailedReplacement(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, true)
	// The staging directory is swept the moment the daemon stops, so the
	// rename that follows has nothing to move.
	d.onShutdown = func() {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".ccdad-update.*"))
		for _, m := range matches {
			_ = os.RemoveAll(m)
		}
	}

	code, stdout, _, _ := runRoot(t, "update", "--json")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	payload := decodePayload(t, stdout)
	if got := payload["reason"]; got != "replace-failed" {
		t.Errorf("reason = %v, want %q", got, "replace-failed")
	}
	// The one arm the failure-arm table excludes by name, so this is the only
	// place anything asserts it. A run that set updated before the replacement
	// reports a successful update to every script reading --json, on a machine
	// whose binary is still the old one.
	if got := payload["updated"]; got != false {
		t.Errorf("updated = %v, want false — the replacement is what did not happen", got)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "the old ccdad\n" {
		t.Errorf("the old binary is gone (%q, %v); a failed replacement must leave the machine as it was", body, err)
	}
}

func TestUpdatePrintsWhatItDid(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	_ = stubUpdateDaemon(t, true)

	code, _, stderr, top := runRoot(t, "update")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	for _, line := range []string{
		"Stopped the ccdad daemon",
		"Verified sha256sums.txt against ccdad's release key.",
		"Replaced " + target + ": 0.6.1 -> 0.7.0",
		"Started the ccdad daemon",
	} {
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr does not contain %q.\n%s", line, stderr)
		}
	}
}

// Everything the command says goes to stderr, and --json silences the prose it
// owns. stdout is the payload and nothing else, which the tree-wide contract
// test also pins.
func TestUpdateJSONSaysNothingItselfOnStderr(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
	_ = stubUpdateDaemon(t, true)

	code, stdout, stderr, _ := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stderr, "Replaced ") || strings.Contains(stderr, "Verified ") {
		t.Errorf("--json printed the human prose on stderr:\n%s", stderr)
	}
	if len(decodePayload(t, stdout)) == 0 {
		t.Error("no payload on stdout")
	}
}

// The PATH note is computed against the UNRESOLVED directory. An install that
// is a symlink on PATH pointing at a real file under /opt is the shape this
// exists for: warning about /opt would train the user to ignore the one message
// that matters.
func TestThePathNoteIsComputedFromTheDirectoryTheInvocationCameFrom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege the runner does not grant; the note's input is the same code either way")
	}
	isolate(t)
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("SCOOP", "")
	stubVersion(t, "0.6.1")

	realDir := t.TempDir() // never on PATH
	linkDir := t.TempDir() // on PATH
	real := filepath.Join(realDir, "ccdad")
	if err := os.WriteFile(real, []byte("the old ccdad\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "ccdad")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	saved := executablePath
	t.Cleanup(func() { executablePath = saved })
	executablePath = func() (string, error) { return link, nil }
	t.Setenv("PATH", linkDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	newFakeRelease(t, "v0.7.0")
	_ = stubUpdateDaemon(t, false)

	code, _, stderr, top := runRoot(t, "update")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if strings.Contains(stderr, "not on your PATH") {
		t.Errorf("the PATH note fired for a symlinked install that IS on PATH:\n%s", stderr)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Replaced "+resolved) {
		t.Errorf("stderr = %q, want the RESOLVED file to be the one named as replaced", stderr)
	}
}

// And the arm that must fire, exactly once. An update that is off PATH replaces
// one binary while `ccdad` keeps resolving to an older one somewhere else, and
// the user sees a successful update and no change.
func TestThePathNoteFiresOnceWhenTheDirectoryIsNotOnPath(t *testing.T) {
	target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
	_ = stubUpdateDaemon(t, false)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "somewhere-else"))

	code, _, stderr, top := runRoot(t, "update")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if n := strings.Count(stderr, "not on your PATH"); n != 1 {
		t.Errorf("the PATH note appeared %d times, want 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "setup-path") {
		t.Errorf("stderr = %q, want the note to name the command that fixes it", stderr)
	}
	_ = target
}

// `update` is allowed inside a `ccdad run` session, because it writes only the
// ccdad BINARY and a session scopes Claude Code's credential and config homes
// and nothing else. The one part that would not be safe is the restart, and it
// is skipped in here rather than the whole command being refused.
func TestUpdateRunsInsideAScopedSessionAndLeavesTheDaemonStopped(t *testing.T) {
	for _, enter := range []struct {
		name string
		fn   func(*testing.T, string) string
	}{
		{"a run session", enterRunSession},
		{"a --full-profile session", enterFullProfileSession},
	} {
		t.Run(enter.name, func(t *testing.T) {
			target, _, _ := updateWorld(t, "0.6.1", "v0.7.0")
			d := stubUpdateDaemon(t, true)
			enter.fn(t, "acct-1")

			code, stdout, stderr, top := runRoot(t, "update", "--json")
			if code != ExitOK {
				t.Fatalf("exit = %d, want 0 (%s) — update is on the allowed side of the "+
					"scoped-session table, not the refused side", code, top)
			}
			if len(d.signalled) == 0 {
				t.Error("the daemon was not stopped; leaving it running keeps the machine on old " +
					"code until somebody restarts it by hand, which is the alternative that was rejected")
			}
			if d.spawns != 0 {
				t.Errorf("spawns = %d, want 0 — daemon.ChildEnv resolves both path variables before "+
					"handing them on, so a daemon started in here would manage this session's "+
					"directory for the rest of its life, after `run` has deleted it", d.spawns)
			}
			payload := decodePayload(t, stdout)
			if got := payload["updated"]; got != true {
				t.Errorf("updated = %v, want true", got)
			}
			if got := payload["daemonRestarted"]; got != false {
				t.Errorf("daemonRestarted = %v, want false", got)
			}
			_ = stderr
			_ = target
		})
	}
}

// The sentence is gated on the daemon having been running, because on a machine
// whose daemon was not up it is simply false.
func TestTheScopedSessionNoticeAppearsOnlyWhenThereWasADaemon(t *testing.T) {
	const notice = "will stay stopped for this `ccdad run` session"

	t.Run("there was one", func(t *testing.T) {
		_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
		_ = stubUpdateDaemon(t, true)
		enterRunSession(t, "acct-1")

		code, _, stderr, top := runRoot(t, "update")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0 (%s)", code, top)
		}
		if !strings.Contains(stderr, notice) {
			t.Errorf("stderr = %q, want it to say the daemon stays stopped", stderr)
		}
		if !strings.Contains(stderr, "normal shell") {
			t.Errorf("stderr = %q, want it to say how to bring the daemon back", stderr)
		}
	})

	t.Run("there was not", func(t *testing.T) {
		_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
		_ = stubUpdateDaemon(t, false)
		enterRunSession(t, "acct-1")

		code, _, stderr, top := runRoot(t, "update")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0 (%s)", code, top)
		}
		if strings.Contains(stderr, notice) {
			t.Errorf("stderr = %q, want no sentence about a daemon that was never up", stderr)
		}
	})
}

// Outside a session the restart happens, which is the control the two tests
// above need: without it they would pass on a build that never restarts at all.
func TestOutsideASessionTheDaemonComesBack(t *testing.T) {
	_, _, _ = updateWorld(t, "0.6.1", "v0.7.0")
	d := stubUpdateDaemon(t, true)

	if code, _, _, top := runRoot(t, "update"); code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s)", code, top)
	}
	if d.spawns != 1 {
		t.Fatalf("spawns = %d, want 1 outside a session", d.spawns)
	}
}

// Every way this command can fail, and the two things all of them owe the user:
// the binary they are running is exactly as it was, and there is no
// .ccdad-update.* directory sitting beside it.
//
// A table rather than an assertion inside each test, because the property is
// about the COMMAND and not about any one refusal — and because the arm
// somebody adds later costs one row here rather than a decision.
//
// Three reasons are deliberately not rows. `no-executable-path` cannot be
// arranged and then asserted against, since the assertion needs the very path
// the arm says cannot be found. `key-id` needs a second keypair and is asserted
// where it is produced, with the same untouched-binary check. `replace-failed`
// needs the staging directory swept out from under the rename mid-run, which is
// asserted where it is produced for the same reason.
//
// `already-current` is a row even though it is not a failure: it returns after
// the staging directory has been created, so it owes the same two things, and
// it is the arm most likely to be reached on an ordinary machine.
func TestUpdateLeavesNothingBehindOnEveryFailure(t *testing.T) {
	for _, c := range []struct {
		reason  string
		want    ExitCode
		argv    []string
		arrange func(t *testing.T, f *fakeRelease, sign signFunc, d *fakeDaemon, target string)
	}{{
		reason: "no-pinned-key", want: ExitBlocked,
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			saved := releaseKeys
			t.Cleanup(func() { releaseKeys = saved })
			releaseKeys = func() []relsign.PublicKey { return nil }
		},
	}, {
		reason: "dev-build", want: ExitBlocked,
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			stubVersion(t, "dev")
		},
	}, {
		reason: "package-manager", want: ExitBlocked,
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, target string) {
			t.Setenv("HOMEBREW_PREFIX", filepath.Dir(target))
		},
	}, {
		reason: "resolve-failed", want: ExitFailure,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.setTag("")
		},
	}, {
		reason: "already-current", want: ExitNothingToDo,
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			stubVersion(t, "0.7.0")
		},
	}, {
		reason: "rollback", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.setTag("v0.1.0")
		},
	}, {
		reason: "download-sums", want: ExitFailure,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.setStatus("sha256sums.txt", http.StatusInternalServerError)
		},
	}, {
		reason: "unsigned-release", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.setStatus("sha256sums.txt.minisig", http.StatusNotFound)
		},
	}, {
		reason: "signature", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc, _ *fakeDaemon, _ string) {
			f.put("sha256sums.txt.minisig", sign([]byte("other bytes"), "v0.7.0"))
		},
	}, {
		reason: "wrong-release", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc, _ *fakeDaemon, _ string) {
			f.put("sha256sums.txt.minisig", sign(f.get("sha256sums.txt"), "v0.6.1"))
		},
	}, {
		reason: "algorithm", want: ExitBlocked,
		arrange: func(t *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.put("sha256sums.txt.minisig", prehashedSig(t, f.get("sha256sums.txt.minisig")))
		},
	}, {
		reason: "malformed", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.put("sha256sums.txt.minisig", []byte("not a signature\n"))
		},
	}, {
		reason: "shape", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc, _ *fakeDaemon, _ string) {
			page := []byte("<!doctype html>\n")
			f.put("sha256sums.txt", page)
			f.put("sha256sums.txt.minisig", sign(page, "v0.7.0"))
		},
	}, {
		reason: "not-listed", want: ExitBlocked,
		arrange: func(_ *testing.T, f *fakeRelease, sign signFunc, _ *fakeDaemon, _ string) {
			sums := sumsFor(map[string][]byte{"LICENSE": []byte("license\n")})
			f.put("sha256sums.txt", sums)
			f.put("sha256sums.txt.minisig", sign(sums, "v0.7.0"))
		},
	}, {
		reason: "download-asset", want: ExitFailure,
		arrange: func(_ *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.setStatus(release.Asset(), http.StatusInternalServerError)
		},
	}, {
		reason: "size", want: ExitBlocked,
		arrange: func(t *testing.T, f *fakeRelease, sign signFunc, _ *fakeDaemon, _ string) {
			reseal(t, f, sign, "v0.7.0", []byte("too small to be a ccdad\n"))
		},
	}, {
		reason: "checksum", want: ExitBlocked,
		arrange: func(t *testing.T, f *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			f.put(release.Asset(), append(stagedAssetBody(t), []byte("\n// tampered\n")...))
		},
	}, {
		reason: "smoke", want: ExitBlocked,
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, _ string) {
			t.Setenv(updateAssetRoleEnv, updateAssetRoleFail)
		},
	}, {
		reason: "daemon", want: ExitFailure,
		arrange: func(_ *testing.T, _ *fakeRelease, _ signFunc, d *fakeDaemon, _ string) {
			d.shutdownErr = errors.New("the shutdown request went nowhere")
		},
	}, {
		reason: "not-writable", want: ExitBlocked,
		argv: []string{"update", "--check", "--json"},
		arrange: func(t *testing.T, _ *fakeRelease, _ signFunc, _ *fakeDaemon, target string) {
			if runtime.GOOS == "windows" {
				t.Skip("a mode is not an ACL: chmod 0500 does not stop a write here")
			}
			if os.Geteuid() == 0 {
				t.Skip("root ignores the mode bits, so no chmod can make a directory unwritable")
			}
			dir := filepath.Dir(target)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		},
	}} {
		t.Run(c.reason, func(t *testing.T) {
			target, f, sign := updateWorld(t, "0.6.1", "v0.7.0")
			d := stubUpdateDaemon(t, true)
			c.arrange(t, f, sign, d, target)

			argv := c.argv
			if argv == nil {
				argv = []string{"update", "--json"}
			}
			code, stdout, _, _ := runRoot(t, argv...)
			if code != c.want {
				t.Fatalf("exit = %d, want %d", code, c.want)
			}
			payload := decodePayload(t, stdout)
			if got := payload["reason"]; got != c.reason {
				t.Fatalf("reason = %v, want %q", got, c.reason)
			}
			if got := payload["updated"]; got != false {
				t.Errorf("updated = %v, want false", got)
			}
			assertBinaryUntouched(t, target)
		})
	}
}

// Every one of those arms is a row in the exit-code contract, and the contract
// is what a supervisor branches on. Nothing above pins that the codes are the
// ones the documentation promises rather than merely self-consistent, so this
// asserts the split directly: 4 is what the origin served and ccdad refused, 1
// is what ccdad itself could not do.
func TestUpdateExitCodesSplitRefusalsFromFailures(t *testing.T) {
	refusals := []string{
		"no-pinned-key", "dev-build", "package-manager", "not-writable", "rollback",
		"unsigned-release", "signature", "key-id", "wrong-release", "algorithm",
		"malformed", "shape", "not-listed", "size", "checksum", "smoke",
	}
	failures := []string{
		"no-executable-path", "resolve-failed", "download-sums", "download-asset",
		"daemon", "replace-failed",
	}
	for _, r := range refusals {
		if got := updateReasonCode(r); got != ExitBlocked {
			t.Errorf("%s maps to %d, want %d", r, got, ExitBlocked)
		}
	}
	for _, r := range failures {
		if got := updateReasonCode(r); got != ExitFailure {
			t.Errorf("%s maps to %d, want %d", r, got, ExitFailure)
		}
	}
	if got := updateReasonCode("already-current"); got != ExitNothingToDo {
		t.Errorf("already-current maps to %d, want %d", got, ExitNothingToDo)
	}
	if got := updateReasonCode("nonsense"); got != ExitFailure {
		t.Errorf("an unknown reason maps to %d, want %d — the fallback must be the safe half", got, ExitFailure)
	}
}
