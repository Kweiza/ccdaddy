package cli

import (
	"crypto/sha256"
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

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/credhome"
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
// credential-home probe that startDaemonFrom runs first — a real probe answers
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
	}{{
		name: "no key is pinned into this build",
		arrange: func(t *testing.T, _ string) {
			saved := releaseKeys
			t.Cleanup(func() { releaseKeys = saved })
			releaseKeys = func() []relsign.PublicKey { return nil }
		},
		want:   ExitBlocked,
		reason: "no-pinned-key",
	}, {
		name:    "a dev build",
		arrange: func(t *testing.T, _ string) { stubVersion(t, "dev") },
		want:    ExitBlocked,
		reason:  "dev-build",
	}, {
		name: "a Homebrew-owned binary",
		arrange: func(t *testing.T, target string) {
			t.Setenv("HOMEBREW_PREFIX", filepath.Dir(target))
		},
		want:   ExitBlocked,
		reason: "package-manager",
	}, {
		name: "the binary cannot be located",
		arrange: func(t *testing.T, _ string) {
			saved := executablePath
			t.Cleanup(func() { executablePath = saved })
			executablePath = func() (string, error) { return "", errors.New("no /proc on this machine") }
		},
		want:   ExitFailure,
		reason: "no-executable-path",
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
			if got, want := payload["currentVersion"], "0.6.1"; got != want && c.reason != "dev-build" {
				t.Errorf("currentVersion = %v, want %q — it is always present", got, want)
			}
			if got := payload["updated"]; got != false {
				t.Errorf("updated = %v, want false — it is always present", got)
			}
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
	if _, ok := payload["installDir"]; !ok {
		t.Error("payload carries no installDir, and the paths are known")
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

	code, _, _, top := runRoot(t, "update", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (%s) — an unreadable running version falls through to the download", code, top)
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
