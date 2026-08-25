package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// Auto-starting a daemon from any ccdad command when none is up is the feature
// — automatic switching with nothing for the user to run is the whole point —
// and the most dangerous thing in this program. It puts a process spawn on the
// hot path of every invocation, including completion scripts and `go test`.
//
// Five rules hold it down, and each of them exists because of a specific way
// this goes wrong:
//
//  1. An ALLOW-LIST, never a default-on hook. A command added later defaults to
//     safe rather than to spawning, and every entry below had to be argued for.
//  2. A recursion guard with two halves. The child is itself `ccdad
//     <something>`, so one missing guard spawns as fast as the operating system
//     allows: the hidden `__daemon` entrypoint is not on the allow-list, and
//     daemon.ChildEnvVar marks every process ccdad started, whatever it runs.
//  3. Never into a credential environment scoped to one terminal. That is TWO
//     checks below rather than one, and reading only the first is how a reader
//     concludes this hole is still open: CLAUDE_SECURESTORAGE_CONFIG_DIR being
//     DEFINED, and — because `ccdad run --full-profile` REMOVES that variable
//     and scopes with CLAUDE_CONFIG_DIR instead — a config home inside this
//     store's own sessions or profiles container, which scoped.go answers.
//  4. Silent, and never a failure. One stray line on stdout breaks `ccdad list
//     --json | jq`, and a daemon that will not start is a degraded mode rather
//     than an error for a command that was not asking for one.
//  5. Never into a credential home another store's engine already holds. A
//     daemon refused there releases the singleton on its way out, so rule 2's
//     probe stays negative and every allow-listed command below forks a child
//     that dies at once — the same unbounded spawn rule 2 exists to stop,
//     reached by a different door.
//
// The suppression under `go test` is the fourth thing, and it is a hard
// requirement rather than a convenience: an unsuppressed spawn detaches a real
// daemon pinned to a t.TempDir() that is about to be deleted underneath it,
// holding a lock in a directory that no longer exists. internal/cli's isolate
// helper replaces the hook for the whole suite; the tests that exercise the
// policy put it back by name.

// autoStartCommands is the allow-list, keyed by the full command path so that
// `ccdad status` and `ccdad daemon status` cannot be confused for each other —
// they share a name and sit at opposite ends of this decision.
//
// What is here: the commands a user runs while USING their accounts, where an
// engine that is not running is the thing they are about to be surprised by.
//
// What is deliberately not here, with the reason, because these are the entries
// a later reader will want to add:
//
//   - the whole `daemon` group. `status` is a probe, and a probe that starts
//     what it probes can never answer 5 — the entire exit contract goes with
//     it. `stop` would resurrect exactly what it just stopped. `start` and
//     `restart` do their own spawning, with a wait auto-start deliberately does
//     not have.
//   - `doctor`, which must not create what it is checking for. The singleton
//     probe holds the same line about the lock file, and doctor.go repeats it
//     for the store directory.
//   - `completion`, which is a daemon per TAB press.
//   - `remove`, `export`, `import`, `config`, `uninstall` and `update`:
//     administering ccdad is not using it, and `uninstall` and `update` in
//     particular would start the process they are about to stop.
//   - `primary`, for the same reason: setting an account's money policy is
//     administering ccdad rather than using it, and the engine reads the flag
//     on its next tick either way, whether or not one is already running.
//   - `hover`, because this map is keyed by command path — listing it would
//     auto-start a daemon for the verb that turns the mode off, and `ccdad
//     hover off` is the one moment a stray daemon is least welcome.
//   - `probe`, which is the engine's own errand rather than a user using their
//     accounts, and which a daemon RE-EXECS. An entry here would leave the
//     recursion guard as the only thing between a probe and an unbounded spawn,
//     and it would mean a `ccdad probe --all` on a machine with no engine spent
//     quota and left a process behind for it.
//   - `bootstrap`, which a container entrypoint runs before it starts the
//     daemon itself. An entry here would spawn an engine over a store that has
//     not finished importing — or, off a container, one nobody asked to run —
//     and it would then race the entrypoint's own `daemon start` for the
//     singleton a moment later.
//   - `setup-path`, which runs when `ccdad` does not resolve yet. Spawning a
//     daemon from the command whose whole job is to make the NEXT terminal find
//     the binary starts an engine for a machine that has not finished being set
//     up, and it is the one command a user may run before they have added a
//     single account.
//   - `auto`, which IS the engine. Starting a daemon for it would hand the
//     singleton to the daemon and make the continuous form refuse itself, and
//     `auto --once` exists precisely so the engine can be run WITHOUT one.
//   - `mcp`, which the client starts and restarts on its own schedule. An entry
//     here would spawn a daemon every time that happened, before any tool was
//     called — and the tools that DO auto-start reach the hook through the
//     command tree anyway, one fresh root per call, so listing the server buys
//     nothing and spends an engine on a session that may never ask for one.
//
// `ccdad tui` is here, and that is a decision MADE rather than one made by
// omission: this map has no totality test, so an unlisted command silently
// never spawns and nothing in the tree ever says the question was asked. The
// dashboard is `ccdad status` under another name, `status` is above, and the
// person opening it is doing exactly what the paragraph at the top of this
// list describes -- looking at an engine that is not running. The
// counter-argument is real and is written here rather than left in nobody's
// head: off a terminal `ccdad tui > file` renders once and exits 0, and the
// hook fires before that render, so a redirected invocation spawns a daemon
// too. That is the shape `ccdad status > file` already has, and it is accepted
// for the same reason.
//
// Bare `ccdad` is the one entry that is here and is NOT started by the hook.
// That slot is a dashboard behind a TTY gate and a usage error otherwise, and
// only the dashboard half wants a daemon: a hook firing before the gate would
// spawn one for `ccdad | head` as well. root.PersistentPreRun therefore skips
// the bare root and runBare calls the hook itself, which is also why deleting
// the entry below does not merely narrow the policy — it makes the dashboard
// the one place in the tree that asks for a daemon and is refused.
var autoStartCommands = map[string]bool{
	"ccdad":           true,
	"ccdad add":       true,
	"ccdad add-token": true,
	"ccdad list":      true,
	"ccdad status":    true,
	"ccdad tui":       true,
	"ccdad switch":    true,
	"ccdad which":     true,
}

// autoStart is the hook the root runs before every command. It is a package var
// so the test suite can replace it with a no-op; production never reassigns it.
var autoStart = maybeAutoStart

// maybeAutoStart starts a daemon if this command may have one and none is
// running.
//
// It returns nothing, and that is the signature doing the work: rule 4 says
// auto-start must never fail the command, and a function that cannot report an
// error cannot be wired into one by a later change.
//
// It does not WAIT for the daemon to take the singleton, unlike `ccdad daemon
// start`. That command's caller asked for a daemon and wants to know it is
// there; every other command is doing something else, and the latency of a
// process reaching its first lock has no business on the hot path of the whole
// tree. The consequence is honest: the command that triggered the start
// generally does not benefit from it, the next one does.
func maybeAutoStart(cmd *cobra.Command) {
	// The recursion guard first, because it is the one whose failure is
	// unbounded. Everything below it is a decision; this is a fuse.
	if os.Getenv(daemon.ChildEnvVar) != "" {
		return
	}
	if !autoStartCommands[cmd.CommandPath()] {
		return
	}
	// CLAUDE_SECURESTORAGE_CONFIG_DIR is how a credential home is scoped to one
	// terminal, and ccdad itself sets it that way. A daemon auto-started from
	// inside such a shell would manage THAT shell's credentials for the rest of
	// its life, silently — and pinning the resolved path into the child only
	// makes it permanent rather than preventing it. DEFINED is the test, not
	// non-empty: Claude Code reads a defined-but-empty value as ~/.claude
	// rather than as the config home, which is a different file again.
	if _, scoped := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); scoped {
		return
	}
	// The half the test above cannot see. `ccdad run --full-profile` REMOVES
	// that variable rather than emptying it and scopes with CLAUDE_CONFIG_DIR
	// instead, so a session in that mode reads as an unscoped shell here —
	// while ChildEnv would pin the profile into the daemon exactly the same
	// way. CLAUDE_CONFIG_DIR on its own is NOT a reason to refuse: it is where
	// a great many people keep their Claude Code configuration, and refusing
	// there would turn auto-start off for all of them. Only a config home
	// ccdad created for a run counts, which is what scoped.go answers.
	if _, session := currentScopedSession(); session {
		return
	}
	held, err := singletonHeld()
	if err != nil || held {
		// A lock that cannot be probed is not an invitation. This is the same
		// rule the daemon group is built on, on the hottest path there is:
		// treating "cannot determine" as "not running" starts a daemon per
		// invocation forever on a filesystem where locks do not work.
		return
	}
	// Rule 5, and it is the same shape as rule 2: a fuse against an unbounded
	// spawn rather than a policy decision.
	//
	// A daemon refused by the credential-home claim gives the SINGLETON back on
	// its way out — the defer that releases it runs — so the probe above stays
	// negative forever. Without this, every one of the allow-listed commands
	// above forks a child that dies immediately, on every invocation, silently.
	//
	// Held-but-not-ours is the whole test, named or not: credhome.Acquire
	// refuses on a claim it cannot attribute exactly as it refuses on one it
	// can, because the lock is held either way. A probe that could not ANSWER is
	// different and does not stop the spawn: the daemon degrades and keeps
	// running in that case, so it takes the singleton and ends the loop itself.
	if claim, cerr := credentialHomeClaim(); cerr == nil && claim.Held && !claim.Ours {
		return
	}
	// The error is discarded, not ignored: rule 4. `ccdad doctor` and `ccdad
	// status` are where a daemon that will not start is reported, and both say
	// so from evidence rather than from a message this path could have printed.
	_ = spawnDaemon("")
}
