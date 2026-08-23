package identity

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// This file fills OAuthEnvironment from the machine. It stats and reads; it
// creates nothing, and it never opens a credential.

// HostOAuthTokenFile and HostAPIKeyFile are the two paths Claude Code compiles
// in as literals -- one base directory (grep: `/home/claude/.claude/remote`)
// with a token file and a key file under it. They are not derived from a home
// directory, and no CLAUDE_CONFIG_DIR or CLAUDE_SECURESTORAGE_CONFIG_DIR moves
// them.
//
// They are VARS rather than consts, and the reason is sharper than the usual
// one: they are absolute and outside the home directory, so no t.Setenv can
// sandbox them. Without this seam a test suite's answer depends on whether the
// machine running it happens to have a /home/claude, and that failure reads as
// a flake rather than as an unsandboxed input. Exported because more than one
// package has to neutralise them.
var (
	HostOAuthTokenFile = "/home/claude/.claude/remote/.oauth_token"
	HostAPIKeyFile     = "/home/claude/.claude/remote/.api_key"
)

// maxProfileConfig caps a profile config read, the same refusal cclink applies
// to the credentials file: a path that happened to name a huge file is not
// something a diagnostic should read into memory.
const maxProfileConfig = 1 << 20

// tokenFilePresent reports that a well-known TOKEN file would give Claude Code
// a credential: a regular file with bytes in it.
//
// A STAT, NEVER A READ, and this is the whole discipline of this file: both
// well-known paths hold live bearer tokens. Non-empty is the closest a stat
// gets to Claude Code's own reader, which reads the file and TRIMS, returning
// null when what is left is empty -- so a whitespace-only file is nothing to
// Claude Code and present here. That over-reports, which is the safe direction:
// ccdad says a credential may outrank the login when it would not.
//
// AN UNREADABLE PATH IS ABSENT, and that is faithfulness rather than a
// judgement call. Claude Code's reader is a readFileSync in a try/catch that
// returns null for the whole error class -- so a /home/claude it cannot read
// gives IT no credential either, and "absent" is what that machine actually
// resolves to. The consequence of guessing the other way is not a softer
// warning: HostTokenFile feeds the daemon's stand-down, so an unrelated local
// user named `claude` with a mode-700 home would stop the engine switching
// forever, on a machine where Claude Code is reading nothing of the sort.
func tokenFilePresent(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > 0
}

// profileCredentialsPresent reports that an Anthropic CLI profile's credentials
// file is there at all, WITHOUT the non-empty test the token files get.
//
// Claude Code's gate here is different, and the difference matters in the
// direction that would hide a source: Wra() keeps a user_oauth profile when its
// reader returns anything but null, and that reader returns the file's CONTENTS
// without trimming -- so a ZERO-BYTE credentials file yields "" and the profile
// STANDS. Requiring bytes here would drop a profile Claude Code is using and
// report the login as the winner instead, which is the one direction this
// package's approximations are not allowed to err in.
//
// The residual divergence is the unreadable file: Claude Code's reader swallows
// a set of errnos and returns null (no profile) and rethrows the rest out of
// the branch entirely. ccdad stats, so an EACCES here reads as present. That
// over-reports a profile, which is the safe direction, and the alternative is
// opening another program's credential file to answer a presence question.
func profileCredentialsPresent(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

// HostAPIKeyFilePresent is the API-KEY half of the same well-known-file rule,
// and it lives here because it is the same mechanism read from the same base
// directory.
//
// Claude Code's API-key resolver eB() calls DDn(), and DDn() is the same reader
// as mbe() with a different path and a different cache (grep: `wellKnownPath:`)
// -- so the descriptor variable and this file are two ways into ONE branch, and
// Claude Code reports both under the source name ANTHROPIC_API_KEY. ccdad
// modelled only the variable, which meant a machine with this file and no
// variable had a key that displaces the login and a report that said nothing.
//
// It is a bare function rather than a field of OAuthEnvironment because it
// belongs to the other axis; APIKeyEnvironment.FileDescriptorKey is where it
// lands.
func HostAPIKeyFilePresent() bool { return tokenFilePresent(HostAPIKeyFile) }

// ProbeOAuthEnvironment fills everything BT() reads EXCEPT Helper, from this
// process's environment and three stats.
//
// Helper is left false for the caller to fill. The apiKeyHelper lives in Claude
// Code's settings tree, whose project half resolves against the working
// directory and already has a reader in internal/cli; a second probe here would
// be a second answer that can disagree with the first. The consequence is named
// rather than hidden: a caller that cannot reach that reader does not see a
// configured helper, which is what it saw before this file existed.
func ProbeOAuthEnvironment() OAuthEnvironment {
	return OAuthEnvironment{
		Bare:            TruthyEnv(os.Getenv("CLAUDE_CODE_SIMPLE")),
		AuthToken:       envPresent("ANTHROPIC_AUTH_TOKEN"),
		TokenEnv:        strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")),
		TokenDescriptor: envPresent("CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"),
		HostTokenFile:   tokenFilePresent(HostOAuthTokenFile),
		BgSnapshot:      tokenFilePresent(os.Getenv("CLAUDE_BG_AUTH_SNAPSHOT_PATH")),
		Host: HostContext{
			// TruthyEnv, NOT presence. Claude Code does not read the raw
			// environment: it reads through a typed accessor, and this variable
			// is declared bool there -- the same Un() parser CLAUDE_CODE_SIMPLE
			// gets. CLAUDE_CODE_REMOTE=0 is therefore NOT hosted, and reading it
			// as presence errs in the one unsafe direction this axis has:
			// believing the session is hosted SUPPRESSES ANTHROPIC_AUTH_TOKEN,
			// suppresses the helper and disqualifies the profile, so ccdad would
			// report the login as the winner while one of them is deciding the
			// session. Claude Code's own writer for that accessor stores a false
			// boolean as the string "0", so the value is not hypothetical.
			Remote:         TruthyEnv(os.Getenv("CLAUDE_CODE_REMOTE")),
			Entrypoint:     strings.TrimSpace(os.Getenv("CLAUDE_CODE_ENTRYPOINT")),
			HostAuthEnvVar: envPresent("CLAUDE_CODE_HOST_AUTH_ENV_VAR"),
		},
		Profile: probeAntProfile(),
	}
}

// antConfigDir is Kra() (grep: `.config","anthropic"`):
//
//	ANTHROPIC_CONFIG_DIR ?? (XDG_CONFIG_HOME ? <that>/anthropic
//	                       : HOME ? <HOME>/.config/anthropic : null)
//
// $HOME and not os.UserHomeDir, deliberately: Kra reads process.env.HOME with
// no fallback of its own, so on a Windows machine with HOME unset there is no
// Anthropic config directory as far as Claude Code is concerned, and inventing
// %USERPROFILE% here would model a directory it never looks in.
func antConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_CONFIG_DIR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(v, "anthropic")
	}
	if v := strings.TrimSpace(os.Getenv("HOME")); v != "" {
		return filepath.Join(v, ".config", "anthropic")
	}
	return ""
}

// federationVars is the pair Ivb() requires TOGETHER for the "env-quad"
// precedence (grep: `ANTHROPIC_FEDERATION_RULE_ID`). Both, or neither counts.
var federationVars = []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID"}

// probeAntProfile is Ivb() for the precedence and Rvb() for the auth type,
// which are two functions over one walk -- so this does the walk once and
// returns both.
//
// The precedence rules, in Ivb()'s order:
//
//   - ANTHROPIC_PROFILE set: with no config dir the answer is "no profile", and
//     it STOPS there rather than falling through to the federation pair. Then
//     the named config's auth type must be one of the two recognised values, or
//     again "no profile" and stop.
//   - both federation variables set and non-blank: "env-quad", whose auth type
//     is oidc_federation with no file read at all.
//   - a config dir exists: the profile named by <dir>/active_config, or
//     "default", and again its type must be one of the two.
func probeAntProfile() AntProfile {
	dir := antConfigDir()

	if name := strings.TrimSpace(os.Getenv("ANTHROPIC_PROFILE")); name != "" {
		if dir == "" {
			return AntProfile{}
		}
		if kind, ok := antAuthType(dir, name); ok {
			return AntProfile{Precedence: "profile-explicit", AuthType: kind}
		}
		return AntProfile{}
	}

	federated := true
	for _, v := range federationVars {
		if strings.TrimSpace(os.Getenv(v)) == "" {
			federated = false
			break
		}
	}
	if federated {
		return AntProfile{Precedence: "env-quad", AuthType: "oidc_federation"}
	}

	if dir == "" {
		return AntProfile{}
	}
	if kind, ok := antAuthType(dir, activeAntProfileName(dir)); ok {
		return AntProfile{Precedence: "profile-implicit", AuthType: kind}
	}
	return AntProfile{}
}

// activeAntProfileName is Vra(): the trimmed contents of <dir>/active_config,
// or "default" when that file is absent or blank.
func activeAntProfileName(dir string) string {
	body, err := readSmallFile(filepath.Join(dir, "active_config"))
	if err != nil {
		return "default"
	}
	if name := strings.TrimSpace(body); name != "" {
		return name
	}
	return "default"
}

// antAuthType is Wra(): read <dir>/configs/<name>.json, take
// authentication.type, and for user_oauth ALSO require the profile's
// credentials file to be there.
//
// ONE DELIBERATE DIVERGENCE, and it is the only place this file departs from
// the bundle. Wra tests that the credentials file is READABLE -- its reader
// swallows a set of errnos and rethrows the rest -- while this stats it. The
// divergence costs a profile reported on a machine where Claude Code would fall
// through to the login, which over-reports in the same safe direction as
// everything else here, and the alternative is opening another program's
// credential file to answer a presence question.
//
// The config file itself IS read, and that is not the same call: it holds a
// type name, not a credential, and doctor already reads Claude Code's own
// config for the api-key row.
func antAuthType(dir, name string) (string, bool) {
	body, err := readSmallFile(filepath.Join(dir, "configs", name+".json"))
	if err != nil {
		return "", false
	}
	var config struct {
		Authentication struct {
			Type            string `json:"type"`
			CredentialsPath string `json:"credentials_path"`
		} `json:"authentication"`
	}
	if err := json.Unmarshal([]byte(body), &config); err != nil {
		return "", false
	}
	kind := config.Authentication.Type
	if kind == "user_oauth" {
		// Lvb(): the profile may name its own credentials path, and the
		// default is <dir>/credentials/<name>.json.
		path := config.Authentication.CredentialsPath
		if path == "" {
			path = filepath.Join(dir, "credentials", name+".json")
		}
		if !profileCredentialsPresent(path) {
			return "", false
		}
	}
	if kind == "oidc_federation" || kind == "user_oauth" {
		return kind, true
	}
	return "", false
}

// readSmallFile reads a capped amount of a text file. An unreadable file and an
// absent one are one answer here, exactly as they are to the reader this
// mirrors.
func readSmallFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProfileConfig))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// AuthEnvironmentVars is every environment variable the two resolvers in this
// package read. It exists for ONE purpose: a test suite that means to describe
// a machine has to neutralise all of them, and three packages now reach a
// resolver -- internal/cli, internal/switcher and internal/daemon.
//
// A list copied into three isolate helpers drifts, and the drift is silent:
// the suite keeps passing on CI, where none of these are set, and fails only on
// the shell of whoever exports one. This is that list in one place, so adding a
// variable to a resolver adds it to every sandbox at once.
//
// It does NOT include the two well-known FILE paths. Those are HostOAuthTokenFile
// and HostAPIKeyFile, they are package vars precisely because no environment
// variable can move them, and a sandbox has to repoint them separately.
func AuthEnvironmentVars() []string {
	return []string{
		// The api-key axis.
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
		// Read by both.
		"CLAUDE_CODE_SIMPLE",
		// The OAuth axis.
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
		"CLAUDE_BG_AUTH_SNAPSHOT_PATH",
		// Whether Claude Code believes it is inside a session host, which
		// INVERTS two branches of the OAuth axis.
		"CLAUDE_CODE_REMOTE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_HOST_AUTH_ENV_VAR",
		// The Anthropic CLI's profile walk.
		"XDG_CONFIG_HOME",
		"ANTHROPIC_CONFIG_DIR",
		"ANTHROPIC_PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID",
		"ANTHROPIC_ORGANIZATION_ID",
	}
}
