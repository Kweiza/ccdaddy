package cclink

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// withGlobalConfig sandboxes ~/.claude.json inside t.TempDir() and returns its
// path. CLAUDE_CONFIG_DIR is what moves it, and the assertion at the end is
// what makes an unsandboxed run loud rather than silent -- these tests WRITE
// this file, and the real one holds the developer's project history.
func withGlobalConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", home)

	path := mustPath(ccpath.GlobalConfigPath())
	if filepath.Dir(path) != home {
		t.Fatalf("global config resolved to %q, outside the sandbox %q -- refusing to run", path, home)
	}
	return path
}

func writeGlobal(t *testing.T, body string) string {
	t.Helper()
	path := mustPath(ccpath.GlobalConfigPath())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readGlobal(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(mustPath(ccpath.GlobalConfigPath()))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The approval value is the LAST twenty characters of the trimmed key.
//
// Pinned against a LITERAL rather than against APIKeyApproval's own output,
// because the two plausible implementations -- last twenty and first twenty --
// produce equally well-formed values, and every caller in the tree computes the
// expectation by calling this same function. A test written that way passes
// under both, and the wrong one is silently broken in the worst way available:
// Claude Code stores `key.trim().slice(-20)` and matches on it, so a prefix
// entry means an interactive session refuses the key at every start with no
// hint as to why.
func TestAPIKeyApprovalIsTheLastTwentyCharacters(t *testing.T) {
	// 36 characters, with a distinct head and tail so the two candidate
	// implementations cannot produce the same answer.
	const key = "sk-ant-api03-HEADHEADzzzzTAILTAILTAIL"
	if got, want := APIKeyApproval(key), "zzzzTAILTAILTAIL"; !strings.HasSuffix(key, got) || len(got) != 20 {
		t.Fatalf("APIKeyApproval(%q) = %q; want the last twenty characters (which end %q)", key, got, want)
	}
	if got := APIKeyApproval(key); got != key[len(key)-20:] {
		t.Fatalf("APIKeyApproval = %q, want %q", got, key[len(key)-20:])
	}
	// Claude Code trims before slicing, so a key with surrounding whitespace --
	// which is what a copy-paste out of a terminal produces -- has to yield the
	// same value as the clean one, or the entry never matches.
	if got, want := APIKeyApproval("  "+key+"\n"), APIKeyApproval(key); got != want {
		t.Fatalf("APIKeyApproval trims: got %q, want %q", got, want)
	}
	// Shorter than the window is the whole string, not a panic.
	if got, want := APIKeyApproval("short"), "short"; got != want {
		t.Fatalf("APIKeyApproval(%q) = %q, want %q", "short", got, want)
	}
}

// The config is a read-modify-write of the user's file, not a replacement, and
// the KEY ORDER survives.
//
// Order matters for a practical reason rather than an aesthetic one: this file
// runs to hundreds of kilobytes and people keep it under version control, so a
// sorted rewrite turns a two-key edit into a whole-file diff on every switch.
func TestUpdateGlobalConfigPreservesEverythingElseAndItsOrder(t *testing.T) {
	withGlobalConfig(t)
	writeGlobal(t, `{
  "zzzLast": 1,
  "projects": {
    "/home/x": {
      "history": [
        "one"
      ]
    }
  },
  "aaaFirst": "keep"
}`)

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV")
	}); err != nil {
		t.Fatal(err)
	}

	got := readGlobal(t)
	for _, want := range []string{`"zzzLast": 1`, `"aaaFirst": "keep"`, `"one"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the config lost %s:\n%s", want, got)
		}
	}
	// The pre-existing keys keep their positions and the new ones are appended,
	// which is what a JavaScript `{...config, key: value}` does and therefore
	// what Claude Code's own writes do.
	order := []string{`"zzzLast"`, `"projects"`, `"aaaFirst"`, `"primaryApiKey"`, `"customApiKeyResponses"`}
	at := -1
	for _, key := range order {
		i := strings.Index(got, key)
		if i < 0 {
			t.Fatalf("%s missing from:\n%s", key, got)
		}
		if i < at {
			t.Fatalf("%s moved; keys must keep file order:\n%s", key, got)
		}
		at = i
	}
}

// A config file that is not a JSON object is refused rather than replaced.
// Whatever is in there is the user's, and a file we cannot merge into is one we
// cannot edit without losing it.
func TestUpdateGlobalConfigRefusesANonObject(t *testing.T) {
	withGlobalConfig(t)
	writeGlobal(t, `["not", "an", "object"]`)

	err := UpdateGlobalConfig(func(g *GlobalConfig) error { return SetPrimaryAPIKey(g, "sk-ant-api03-K") })
	if !errors.Is(err, ErrGlobalConfigShape) {
		t.Fatalf("UpdateGlobalConfig() = %v, want ErrGlobalConfigShape", err)
	}
	if got := readGlobal(t); got != `["not", "an", "object"]` {
		t.Fatalf("the refused write touched the file: %s", got)
	}
}

// A mutation that changes nothing must not rewrite the file.
//
// Claude Code watches this file and re-reads it when it changes, so a no-op
// switch that rewrites it makes every running session reload a config that did
// not change. It also means ccdad never normalises formatting it was not asked
// to touch.
//
// Asserted on the BYTES rather than on the mtime, and the difference is not
// cosmetic: an earlier version of this test compared modification times and
// passed under the mutation that deletes the check entirely. Inode timestamps
// come from the kernel's coarse clock, so a write and a rewrite microseconds
// apart can carry the identical mtime — the assertion was never capable of
// failing. The fixture is deliberately written in a formatting ccdad's encoder
// would NOT produce (one line, no indent), so any rewrite at all is visible.
func TestUpdateGlobalConfigSkipsAnUnchangedDocument(t *testing.T) {
	withGlobalConfig(t)
	const asWritten = `{"a": 1}`
	writeGlobal(t, asWritten)

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if got := readGlobal(t); got != asWritten {
		t.Fatalf("an unchanged config was rewritten as %q; Claude Code re-reads this file when it changes", got)
	}
}

// The write goes THROUGH a symlink, not over it.
//
// This is the one place ccdad deliberately diverges from how it treats the
// credentials file, and the divergence is Claude Code's: it writes this file
// with `allowSymlink: true` and refuses to follow the credentials file at all.
// A ~/.claude.json symlinked into a dotfiles repository is an ordinary setup,
// and a sibling-temp-plus-rename would replace the link with a regular file --
// silently unlinking the user's config from the repository that manages it.
func TestUpdateGlobalConfigWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege Windows does not grant by default")
	}
	withGlobalConfig(t)
	path := mustPath(ccpath.GlobalConfigPath())

	real := filepath.Join(t.TempDir(), "dotfiles-claude.json")
	if err := os.WriteFile(real, []byte(`{"a": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV")
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file; a dotfiles-managed config would be unlinked")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "primaryApiKey") {
		t.Fatalf("the target of the symlink was not updated: %s", raw)
	}
}

// The write happens while Claude Code's config lock is held.
//
// Same class as TestActivateWritesWhileTheCredentialLocksAreHeld and the same
// reason for asserting it at the RENAME rather than around the call: an
// implementation that read under the lock, released, and then wrote would
// satisfy every other test in this file while racing Claude Code's own
// read-modify-write of a file it rewrites on every startup.
func TestUpdateGlobalConfigWritesUnderTheConfigLock(t *testing.T) {
	withGlobalConfig(t)
	writeGlobal(t, `{"a": 1}`)
	lockDir := mustPath(cclock.GlobalConfigLockDir())

	var heldAtWrite bool
	var contendErr error
	orig := renameFile
	renameFile = func(from, to string) error {
		_, statErr := os.Stat(lockDir)
		heldAtWrite = statErr == nil
		lk, err := cclock.Acquire(lockDir, cclock.Options{Stale: time.Minute, Timeout: 0})
		if err == nil {
			_ = lk.Release()
		}
		contendErr = err
		return orig(from, to)
	}
	t.Cleanup(func() { renameFile = orig })

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV")
	}); err != nil {
		t.Fatal(err)
	}

	if !heldAtWrite {
		t.Error("the config was written with its lock not held")
	}
	if !errors.Is(contendErr, cclock.ErrTimeout) {
		t.Errorf("acquiring the config lock during the write = %v, want cclock.ErrTimeout", contendErr)
	}
}

// The lock ccdad takes has to be the one Claude Code takes, or the two exclude
// nothing. Claude Code passes proper-lockfile an explicit
// `lockfilePath: `${configPath}.lock“, so the name is the config file's own
// path with .lock appended.
func TestGlobalConfigLockIsNamedAfterTheConfigFile(t *testing.T) {
	withGlobalConfig(t)
	path := mustPath(ccpath.GlobalConfigPath())
	if got, want := mustPath(cclock.GlobalConfigLockDir()), path+".lock"; got != want {
		t.Fatalf("GlobalConfigLockDir() = %q, want %q", got, want)
	}
}

// customApiKeyResponses is merged, not replaced: a rejected list ccdad does not
// understand, and any other sub-key, survives having a key approved.
func TestApprovingAKeyPreservesTheOtherResponses(t *testing.T) {
	withGlobalConfig(t)
	writeGlobal(t, `{"customApiKeyResponses":{"approved":["alreadyThere"],"rejected":["nope"],"future":1}}`)

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV")
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	approved := ApprovedAPIKeys(cfg)
	if len(approved) != 2 || approved[0] != "alreadyThere" {
		t.Fatalf("approved = %v, want the existing entry kept and the new one appended", approved)
	}
	raw, _ := cfg.Get("customApiKeyResponses")
	var responses map[string]json.RawMessage
	if err := json.Unmarshal(raw, &responses); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"rejected", "future"} {
		if _, ok := responses[key]; !ok {
			t.Fatalf("customApiKeyResponses.%s was destroyed: %s", key, raw)
		}
	}
}

// Approving the same key twice must not grow the list. Claude Code's own
// writer is `approved.includes(v) ? approved : [...approved, v]`.
func TestApprovingTheSameKeyTwiceIsIdempotent(t *testing.T) {
	withGlobalConfig(t)
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	for i := 0; i < 2; i++ {
		if err := UpdateGlobalConfig(func(g *GlobalConfig) error { return SetPrimaryAPIKey(g, key) }); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if approved := ApprovedAPIKeys(cfg); len(approved) != 1 {
		t.Fatalf("approved = %v, want one entry", approved)
	}
}

// A missing file is an empty document, which is the state of a machine Claude
// Code has never run on -- and the state a first `ccdad switch` to an api-key
// account has to write into.
func TestLoadGlobalConfigTreatsAMissingFileAsEmpty(t *testing.T) {
	withGlobalConfig(t)
	cfg, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("LoadGlobalConfig() = %v, want an empty document", err)
	}
	if len(cfg.Keys()) != 0 {
		t.Fatalf("Keys() = %v, want none", cfg.Keys())
	}

	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV")
	}); err != nil {
		t.Fatal(err)
	}
	if got := readGlobal(t); !strings.HasPrefix(got, "{\n  \"primaryApiKey\"") {
		t.Fatalf("first write produced:\n%s", got)
	}
}

// ClearPrimaryAPIKey removes the key and LEAVES the approval entry. The entry
// is not a credential -- it is twenty characters -- and it records a consent
// the user gave through Claude Code's own prompt.
func TestClearPrimaryAPIKeyLeavesTheApproval(t *testing.T) {
	withGlobalConfig(t)
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	if err := UpdateGlobalConfig(func(g *GlobalConfig) error { return SetPrimaryAPIKey(g, key) }); err != nil {
		t.Fatal(err)
	}
	if err := UpdateGlobalConfig(func(g *GlobalConfig) error {
		if !ClearPrimaryAPIKey(g) {
			t.Error("ClearPrimaryAPIKey() = false, want it to report the key it removed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := PrimaryAPIKey(cfg); ok {
		t.Fatalf("PrimaryAPIKey() = %q, want it gone", got)
	}
	if approved := ApprovedAPIKeys(cfg); len(approved) != 1 || approved[0] != APIKeyApproval(key) {
		t.Fatalf("approved = %v, want the entry left in place", approved)
	}
}

// UpdateGlobalConfigAt is the whole reason `ccdad run --full-profile` can
// serve an API-key account, and the property that makes it safe is negative:
// the file the caller named is the ONLY one it writes. A variant that
// re-resolved ccpath.GlobalConfigPath() anywhere inside would pass every
// positive assertion and quietly rewrite the user's live configuration.
func TestUpdateGlobalConfigAtWritesTheNamedFileAndNoOther(t *testing.T) {
	live := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	livePath := filepath.Join(live, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"numStartups":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), ".claude.json")
	if err := UpdateGlobalConfigAt(other, func(g *GlobalConfig) error {
		return SetPrimaryAPIKey(g, "sk-ant-api-PROFILE")
	}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadGlobalConfigAt(other)
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := PrimaryAPIKey(got); !ok || key != "sk-ant-api-PROFILE" {
		t.Errorf("primaryApiKey at %s = %q (set: %v), want the key that was written", other, key, ok)
	}
	after, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the live config was rewritten\nbefore: %s\nafter:  %s", before, after)
	}
}

// The lock is named after the config PATH, so locking a profile's config locks
// the file the Claude Code inside that profile would lock. Asserted through
// the lock directory rather than through an outcome, because two processes
// locking different files exclude nothing and every single-process test passes
// either way.
func TestUpdateGlobalConfigAtLocksTheFileItWrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	profile := filepath.Join(t.TempDir(), ".claude.json")

	var seen string
	if err := UpdateGlobalConfigAt(profile, func(g *GlobalConfig) error {
		if _, err := os.Stat(profile + ".lock"); err == nil {
			seen = profile + ".lock"
		}
		return SetPrimaryAPIKey(g, "sk-ant-api-X")
	}); err != nil {
		t.Fatal(err)
	}
	if seen == "" {
		t.Fatalf("no lock was held at %s.lock while its config was being rewritten", profile)
	}
}
