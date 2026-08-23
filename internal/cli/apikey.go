package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// settingsAPIKeyHelper is the settings key naming a command Claude Code runs to
// obtain an API key.
type settingsAPIKeyHelper struct {
	APIKeyHelper string `json:"apiKeyHelper"`
}

// apiKeyHelperConfigured reports whether any settings file Claude Code reads
// configures an apiKeyHelper.
//
// It matters because a configured helper resolves a key ccdad cannot see -- the
// value comes from running the command, which a read-only question must not do
// -- and that key DISPLACES the OAuth login. Without this probe `ccdad which`
// would look at the credentials file, find a managed account, and name it,
// while the session is actually authenticating as whatever the helper prints.
//
// Only the user and project files are read. Claude Code also merges managed
// policy settings from an OS-specific location, and a machine under one is a
// machine where ccdad's account model is not the whole story anyway; missing a
// helper there costs a "not managed" answer that is wrong in the safe
// direction, whereas guessing at the path per platform would be a fact this
// package has not verified.
func apiKeyHelperConfigured() bool {
	var paths []string
	if home, err := ccpath.ConfigHome(); err == nil {
		paths = append(paths, filepath.Join(home, "settings.json"))
	}
	paths = append(paths, projectSettingsFiles()...)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s settingsAPIKeyHelper
		// A settings file that does not parse is Claude Code's problem to
		// report; here it simply carries no helper.
		if json.Unmarshal(data, &s) == nil && s.APIKeyHelper != "" {
			return true
		}
	}
	return false
}

// claudeAPIKeyEnvironment gathers everything identity's model reads.
//
// Interactive is hard-coded true, and the choice is deliberate rather than
// unexamined: ccdad is being asked about a Claude Code that has not started
// yet, so nothing here can know whether the next `claude` will be a terminal
// session or a `claude -p`. Interactive is the stricter of the two -- it is the
// mode that requires an approved key -- and the single case where the two
// answers differ is reported separately through EnvKeyNeedsApproval rather than
// being silently resolved one way.
func claudeAPIKeyEnvironment(cfg *cclink.GlobalConfig) identity.APIKeyEnvironment {
	env := identity.APIKeyEnvironment{
		Bare:              os.Getenv("CLAUDE_CODE_SIMPLE") != "",
		Interactive:       true,
		EnvKey:            os.Getenv("ANTHROPIC_API_KEY"),
		FileDescriptorKey: os.Getenv("CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR") != "",
		Helper:            apiKeyHelperConfigured(),
	}
	if cfg != nil {
		env.Approved = cclink.ApprovedAPIKeys(cfg)
		if managed, ok := cclink.PrimaryAPIKey(cfg); ok {
			env.ManagedKey = managed
		}
	}
	return env
}

// activateAPIKeyAccount makes an API-key account the credential Claude Code
// uses. It is two writes to two files, and the ORDER is the safety property.
//
// The config is written first because the key it installs is INERT while a
// claudeAiOauth record is still in the credentials file: Claude Code's client
// binds `anthropicAuthEnabled: BE()`, and BE() is unaffected by a stored
// primaryApiKey, so the login keeps winning. Only the second write -- removing
// the login -- makes the key the answer.
//
// Interrupted between the two, the machine is still logged in as whatever it
// was, with an unused key stored beside it. The opposite order would leave a
// window with no login and no key, which is a logged-out machine.
func activateAPIKeyAccount(key string) error {
	if err := cclink.UpdateGlobalConfig(func(g *cclink.GlobalConfig) error {
		return cclink.SetPrimaryAPIKey(g, key)
	}); err != nil {
		return err
	}
	return cclink.ClearLogin()
}

// noteCooldown reports an anti-flap stamp that could not be written. It never
// fails the command: the credential is already installed by the time this runs,
// so returning an error here would report a completed switch as a failure.
func noteCooldown(cmd *cobra.Command, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: %v; the daemon may switch again sooner than it should.\n", err)
}

// noteCredentialHomeClaim warns an ATTENDED caller that another ccdad store's
// engine is driving this Claude Code login, and it never refuses.
//
// A human typed the command and is watching the result. The useful thing to
// tell them is that the switch they just made will probably be undone within
// the second — not that ccdad declined to do what they asked. The unattended
// half of the same fact is switcher.Contended, which does stand down, because a
// notice printed to a log nobody reads is not a notice.
//
// Every attended path that writes the live credentials file calls this, and
// there are three: `ccdad switch`, its API-key form, and `ccdad add
// --activate`. The first has the verdict already — Execute decided it under
// Claude Code's credential locks — and the other two write the file without
// ever constructing a switcher.Result, which is why this takes the verdict
// rather than the Result.
func noteCredentialHomeClaim(cmd *cobra.Command, v credhome.Verdict) {
	switch {
	case v.StandDown:
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Note: the ccdad store at %s (pid %d) is also driving this Claude Code login, and will "+
				"probably switch it back. Point CLAUDE_CONFIG_DIR at a directory of this store's own, "+
				"or stop that engine.\n", v.Owner.Store, v.Owner.PID)
	case v.Notice != "":
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", v.Notice)
	}
}

// noteReleasedAPIKey reports what the executor did with a stored key. It never
// fails the command: the login it was clearing the way for is already installed
// by the time this runs.
func noteReleasedAPIKey(cmd *cobra.Command, res switcher.Result) {
	if res.KeyErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: the API key stored in Claude Code's config could not be cleared (%v); "+
				"it stays inert while this login is live, but it becomes the login again if you sign out.\n", res.KeyErr)
		return
	}
	if res.ClearedKey {
		fmt.Fprintf(cmd.ErrOrStderr(), "Removed %s's API key from Claude Code's config.\n", res.ClearedKeyOwner.Label())
	}
}

// displacingAuth names everything Claude Code would use INSTEAD of a
// primaryApiKey ccdad has just installed.
//
// The stored key is the LOWEST-priority source Claude Code has -- `eB()` falls
// back to it only after the environment variable, the file descriptor and the
// helper, and `ua()` puts CLAUDE_CODE_OAUTH_TOKEN ahead of everything by
// producing an OAuth token that keeps `BE()` on. So a machine with any of these
// set can be switched successfully and go on authenticating as something else
// entirely. Saying so at the moment of the switch is the difference between a
// confusing afternoon and a one-line fix.
//
// installed is the key just written, and an ANTHROPIC_API_KEY holding exactly
// that value is not displacing anything -- it is the same account by another
// route.
func displacingAuth(installed string) []string {
	var out []string
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		out = append(out, "ANTHROPIC_AUTH_TOKEN")
	}
	if env := os.Getenv("ANTHROPIC_API_KEY"); env != "" && env != installed {
		out = append(out, "ANTHROPIC_API_KEY")
	}
	if os.Getenv("CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR") != "" {
		out = append(out, "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR")
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		out = append(out, "CLAUDE_CODE_OAUTH_TOKEN")
	}
	if apiKeyHelperConfigured() {
		out = append(out, "the apiKeyHelper setting")
	}
	return out
}

// noteDisplacingAuth warns after a successful API-key switch. It never fails
// the command: the switch happened, and what it reports is the environment, not
// a fault in the switch.
func noteDisplacingAuth(cmd *cobra.Command, installed string) {
	displacing := displacingAuth(installed)
	if len(displacing) == 0 {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Note: Claude Code reads a stored API key LAST, and %s is set — that wins instead.\n"+
			"Unset it for this switch to take effect.\n", strings.Join(displacing, ", "))
}

// switchToAPIKey is the whole switch for an api-key account.
//
// It mirrors the OAuth path step for step -- already-on check, unknown-key
// drift probe, activate, record, report -- so the two cannot drift apart in
// what a switch means.
func switchToAPIKey(cmd *cobra.Command, s *store.Store, target store.Account, key string, live cclink.Blob, force bool) error {
	stderr := cmd.ErrOrStderr()

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		return err
	}
	// "Already on" for an api-key account is BOTH halves of the activation:
	// the key is stored AND no OAuth login is sitting in front of it. With a
	// login still present the key is inert, so reporting "already on" would be
	// reporting a state that is not in effect.
	if !force {
		stored, ok := cclink.PrimaryAPIKey(cfg)
		_, hasOAuth := live["claudeAiOauth"]
		if ok && stored == key && !hasOAuth {
			fmt.Fprintf(stderr, "Already on %s.\n", target.Label())
			return WithCode(errSilent, ExitNothingToDo)
		}
	}

	// The drift probe runs on every switch, and this is one: the credentials
	// file is about to be rewritten -- this path empties it of account-scoped
	// keys -- so the probe belongs here for the same reason it belongs on the
	// OAuth path.
	if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
		fmt.Fprintf(stderr,
			"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
			strings.Join(unknown, ", "))
	}

	// Whether the login about to be removed is one ccdad can put back is
	// decided BEFORE removing it, because afterwards there is nothing left to
	// attribute. A switch between two OAuth accounts destroys an unmanaged live
	// login in exactly the same way, so this is not a new hazard — but there the
	// machine is left holding another login, and here it is left holding none,
	// which makes the difference worth a sentence.
	_, managedLogin := switcher.AttributeFile(live, s.Accounts(), s.Credentials)
	_, hadLogin := live["claudeAiOauth"]

	if err := activateAPIKeyAccount(key); err != nil {
		return err
	}
	if err := s.SetActive(target.UUID); err != nil {
		return err
	}
	noteCooldown(cmd, switcher.RecordSwitch(target.UUID))

	fmt.Fprintf(stderr, "Switched to %s.\n", target.Label())
	switch {
	case !hadLogin:
		// Nothing was removed, so there is nothing to say about getting it back.
	case managedLogin:
		fmt.Fprintln(stderr, "Claude Code's OAuth login was removed so the key is the credential it uses; "+
			"switching back to that account restores it from ccdad's store.")
	default:
		fmt.Fprintln(stderr, "warning: Claude Code's OAuth login was removed so the key is the credential it uses, "+
			"and that login was NOT one ccdad manages — signing in again is the only way back to it.")
	}
	noteDisplacingAuth(cmd, key)
	// Probed rather than carried: this path never constructs a switcher.Result,
	// so there is no verdict decided under the lock to reuse. It runs after the
	// write for the same reason the notes above do — the switch has happened,
	// and this describes what will happen to it.
	noteCredentialHomeClaim(cmd, credhome.Decide())
	return nil
}

// setupTokenRefusal is the message for the one activation that is still not
// possible. Claude Code reads a setup token from the environment ONLY --
// `claude setup-token` prints it and deliberately skips saving it -- so there
// is no file to write, and the mechanism is a child process with the variable
// exported.
func setupTokenRefusal(label string) error {
	return UsageError("%s is a setup-token account, and Claude Code reads a setup token from %s only — "+
		"there is no file to install it into.\n"+
		"Run Claude Code with it exported for the session instead:\n"+
		"  %s=<token> claude",
		label, envVarFor("setup-token"), envVarFor("setup-token"))
}

// noteEnvKeyApproval reports the one state where the answer depends on how
// Claude Code is started.
//
// An ANTHROPIC_API_KEY whose approval value is not in ~/.claude.json is used by
// `claude -p` and refused by an interactive `claude` — the approval list is an
// interactive-mode gate (`eB()` takes the environment key outright when
// `hIr()`, which is `!isInteractive()`). ccdad is answering about a session
// that has not started, so it cannot pick between the two and says so instead.
func noteEnvKeyApproval(cmd *cobra.Command, env identity.APIKeyEnvironment) {
	if !env.EnvKeyNeedsApproval() {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"note: ANTHROPIC_API_KEY is set but is not in Claude Code's approved list, so it is used by "+
			"'claude -p' and refused by an interactive session. This answer is for the interactive case.")
}

// attributeLive is switcher.AttributeLogin for the callers that want the account and
// have no place to report the mechanism — `list` marking a row active, and
// `disable` noticing it is talking about the live one.
//
// A config that cannot be read is not fatal to the question. It costs the two
// inputs that come out of ~/.claude.json, and the environment axes — the ones
// that override a login rather than merely backing it up — still answer. The
// commands that can report a degraded read do so themselves; these two would
// have nowhere to put the sentence.
func attributeLive(live cclink.Blob, accounts []store.Account,
	lookup func(uuid string) (cclink.Blob, error)) (store.Account, bool) {
	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		cfg = nil
	}
	res := switcher.AttributeLogin(live, accounts, lookup, claudeAPIKeyEnvironment(cfg))
	return res.Account, res.OK
}

// projectSettingsFiles names the per-project settings Claude Code merges,
// relative to the working directory.
//
// It is a seam because those two paths are the one input to this package that
// no environment variable can sandbox: `go test` runs with the package
// directory as its working directory, and a repository that happens to contain
// a .claude/settings.json would change what the tests measure. isolate() empties
// it, and the test that means to describe a configured helper writes one.
var projectSettingsFiles = func() []string {
	return []string{
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "settings.local.json"),
	}
}
