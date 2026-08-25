package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/mcpsrv"
)

// This file registers ccdad's MCP server with Claude Code by writing Claude
// Code's own configuration.
//
// Why ccdad writes that file at all, rather than shelling out to the `claude`
// command line: shelling out adds a hard runtime dependency on a CLI ccdad
// otherwise never needs, inherits its error-on-a-duplicate behaviour, and hands
// over control of the merge. The file is 195 KB on a real machine, with dozens
// of project entries and live sessions rewriting it, so the merge is the part
// that matters. `--print-config` exists for anyone who refuses on-disk mutation
// on principle.
//
// IT INVENTS NO MECHANISM FOR THE WRITE, and that is the most important thing
// in this file. internal/cclink's global-config writer already takes Claude
// Code's OWN config lock, loads INSIDE it, keeps every key it does not
// understand byte for byte, preserves file order, writes through a symlink
// rather than over it, and declines to touch the mtime when nothing changed. A
// compare-and-swap on mtime and size beside all that would be strictly weaker
// and would make this binary the second writer of one file. So the user scope
// is cclink.UpdateGlobalConfig and nothing more.
//
// The one thing that writer does not know is how to reach INSIDE
// projects["<abs cwd>"], which is where the local scope lives. That nesting is
// done here, on the raw bytes it hands back, with no change to internal/cclink.

// mcpScope is where a registration lives. Three scopes, three different places.
//
// The default is user, and it DIFFERS from `claude mcp add`'s own default of
// local. ccdad is machine-wide, not per-repository, and project is wrong twice
// by default -- it commits ccdad into the user's repository and it sits at
// pending approval until somebody accepts it. The help says the difference out
// loud, because a user who has read Anthropic's documentation will otherwise
// assume they match.
type mcpScope string

const (
	scopeUser    mcpScope = "user"    // top-level mcpServers in .claude.json -- ccdad's DEFAULT
	scopeLocal   mcpScope = "local"   // .claude.json -> projects["<abs cwd>"].mcpServers
	scopeProject mcpScope = "project" // <cwd>/.mcp.json, top-level mcpServers
)

// mcpProjectConfigFile is the project scope's own file, in the directory the
// command was run in. ~/.claude/settings.json is NOT one of these three: it
// holds approval lists and no mcpServers object at all, so a write there would
// put an entry where nothing reads one.
const mcpProjectConfigFile = ".mcp.json"

const mcpServersKey = "mcpServers"

// mcpEntryJSON is one server entry, byte-identical to what Claude Code writes
// itself in all three scopes.
//
//	{"type":"stdio","command":"ccdad","args":["mcp"],"env":{}}
//
// Three things about those bytes are deliberate. env is emitted even when
// empty, and type is written even though it is optional for a stdio server,
// because matching what Claude Code writes means a user diffing this file sees
// nothing unusual. And command is the BARE WORD, never this binary's own path:
// Claude Code de-duplicates plugin and connector servers by ENDPOINT -- the
// command plus its arguments -- so a bare ccdad collapses with the plugin's
// identical entry into one server, while an absolute path leaves both running.
// Measured: same name, different command, two full tool sets and two ways to
// move a credential.
//
// The bare word is spelled out here rather than taken from mcpsrv.ServerName.
// The two happen to agree today and are different things: that constant is the
// KEY this entry is filed under, and this is the executable to run.
func mcpEntryJSON() []byte {
	return []byte(`{"type":"stdio","command":"ccdad","args":["mcp"],"env":{}}`)
}

// mcpConfigJSON is the whole-file shape, and the wrapper is required rather
// than decorative: it is what a project-scope config file and the client's own
// --mcp-config flag both consume.
//
//	{"mcpServers":{"ccdad":{"type":"stdio","command":"ccdad","args":["mcp"],"env":{}}}}
//
// One artifact, three uses: the plugin's server config, --print-config's
// output, and the entry written into all three scopes.
func mcpConfigJSON() []byte {
	return []byte(`{"` + mcpServersKey + `":{"` + mcpsrv.ServerName + `":` + string(mcpEntryJSON()) + `}}`)
}

// mcpInstallRecord is what unwireMCP reads so it does not have to guess which
// file an entry went into.
type mcpInstallRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name"`
	Scope         string `json:"scope"`
	Path          string `json:"path"`
	InstalledAt   string `json:"installedAt"`
}

// mcpRecordFileName is the record's name under CCDAD_HOME. It is ccdad's own
// state, not Claude Code's, which is why it does not live beside the entry.
const mcpRecordFileName = "mcp.json"

// lookPath is exec.LookPath as a package var, beside stdoutIsTTY and
// browserAvailable and for the same reason: whether the bare word `ccdad`
// resolves is a property of the machine a test happens to run on, and both
// answers have to be exercised.
var lookPath = exec.LookPath

func newMCPInstallCmd() *cobra.Command {
	var (
		scope       string
		printConfig bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register ccdad's MCP server with Claude Code",
		Long: "Register ccdad's MCP server with Claude Code, by writing Claude Code's own\n" +
			"configuration.\n\n" +
			"The default scope is 'user': the top-level mcpServers object in .claude.json,\n" +
			"which makes the server available everywhere on this machine. That is NOT the\n" +
			"same default as 'claude mcp add', which registers into the current project\n" +
			"('local'). ccdad manages a machine's logins rather than a repository's, so\n" +
			"machine-wide is the honest default here.\n\n" +
			"  user     .claude.json, top level -- everywhere on this machine (default)\n" +
			"  local    .claude.json, under this directory's project entry -- here only\n" +
			"  project  ./.mcp.json -- committed to the repository, and pending approval\n" +
			"           until each person who checks it out accepts it\n\n" +
			"Running it twice is not an error: an entry that is already correct exits 3 and\n" +
			"changes nothing. An entry pointing somewhere else is rewritten, and both the\n" +
			"old and the new are printed.\n\n" +
			"--print-config writes the configuration to standard output and touches no\n" +
			"file, for anyone who would rather paste it themselves.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printConfig {
				// Nothing is read and nothing is written, which is the point:
				// this half has to work on a machine whose config is
				// unparseable, since that is one of the two situations
				// somebody reaches for it in.
				fmt.Fprintln(cmd.OutOrStdout(), string(mcpConfigJSON()))
				return nil
			}
			sc, err := parseMCPScope(scope)
			if err != nil {
				return err
			}
			return runMCPInstall(cmd, sc)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", string(scopeUser), "where to register: user, local or project")
	// NOT named --json, and the distinction is the JSON contract's: --json
	// changes how a command REPRESENTS its answer, and this changes what the
	// command does. Naming it --json would pull an installer into four
	// contract rules about payload shape that it has no answer for.
	cmd.Flags().BoolVar(&printConfig, "print-config", false, "print the configuration instead of writing it")
	return cmd
}

func newMCPUninstallCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove ccdad's MCP registration from Claude Code",
		Long: "Remove ccdad's MCP registration from Claude Code's configuration.\n\n" +
			"With no --scope it removes the entry from wherever 'ccdad mcp install' put\n" +
			"one, which ccdad recorded at the time. An explicit --scope overrides that and\n" +
			"looks only where it names.\n\n" +
			"For the 'local' scope it looks under THIS directory's project entry, the same\n" +
			"place an install run here would have written.\n\n" +
			"Nothing to remove is exit 3, not a failure.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := parseMCPScope(scope)
			if err != nil {
				return err
			}
			return runMCPUninstall(cmd, sc, cmd.Flags().Changed("scope"))
		},
	}
	cmd.Flags().StringVar(&scope, "scope", string(scopeUser),
		"where to look: user, local or project (default: wherever the install was recorded)")
	return cmd
}

func parseMCPScope(s string) (mcpScope, error) {
	switch mcpScope(s) {
	case scopeUser, scopeLocal, scopeProject:
		return mcpScope(s), nil
	}
	return "", UsageError("unknown --scope %q: one of user, local or project", s)
}

func runMCPInstall(cmd *cobra.Command, scope mcpScope) error {
	path, err := mcpTargetPath(scope)
	if err != nil {
		return err
	}
	entry := json.RawMessage(mcpEntryJSON())

	before, found, err := currentMCPEntry(scope, path)
	if err != nil {
		return err
	}
	if found && sameJSON(before, entry) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s is already registered in %s (%s scope). Nothing to do.\n",
			mcpsrv.ServerName, path, scope)
		return WithCode(errSilent, ExitNothingToDo)
	}
	if err := setMCPEntry(scope, path, entry); err != nil {
		return err
	}

	if found {
		// Printed rather than silently replaced: somebody put a different
		// endpoint there on purpose, and the reason the rewrite happens anyway
		// is that the two cannot both be live once the names match. Telling
		// them what was there is the least this can do.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"%s was registered in %s with a different endpoint; it has been rewritten.\n  was: %s\n  now: %s\n",
			mcpsrv.ServerName, path, compactJSONOrRaw(before), entry)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "Registered %s in %s (%s scope).\n", mcpsrv.ServerName, path, scope)
	}

	if err := saveMCPRecord(mcpInstallRecord{
		SchemaVersion: 1,
		Name:          mcpsrv.ServerName,
		Scope:         string(scope),
		Path:          path,
		InstalledAt:   timeNow().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("registered in %s, but recording where for a later uninstall failed: %w", path, err)
	}

	warnIfCcdadDoesNotResolve(cmd)
	// TODO(P2): call warnPluginCollision(cmd.ErrOrStderr(), installedCcdadPlugins(), scope) here.
	// A file-scope entry silently SUPPRESSES the plugin's server -- the
	// de-duplication is on the endpoint, so renaming changes nothing -- and the
	// detector and its wording belong to the plugin plan, which also adds a
	// doctor row for it. Deliberately not reimplemented here: two copies of one
	// installed_plugins.json reader is the duplication that plan exists to
	// prevent.
	return nil
}

func runMCPUninstall(cmd *cobra.Command, scope mcpScope, explicit bool) error {
	rec, hasRecord, err := loadMCPRecord()
	if err != nil {
		return err
	}
	path := ""
	// The record wins over the DEFAULT and never over an explicit flag: it is
	// what this command knows, and a flag is what the person typing it knows.
	if !explicit && hasRecord {
		if recorded, perr := parseMCPScope(rec.Scope); perr == nil {
			scope, path = recorded, rec.Path
		}
	}
	if path == "" {
		if path, err = mcpTargetPath(scope); err != nil {
			return err
		}
	}

	removed, err := removeMCPEntry(scope, path)
	if err != nil {
		return err
	}
	// The record is deleted whether or not there was an entry, as long as it
	// pointed here: a record naming a file with no entry in it is a lie, and
	// the next uninstall would follow it back to the same empty place.
	if hasRecord && rec.Scope == string(scope) {
		if err := deleteMCPRecord(); err != nil {
			return err
		}
	}
	if !removed {
		fmt.Fprintf(cmd.ErrOrStderr(), "No %s entry in %s (%s scope). Nothing to do.\n",
			mcpsrv.ServerName, path, scope)
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Removed %s from %s (%s scope).\n", mcpsrv.ServerName, path, scope)
	return nil
}

// warnIfCcdadDoesNotResolve says so when the entry is correct and the machine
// is not yet.
//
// The entry names the bare word on purpose, and nothing above this ever checked
// that the bare word RESOLVES. On this project that is not hypothetical:
// install.sh puts the binary in ~/.local/bin and tells the user to run `ccdad
// setup-path` next, so "not on PATH yet" is the ordinary state of a freshly
// installed machine. Without this the failure is invisible from ccdad's own
// side -- the entry is written, the command exits 0 -- and the first anyone
// hears of it is the client failing to start a server it can see.
//
// It is a diagnostic and never a failure: the exit code stays 0, because the
// entry is right even while PATH is not.
func warnIfCcdadDoesNotResolve(cmd *cobra.Command) {
	if _, err := lookPath(mcpsrv.ServerName); err == nil {
		return
	}
	// os.Executable(), never the entry's own bare command: the useful thing to
	// print is where THIS binary is, which is the directory that has to go on
	// PATH.
	where := "the directory holding this binary"
	if self, err := os.Executable(); err == nil {
		where = filepath.Dir(self)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Warning: registered, but `%s` does not resolve on PATH yet. This binary is at\n"+
			"  %s\n"+
			"Run `ccdad setup-path` (or add that directory to PATH by hand) so Claude Code can\n"+
			"actually start the server it just registered.\n",
		mcpsrv.ServerName, where)
}

// mcpTargetPath is the FILE one scope's entry lives in.
func mcpTargetPath(scope mcpScope) (string, error) {
	if scope == scopeProject {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, mcpProjectConfigFile), nil
	}
	return ccpath.GlobalConfigPath()
}

// mcpServerKeys is the chain of object keys from a document's root down to the
// mcpServers object, for one scope.
//
// The local scope's middle key is the ABSOLUTE current directory, which is how
// Claude Code keys its own projects object.
func mcpServerKeys(scope mcpScope) ([]string, error) {
	if scope != scopeLocal {
		return []string{mcpServersKey}, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	return []string{"projects", abs, mcpServersKey}, nil
}

// currentMCPEntry reads the entry already at scope, if there is one. A missing
// file, a missing object anywhere down the chain and a missing entry are all
// "not there" rather than errors: they are the ordinary state of a machine
// nobody has run this on.
func currentMCPEntry(scope mcpScope, path string) (json.RawMessage, bool, error) {
	keys, err := mcpServerKeys(scope)
	if err != nil {
		return nil, false, err
	}
	root, err := loadMCPDocument(scope, path)
	if err != nil {
		return nil, false, err
	}
	servers, ok, err := objectAt(root, keys)
	if err != nil || !ok {
		return nil, false, err
	}
	entry, ok := servers.Get(mcpsrv.ServerName)
	return entry, ok, nil
}

// setMCPEntry writes the entry at scope, creating whatever objects the chain
// needs and preserving the order of everything already there.
func setMCPEntry(scope mcpScope, path string, entry json.RawMessage) error {
	keys, err := mcpServerKeys(scope)
	if err != nil {
		return err
	}
	return updateMCPDocument(scope, path, func(root jsonObject) error {
		return setAt(root, keys, mcpsrv.ServerName, entry)
	})
}

// removeMCPEntry deletes ccdad's entry from one scope's file, reporting whether
// there was one. `ccdad uninstall` calls it with the scope and path out of the
// install record.
//
// For the LOCAL scope it looks under the current directory's project entry --
// the same place an install run here would have written. A local registration
// made in another directory is not found from this one, and that is the
// behaviour rather than a gap being papered over: the record carries the file,
// not the project key, and the alternative -- deleting ccdad out of every
// project in the file -- is a much larger action than the one that was asked
// for.
func removeMCPEntry(scope mcpScope, path string) (bool, error) {
	keys, err := mcpServerKeys(scope)
	if err != nil {
		return false, err
	}
	removed := false
	err = updateMCPDocument(scope, path, func(root jsonObject) error {
		servers, ok, oerr := objectAt(root, keys)
		if oerr != nil || !ok {
			return oerr
		}
		if !servers.Delete(mcpsrv.ServerName) {
			return nil
		}
		removed = true
		// The now-possibly-empty mcpServers object is left in place rather
		// than pruned. `claude mcp remove` leaves one too, and pruning would
		// mean deciding whether ccdad created the object -- which nothing on
		// disk records. The project scope's whole FILE is the one exception,
		// below, because a .mcp.json with nothing in it is litter in somebody's
		// repository rather than a key in their config.
		return setAt(root, keys[:len(keys)-1], mcpServersKey, servers.encode())
	})
	if err != nil || !removed {
		return false, err
	}
	if scope == scopeProject {
		if err := removeEmptyProjectConfig(path); err != nil {
			return true, err
		}
	}
	return true, nil
}

// removeEmptyProjectConfig deletes a .mcp.json that has nothing left in it.
//
// Only when the document is exactly one empty mcpServers object: anything else
// in the file is the user's and the file stays.
func removeEmptyProjectConfig(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	doc, err := decodeOrderedObject(raw)
	if err != nil {
		return nil
	}
	if len(doc.keys) != 1 || doc.keys[0] != mcpServersKey {
		return nil
	}
	servers, err := decodeOrderedObject(doc.values[mcpServersKey])
	if err != nil || len(servers.keys) != 0 {
		return nil
	}
	return os.Remove(path)
}

// loadMCPDocument reads the document one scope lives in, as a root object.
func loadMCPDocument(scope mcpScope, path string) (jsonObject, error) {
	if scope == scopeProject {
		return loadProjectDocument(path)
	}
	return cclink.LoadGlobalConfigAt(path)
}

// updateMCPDocument applies mutate to the document one scope lives in and
// writes it back.
//
// The two branches are different for a reason that is not symmetry: the global
// config has a writer that holds Claude Code's own lock, and .mcp.json has no
// second writer to race.
func updateMCPDocument(scope mcpScope, path string, mutate func(jsonObject) error) error {
	if scope != scopeProject {
		return cclink.UpdateGlobalConfigAt(path, func(g *cclink.GlobalConfig) error {
			return mutate(g)
		})
	}
	obj, err := loadProjectDocument(path)
	if err != nil {
		return err
	}
	before := obj.encode()
	if err := mutate(obj); err != nil {
		return err
	}
	after := obj.encode()
	if bytes.Equal(before, after) {
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, after, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	return cclink.WriteFileAtomic(path, pretty.Bytes(), 0o600)
}

// loadMCPRecord reads the install record. A missing one is "no record" and no
// error: it is what a machine nobody has run the installer on looks like.
func loadMCPRecord() (mcpInstallRecord, bool, error) {
	path, err := mcpRecordPath()
	if err != nil {
		return mcpInstallRecord{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return mcpInstallRecord{}, false, nil
	}
	if err != nil {
		return mcpInstallRecord{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	var rec mcpInstallRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return mcpInstallRecord{}, false, fmt.Errorf("%s is not readable as an install record: %w", path, err)
	}
	return rec, true, nil
}

func saveMCPRecord(rec mcpInstallRecord) error {
	path, err := mcpRecordPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return cclink.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func deleteMCPRecord() error {
	path, err := mcpRecordPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mcpRecordPath() (string, error) {
	home, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, mcpRecordFileName), nil
}

// sameJSON compares two documents by content rather than by bytes.
//
// It has to: the entry this command writes is compact, and the writer that puts
// it into .claude.json re-indents every value it stores. A byte comparison
// would therefore find a difference on the SECOND install, every time, and
// report a rewrite that changed nothing.
func sameJSON(a, b json.RawMessage) bool {
	var ac, bc bytes.Buffer
	if json.Compact(&ac, a) != nil || json.Compact(&bc, b) != nil {
		return false
	}
	return bytes.Equal(ac.Bytes(), bc.Bytes())
}

// compactJSONOrRaw is sameJSON's half for a message: the value on one line, or
// what was there if it will not compact.
func compactJSONOrRaw(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}

// jsonObject is the little that this file needs of a JSON object that
// remembers the order of its keys.
//
// internal/cclink's GlobalConfig is one, for the TOP level of .claude.json, and
// satisfies this by construction. orderedObject below is the same shape for the
// nested levels that writer does not reach.
type jsonObject interface {
	Get(key string) (json.RawMessage, bool)
	Set(key string, value json.RawMessage)
	Delete(key string) bool
}

// orderedObject is a JSON object that keeps its keys in file order.
//
// Order preservation is not tidiness at this depth -- it is the whole reason
// the nesting is done by hand rather than by decoding into a map. `projects`
// holds one entry per directory Claude Code has ever run in, dozens on a real
// machine, and Go's map encoder SORTS. Decoding that object into a map to add
// one server and re-encoding it would rewrite every line of somebody's 195 KB
// dotfile for a one-key edit -- which is exactly what internal/cclink's own
// header says it refuses to do at the top level, for exactly this file.
//
// It lives here rather than in internal/cclink because this is the only caller
// that reaches below the top level, and that package's writer is on the
// credential path.
type orderedObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func newOrderedObject() *orderedObject {
	return &orderedObject{values: map[string]json.RawMessage{}}
}

func (o *orderedObject) Get(key string) (json.RawMessage, bool) {
	v, ok := o.values[key]
	return v, ok
}

// Set keeps an existing key's position and appends a new one, which is what
// JavaScript's object spread does and therefore what Claude Code's own writes
// do.
func (o *orderedObject) Set(key string, value json.RawMessage) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *orderedObject) Delete(key string) bool {
	if _, ok := o.values[key]; !ok {
		return false
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

func (o *orderedObject) encode() json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		// Marshalled rather than quoted by hand: a project path can contain a
		// backslash on Windows and a quote on any platform.
		key, err := json.Marshal(k)
		if err != nil {
			// json.Marshal of a string cannot fail; the branch exists because
			// silently writing a broken key would be worse than an obvious one.
			key = []byte(`""`)
		}
		buf.Write(key)
		buf.WriteByte(':')
		if v := o.values[k]; len(v) > 0 {
			buf.Write(v)
		} else {
			buf.WriteString("null")
		}
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// decodeOrderedObject parses one JSON object, keeping key order.
//
// The token stream rather than json.Unmarshal into a map, for the reason in
// orderedObject's own header. An empty input and a JSON null are both the empty
// object: a key Claude Code wrote as null is a key with nothing in it, and
// refusing there would fail an install on a config that is merely untidy.
func decodeOrderedObject(raw json.RawMessage) (*orderedObject, error) {
	o := newOrderedObject()
	if len(bytes.TrimSpace(raw)) == 0 || isJSONNull(raw) {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object, found %v", tok)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key, found %v", keyTok)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		// A duplicate key -- legal JSON that no encoder produces -- keeps the
		// last value at the first position, which is what a JavaScript parser
		// does with the same bytes.
		o.Set(key, value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// objectAt walks a chain of object keys and returns the object at the end of
// it. A key missing anywhere along the way is "not there" rather than an error,
// because a read must not create what it is looking for.
func objectAt(root jsonObject, chain []string) (*orderedObject, bool, error) {
	if len(chain) == 0 {
		return nil, false, errors.New("objectAt needs at least one key")
	}
	var current jsonObject = root
	var out *orderedObject
	for _, key := range chain {
		raw, ok := current.Get(key)
		if !ok || isJSONNull(raw) {
			return nil, false, nil
		}
		obj, err := decodeOrderedObject(raw)
		if err != nil {
			return nil, false, fmt.Errorf("reading %q out of the configuration: %w", key, err)
		}
		out, current = obj, obj
	}
	return out, true, nil
}

// setAt walks the same chain, CREATING the objects a write needs, sets one key
// at the end of it, and re-encodes back up so every level's order survives.
func setAt(root jsonObject, chain []string, name string, value json.RawMessage) error {
	if len(chain) == 0 {
		root.Set(name, value)
		return nil
	}
	head := chain[0]
	child := newOrderedObject()
	if raw, ok := root.Get(head); ok && !isJSONNull(raw) {
		decoded, err := decodeOrderedObject(raw)
		if err != nil {
			return fmt.Errorf("reading %q out of the configuration: %w", head, err)
		}
		child = decoded
	}
	if err := setAt(child, chain[1:], name, value); err != nil {
		return err
	}
	root.Set(head, child.encode())
	return nil
}

// loadProjectDocument reads a .mcp.json. A missing file is an empty document:
// the project scope creates its own file, unlike the other two.
func loadProjectDocument(path string) (*orderedObject, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newOrderedObject(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	doc, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not a JSON object ccdad will rewrite: %w", path, err)
	}
	return doc, nil
}
