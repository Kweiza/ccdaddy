package cclink

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// This file handles Claude Code's OTHER file: ~/.claude.json, the general
// config. It is not the credential store and must not be treated like one.
//
// ccdad touches exactly three of its keys:
//
//	primaryApiKey          the key Claude Code uses when nothing higher-priority
//	                       resolves -- its `/login` "managed key" slot
//	customApiKeyResponses  {approved, rejected}; approved holds the LAST TWENTY
//	                       characters of each key the user has agreed to, and an
//	                       interactive Claude Code refuses an ANTHROPIC_API_KEY
//	                       whose suffix is not in it
//	oauthAccount           the profile Claude Code displays for the live OAuth
//	                       login -- accountUuid, emailAddress, and the fields
//	                       around them. Claude Code's own token-refresh handler
//	                       enriches this object's COSMETIC fields but never
//	                       rewrites accountUuid, emailAddress, or
//	                       organizationUuid once one is cached, so a switch
//	                       that only replaces the credentials file leaves this
//	                       naming whoever was live before -- forever, not just
//	                       until the next refresh. See oauthaccount.go.
//
// Everything else in the file is the user's: project history, onboarding state,
// tips, counters. A wholesale replace would destroy all of it, so every write
// here is a read-modify-write of the decoded document with key ORDER preserved.
//
// Order preservation is not tidiness. This file is large and long-lived, people
// keep it in dotfile repositories, and Go's map encoder sorts keys -- so a
// naive re-encode turns a two-key edit into a whole-file diff on every switch.

// maxGlobalConfigSize bounds the read. Unlike the credentials file's 1 MiB cap
// this is NOT a validity rule -- Claude Code imposes no size limit here and a
// real config with a long project history runs to megabytes. It is only a
// runaway guard, set far above any plausible file.
const maxGlobalConfigSize = 64 << 20

// GlobalConfigLockTimeout bounds the wait for the ~/.claude.json lock. Claude
// Code holds it for a read-modify-write of a local file with no network call in
// between, so this outlasts a normal hold by a wide margin.
//
// It is a var so a test can shrink it, exactly as LockTimeout is.
var GlobalConfigLockTimeout = 5 * time.Second

// ErrGlobalConfigShape means ~/.claude.json is not a JSON object. ccdad refuses
// to write over one rather than replacing it: whatever is there is the user's,
// and a file we cannot merge into is a file we cannot edit without loss.
var ErrGlobalConfigShape = errors.New("the Claude Code config is not a JSON object")

// GlobalConfig is a decoded ~/.claude.json that remembers its key order.
//
// Values stay as RawMessage for the same reason Blob's do: everything ccdad
// does not understand has to survive a round trip byte-identical.
type GlobalConfig struct {
	keys   []string
	values map[string]json.RawMessage
}

// NewGlobalConfig returns an empty document, which is what a machine with no
// ~/.claude.json has.
func NewGlobalConfig() *GlobalConfig {
	return &GlobalConfig{values: map[string]json.RawMessage{}}
}

// Get returns a top-level value.
func (g *GlobalConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := g.values[key]
	return v, ok
}

// Set writes a top-level value. An existing key keeps its position and a new
// one is appended, which is what JavaScript's `{...config, key: value}` does
// and therefore what Claude Code's own writes do.
func (g *GlobalConfig) Set(key string, value json.RawMessage) {
	if _, ok := g.values[key]; !ok {
		g.keys = append(g.keys, key)
	}
	g.values[key] = value
}

// Delete removes a top-level key, reporting whether it was there.
func (g *GlobalConfig) Delete(key string) bool {
	if _, ok := g.values[key]; !ok {
		return false
	}
	delete(g.values, key)
	for i, k := range g.keys {
		if k == key {
			g.keys = append(g.keys[:i], g.keys[i+1:]...)
			break
		}
	}
	return true
}

// Keys returns the top-level keys in file order.
func (g *GlobalConfig) Keys() []string {
	return append([]string(nil), g.keys...)
}

// decodeGlobalConfig parses a config document, keeping the order of its
// top-level keys.
//
// json.Decoder's token stream is used rather than json.Unmarshal into a map
// precisely because a map loses that order. A duplicate key -- legal JSON that
// nothing sane writes -- takes the LAST value and its first position, matching
// what a JavaScript object literal does with the same input.
func decodeGlobalConfig(data []byte) (*GlobalConfig, error) {
	g := NewGlobalConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		return g, nil
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parsing the Claude Code config: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, ErrGlobalConfigShape
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parsing the Claude Code config: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, ErrGlobalConfigShape
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("parsing the Claude Code config at %q: %w", key, err)
		}
		g.Set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("parsing the Claude Code config: %w", err)
	}
	return g, nil
}

// encode renders the document the way Claude Code writes it: two-space indent,
// no HTML escaping, no trailing newline -- `JSON.stringify(config, null, 2)`.
//
// It is assembled by hand rather than through a map because the encoder sorts
// map keys; see the file comment.
func (g *GlobalConfig) encode() ([]byte, error) {
	if len(g.keys) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range g.keys {
		name, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.WriteString("  ")
		buf.Write(name)
		buf.WriteString(": ")
		if err := writeIndented(&buf, g.values[k], "  "); err != nil {
			return nil, fmt.Errorf("encoding %q: %w", k, err)
		}
		if i < len(g.keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// writeIndented re-indents one already-encoded value to sit at prefix, which is
// what json.Indent does for everything after the first line.
func writeIndented(buf *bytes.Buffer, raw json.RawMessage, prefix string) error {
	if len(raw) == 0 {
		buf.WriteString("null")
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, prefix, "  "); err != nil {
		return err
	}
	buf.Write(pretty.Bytes())
	return nil
}

// LoadGlobalConfig reads ~/.claude.json. A missing file is an empty document
// and no error: that is a machine Claude Code has never been configured on.
func LoadGlobalConfig() (*GlobalConfig, error) {
	path, err := ccpath.GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	return loadGlobalConfigFrom(path)
}

// LoadGlobalConfigAt reads a named global config -- the `ccdad run
// --full-profile` profile's own, which no environment variable in THIS process
// points at.
func LoadGlobalConfigAt(path string) (*GlobalConfig, error) {
	return loadGlobalConfigFrom(path)
}

func loadGlobalConfigFrom(path string) (*GlobalConfig, error) {
	// Opened WITHOUT the credentials file's O_NOFOLLOW refusal, and that is a
	// deliberate difference rather than an oversight. Claude Code writes this
	// file with `allowSymlink: true` -- it follows the link on purpose -- and a
	// symlinked ~/.claude.json is an ordinary dotfile-manager arrangement, not
	// the redirection hazard a symlinked credential store is.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewGlobalConfig(), nil
		}
		return nil, fmt.Errorf("opening the Claude Code config: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxGlobalConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the Claude Code config: %w", err)
	}
	if int64(len(data)) > maxGlobalConfigSize {
		return nil, fmt.Errorf("%s is over %d bytes, which is not a config file ccdad will rewrite", path, maxGlobalConfigSize)
	}
	return decodeGlobalConfig(data)
}

// UpdateGlobalConfig applies mutate to ~/.claude.json under Claude Code's own
// config lock and writes the result atomically.
//
// The file is read INSIDE the lock, never before it, for the same reason
// Activate re-reads: whatever was loaded before the wait may be stale by the
// time the lock is granted, and Claude Code writes this file constantly --
// every startup counter, every tip shown, every project entry.
//
// mutate returning an error abandons the write and leaves the file untouched.
// So does mutate leaving the document byte-identical: a switch that changes
// nothing must not advance the file's mtime, because Claude Code watches it.
func UpdateGlobalConfig(mutate func(*GlobalConfig) error) error {
	path, err := ccpath.GlobalConfigPath()
	if err != nil {
		return err
	}
	return UpdateGlobalConfigAt(path, mutate)
}

// UpdateGlobalConfigAt is UpdateGlobalConfig against a named config file, for
// the one caller that has a config home no environment variable in this
// process points at: a `ccdad run --full-profile` profile.
//
// It reads the path back out of the lock rather than using its argument, so
// the file that is written and the file that is locked cannot drift apart.
func UpdateGlobalConfigAt(configPath string, mutate func(*GlobalConfig) error) (err error) {
	held, aerr := cclock.AcquireGlobalConfigAt(configPath, GlobalConfigLockTimeout)
	if aerr != nil {
		return globalConfigLockError(aerr)
	}
	// Release's error is joined rather than dropped: its synchronous re-stat is
	// the only check that sees a takeover since the touch goroutine's last tick,
	// and a takeover means Claude Code may have written this file too.
	defer func() { err = errors.Join(err, held.Release()) }()

	path := held.Scope()
	before, err := loadGlobalConfigFrom(path)
	if err != nil {
		return err
	}
	original, err := before.encode()
	if err != nil {
		return err
	}
	if err := mutate(before); err != nil {
		return err
	}
	data, err := before.encode()
	if err != nil {
		return err
	}
	if bytes.Equal(original, data) {
		return nil
	}

	select {
	case <-held.Compromised():
		return fmt.Errorf("aborting the config write: %w", cclock.ErrCompromised)
	default:
	}
	return writeGlobalConfig(path, data)
}

// writeGlobalConfig writes through a symlink rather than over it.
//
// WriteFileAtomic renames a sibling temp file onto the target, which REPLACES a
// symlink with a regular file. That is right for the credentials file, which
// Claude Code refuses to follow at all, and wrong here: Claude Code writes this
// one with `allowSymlink: true`, and a ~/.claude.json symlinked into a dotfiles
// repository is a normal setup that a single ccdad switch would silently
// dismantle.
//
// Resolving the link first and renaming at the RESOLVED path keeps both
// properties -- the link survives because it still points at the file that was
// replaced, and the replacement is still one atomic same-directory rename. It
// is strictly better than Claude Code's own in-place write, which can leave a
// truncated config if the process dies mid-write.
func writeGlobalConfig(path string, data []byte) error {
	// An unresolvable path is the ordinary first-write case -- there is no file
	// yet -- so the error is dropped and the path used as given, which is also
	// what Claude Code's realpath call degrades to.
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
	}
	// 0o600, which is Claude Code's own `mode: 384` on this file.
	return WriteFileAtomic(path, data, 0o600)
}

// globalConfigLockError says which of the two plausible causes it was. A
// contended lock means another Claude Code is mid-write; anything else is a
// filesystem or permission problem and would send a reader looking in the wrong
// place if it were described as contention.
func globalConfigLockError(err error) error {
	if errors.Is(err, cclock.ErrTimeout) {
		return fmt.Errorf("claude code is writing its config; try again in a moment: %w", err)
	}
	return fmt.Errorf("locking the Claude Code config: %w", err)
}

const (
	primaryAPIKeyKey         = "primaryApiKey"
	customAPIKeyResponsesKey = "customApiKeyResponses"
	approvedKey              = "approved"
	rejectedKey              = "rejected"
)

// APIKeyApproval is the value Claude Code stores in customApiKeyResponses to
// stand for a key, and the value it looks the environment's ANTHROPIC_API_KEY
// up by: `Cbe(e){return e.trim().slice(-20)}`, verbatim from 2.1.238.
//
// The LAST twenty characters, not the first. Both halves matter: an entry built
// from the prefix is one Claude Code will never match, so the key would be
// silently rejected at every interactive start.
//
// JavaScript's slice counts UTF-16 code units where this counts bytes. The two
// agree for every real key -- they are ASCII, `sk-ant-api03-...` -- and a value
// where they would disagree is not a Claude API key.
func APIKeyApproval(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 20 {
		return trimmed
	}
	return trimmed[len(trimmed)-20:]
}

// PrimaryAPIKey is the key stored in ~/.claude.json, if any. A value that is
// present but not a JSON string reads as absent rather than as an error: it is
// not a key Claude Code could use either.
func PrimaryAPIKey(g *GlobalConfig) (string, bool) {
	raw, ok := g.Get(primaryAPIKeyKey)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

// ApprovedAPIKeys returns the approval values Claude Code will accept from the
// environment. Entries that are not strings are skipped rather than refused,
// for the same reason PrimaryAPIKey skips a non-string: Claude Code's own
// `includes` would never match them.
func ApprovedAPIKeys(g *GlobalConfig) []string {
	responses, ok := decodeObject(rawOf(g, customAPIKeyResponsesKey))
	if !ok {
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(responses[approvedKey], &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, raw := range list {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func rawOf(g *GlobalConfig, key string) json.RawMessage {
	raw, _ := g.Get(key)
	return raw
}

// SetPrimaryAPIKey installs key as Claude Code's stored API key and adds its
// approval value, mirroring the pair of writes Claude Code's own API-key login
// makes in one transaction.
//
// The approval half is not optional and is not the same mechanism as the first.
// primaryApiKey is what Claude Code falls back to when NOTHING else resolves;
// the approved list is what makes an exported ANTHROPIC_API_KEY acceptable to
// an interactive session. `ccdad run` needs the second, and a user who exports
// the variable by hand needs it too, so both are written together rather than
// leaving one for a later command to remember.
//
// rejected is materialised as an empty array when absent because Claude Code
// materialises it: its writer is
// `{...responses, approved: [...], rejected: responses?.rejected ?? []}`.
func SetPrimaryAPIKey(g *GlobalConfig, key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return errors.New("refusing to store an empty API key")
	}
	encoded, err := marshalNoEscape(trimmed)
	if err != nil {
		return err
	}
	g.Set(primaryAPIKeyKey, encoded)
	return approveAPIKey(g, APIKeyApproval(trimmed))
}

// approveAPIKey adds one approval value, preserving every other sub-key of
// customApiKeyResponses and the order of the list.
func approveAPIKey(g *GlobalConfig, approval string) error {
	responses, err := decodeGlobalConfig(orEmptyObject(rawOf(g, customAPIKeyResponsesKey)))
	if err != nil {
		// A customApiKeyResponses that is not an object is Claude Code's to
		// repair, not ccdad's to overwrite: replacing it would throw away a
		// rejected list we cannot read but the user may still rely on.
		return fmt.Errorf("%s in the Claude Code config cannot be read, so ccdad will not edit it: %w",
			customAPIKeyResponsesKey, err)
	}

	var approved []json.RawMessage
	if raw, ok := responses.Get(approvedKey); ok {
		if err := json.Unmarshal(raw, &approved); err != nil {
			return fmt.Errorf("%s.%s in the Claude Code config is not a list, so ccdad will not edit it",
				customAPIKeyResponsesKey, approvedKey)
		}
	}
	entry, err := marshalNoEscape(approval)
	if err != nil {
		return err
	}
	for _, existing := range approved {
		var s string
		if json.Unmarshal(existing, &s) == nil && s == approval {
			entry = nil
			break
		}
	}
	if entry != nil {
		approved = append(approved, entry)
	}
	encodedApproved, err := marshalNoEscape(approved)
	if err != nil {
		return err
	}
	responses.Set(approvedKey, encodedApproved)
	if _, ok := responses.Get(rejectedKey); !ok {
		responses.Set(rejectedKey, json.RawMessage("[]"))
	}
	encodedResponses, err := responses.encode()
	if err != nil {
		return err
	}
	g.Set(customAPIKeyResponsesKey, encodedResponses)
	return nil
}

func orEmptyObject(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}")
	}
	return raw
}

// ClearPrimaryAPIKey removes the stored key, reporting whether there was one.
//
// The approval entry is deliberately LEFT BEHIND. It is not a credential -- it
// is twenty characters of a key, and its only effect is that an interactive
// Claude Code will accept that key from the environment without prompting.
// Removing it would revoke a consent the USER gave through Claude Code's own
// prompt, on a key that may still be exported in their shell, and the next
// `ccdad switch` back would only have to ask for it again.
func ClearPrimaryAPIKey(g *GlobalConfig) bool {
	return g.Delete(primaryAPIKeyKey)
}
