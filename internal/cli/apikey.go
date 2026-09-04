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
		// TruthyEnv rather than != "": Claude Code parses CLAUDE_CODE_SIMPLE
		// with a four-spelling truthiness test, so CLAUDE_CODE_SIMPLE=0 put
		// ccdad in bare mode -- where the answer is "no key" -- while Claude
		// Code was resolving one normally.
		Bare:        identity.TruthyEnv(os.Getenv("CLAUDE_CODE_SIMPLE")),
		Interactive: true,
		EnvKey:      os.Getenv("ANTHROPIC_API_KEY"),
		// TWO ROUTES INTO ONE BRANCH. The descriptor variable is what ccdad
		// modelled; the well-known file is the other half, and Claude Code
		// reads it whenever the variable is unset. A machine with the file and
		// no variable had a key that displaces the login and a report that said
		// nothing about it.
		FileDescriptorKey: os.Getenv("CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR") != "" ||
			identity.HostAPIKeyFilePresent(),
		Helper: apiKeyHelperConfigured(),
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
// there are three: `ccdad switch`, its API-key form, and `ccdad add claude
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

// noteProfileSync reports a failure to keep ~/.claude.json's displayed
// account name aligned with the login that was just installed. It never fails
// the command: the credentials file already reflects the switch by the time
// this runs, so the account displayed by Claude Code catching up is a
// courtesy, not the thing that just succeeded or failed.
func noteProfileSync(cmd *cobra.Command, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: %v; Claude Code's displayed account name may still name the previous login.\n", err)
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

// claudeOAuthEnvironment is the OAuth axis for this process, with the one field
// the probe leaves to its caller filled in.
//
// The apiKeyHelper lives in Claude Code's settings tree, whose project half
// resolves against the working directory and already has a reader here. A
// second probe inside internal/identity would be a second answer that can
// disagree with the first.
func claudeOAuthEnvironment() identity.OAuthEnvironment {
	env := identity.ProbeOAuthEnvironment()
	env.Helper = apiKeyHelperConfigured()
	return env
}

// displacingSource is one credential Claude Code would use INSTEAD of a
// primaryApiKey ccdad has just installed, with the remedy that actually applies
// to it.
//
// It carries a remedy per source because the single template this used to print
// said "Unset it", which is wrong for a file, wrong for a settings key and
// wrong for an Anthropic CLI profile. Claude Code ships a per-source table and
// ccdad mirrors it rather than inventing a second set of instructions for the
// same user.
type displacingSource struct {
	// Key is what dedupe compares, and it is NOT Name. One thing -- the
	// apiKeyHelper -- resolves on BOTH axes, and the two axes spell it
	// differently ("apiKeyHelper" against "the apiKeyHelper command"), so a
	// dedupe on the printed name never fired for the single case it exists for.
	Key    string
	Name   string
	Remedy string
}

// displacingAuth names everything Claude Code would use INSTEAD of a
// primaryApiKey ccdad has just installed.
//
// The stored key is the LOWEST-priority source Claude Code has -- `eB()` falls
// back to it only after the environment variable, the host-injected key and the
// helper -- and the whole OAuth axis sits in front of it as well. So a machine
// with any of these can be switched successfully and go on authenticating as
// something else entirely. Saying so at the moment of the switch is the
// difference between a confusing afternoon and a one-line fix.
//
// IT IS TWO RESOLVERS NOW, NOT A LIST OF os.Getenv CALLS, and that is what the
// item behind this change turned up: of the two sources it named, one is an
// environment variable and the other is a FILE with no variable behind it. A
// list can only name things that have names to unset.
//
// The OAuth axis is resolved against an EMPTY login, because the activation
// this note follows has just cleared the login: anything the resolver still
// names is something that outranks the key just written.
//
// installed is that key, and an ANTHROPIC_API_KEY holding exactly that value is
// not displacing anything -- it is the same account by another route.
func displacingAuth(installed string) []displacingSource {
	var out []displacingSource
	add := func(key, name, remedy string) {
		for _, seen := range out {
			if seen.Key == key {
				return
			}
		}
		out = append(out, displacingSource{key, name, remedy})
	}

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		cfg = nil
	}
	env := claudeAPIKeyEnvironment(cfg)
	if key, source := env.Resolve(); source.DisplacesOAuth() && key != installed {
		add("apikey:"+source.String(), source.String(), source.Remedy())
	}

	// AN UNAPPROVED ANTHROPIC_API_KEY IS STILL A DISPLACER, and reporting only
	// the winner lost it. claudeAPIKeyEnvironment asks the INTERACTIVE
	// question, where an unapproved key falls through to every lower source --
	// but `claude -p` takes it outright, ahead of the very key this note
	// follows. The old list named the variable whenever it was set, and dropping
	// that was a regression rather than a simplification.
	if env.EnvKeyNeedsApproval() && strings.TrimSpace(env.EnvKey) != installed {
		add("apikey:"+identity.APIKeyEnv.String(),
			identity.APIKeyEnv.String()+" (which an interactive session ignores until you approve it, and "+
				"`claude -p` uses outright)", identity.APIKeyEnv.Remedy())
	}

	source, ok := claudeOAuthEnvironment().Resolve(identity.Login{})
	switch {
	case !ok:
		add("oauth:snapshot",
			"a credential ccdad cannot resolve (CLAUDE_BG_AUTH_SNAPSHOT_PATH names a token snapshot "+
				"Claude Code consumes before it looks at anything else)", "Check the host session that set it.")
	case source == identity.OAuthHelper:
		// The one source that reaches both resolvers, and the reason Key exists.
		add("apikey:"+identity.APIKeyHelper.String(), source.String(), source.Remedy())
	case source != identity.OAuthNone && source != identity.OAuthLogin:
		add("oauth:"+source.SourceName(), source.String(), source.Remedy())
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
	names := make([]string, 0, len(displacing))
	for _, d := range displacing {
		names = append(names, d.Name)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Note: Claude Code reads a stored API key LAST, and %s resolves ahead of it — that wins instead.\n",
		joinAnd(names))
	for _, d := range displacing {
		if d.Remedy != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", d.Remedy)
		}
	}
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
	// SourceNone, and it is not a claim about where live came from: this helper
	// returns the ACCOUNT and drops the Via, so the store's name never reaches a
	// user through here. Commands that DO report a source read it themselves --
	// `which` takes it from cclink.LoadWithSource alongside the blob. If this
	// ever starts returning res.Via, it has to take the source too.
	res := switcher.AttributeLogin(live, accounts, lookup,
		claudeAPIKeyEnvironment(cfg), claudeOAuthEnvironment(), cclink.SourceNone)
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
