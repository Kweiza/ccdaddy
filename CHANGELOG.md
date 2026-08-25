# Changelog

Notable changes, in the format of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
following [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Per-release notes are also generated on the
[releases page](https://github.com/Kweiza/ccdaddy/releases) from the commit
history; this file is the shorter, human-grouped view, and it is where anything
that would surprise an upgrader gets written down.

While the version is below `1.0.0`, the CLI surface may change between minor
versions. The one thing that is already a promise is the stability contract
`ccdad --help` prints: **`idx` is a display ordinal, not a key.** It is
recompacted whenever an account is removed, so scripts must reference accounts
by `uuid` or `alias`.

## [Unreleased]

### Added

- **`ccdad tui`, the interactive dashboard**, and bare `ccdad` now opens it at a
  terminal instead of printing the status dashboard. That is a widening rather
  than a break: off a terminal the bare slot still refuses with usage on stderr
  and exit `2`, exactly as it always has, so no script can have depended on what
  it did. Six keys, and every one that changes something runs the ordinary
  command for it — same refusals, same wording, same exit codes. It never
  fetches; the page is read from disk.
- **`ccdad mcp`, an MCP server for Claude Code**, with fifteen tools in four
  classes: five reads, five that write ccdad's own account file, four that drive
  the daemon, and `switch`, which rewrites the live login and asks the person at
  the keyboard first. Eight ccdad verbs are deliberately not tools, and a
  handler registered under any of their names is refused before it runs.
  `ccdad mcp install` registers the server with Claude Code — three scopes,
  defaulting to `user`, which is **not** `claude mcp add`'s own default of
  `local` — `--print-config` prints the entry without writing anything, and
  `ccdad mcp uninstall` removes it. `ccdad uninstall` removes it too, now that
  there is something to remove.
- **A Claude Code plugin and marketplace**, installable through `/plugin`. It is
  optional and it is MCP wiring only: the same server `ccdad mcp install`
  registers, needing the `ccdad` binary on PATH. The one-liners remain the
  first-class way to install ccdad.
- **`ccdad doctor` gained an `mcp-tools` row**, naming which tool-name spelling
  this machine has.

### Changed

- **Registering the server directly replaces the plugin's copy, and renames
  every tool.** Claude Code de-duplicates MCP servers by endpoint, so the two
  never run side by side once both name `ccdad mcp` — but the plugin's tools are
  `mcp__plugin_ccdad_ccdad__*` and a direct registration's are `mcp__ccdad__*`.
  A permission rule, hook matcher or allowed-tools entry written for one
  silently never fires under the other. `ccdad mcp install` warns and names both
  spellings when it finds the plugin installed; `ccdad mcp uninstall` hands the
  server back.

## [0.5.0] — 2026-08-25

The release that makes a pool safe to drive from more than one machine, and
stops the daemon logging you out of Claude Code. `ccdad own` declares the split,
which is the one piece of multi-machine setup ccdad cannot infer for you: its
target is a pure function of readings the *server* shares between your machines,
while every lock it holds is a file lock on one of them. Underneath, three fixes
end a loop that could get a whole token family rejected — a login the store
cannot name is its own state now rather than "nobody is live", nothing is
installed inside Claude Code's own refresh window, and ccdad rotates the live
credential itself instead of reacting to a rotation it did not perform. Under
`hover`, an account with nothing left no longer outranks the accounts that still
hold quota, and the pre-emptive switch — which a derived threshold had quietly
made unable to fire at all — fires again.

Three things worth knowing before they surprise you. **The danger band now polls
at 180 s rather than 60 s, and `preempt_lead` defaults to 6 minutes rather than
2**: at or above 95% of a window the live account was making 60 requests an hour
against an endpoint allowance of roughly 28–30, and a 2-minute lead against the
cadence that replaces it would have been shorter than a single poll. **`ccdad
probe` now spends a turn on a window whose reset has already passed**, where it
used to refuse with "already reports a reset time" — a probe is a warm-up, not a
way to learn a reset time, and the flat six-hour gate left roughly 4.2–4.6 hours
of stopped clock per account per day. And **`ccdad bootstrap` refuses a
`CCDAD_IMPORT` that holds a document rather than a path**, which `export
--base64` is what made plausible.

### Added

- **`ccdad own` declares which accounts a machine drives**, which is the one
  piece of multi-machine setup ccdad cannot do for you. The engine's target is a
  pure function of usage readings the *server* shares between your machines, and
  every comparator ends in the same tie-break, so two installs given the same
  pool pick the same account at the same moment: both sessions burn one
  five-hour window at twice the rate and hit a rate limit while the rest of the
  pool sits idle. Nothing detected it, because the 429 lands on the session
  rather than on the poller — every lock ccdad holds is a file lock on one
  machine, and the same is true of both projects it was written against.

  Run `ccdad own <ACCOUNT...>` on each machine with a set that does not overlap.
  An account this machine does not own is neither rotated into nor polled on a
  cadence, and an account added later belongs to another machine by default, so
  a split declared once stays declared. `ccdad switch` and `ccdad list
  --refresh` still work on one by name. The account Claude Code is logged in as
  is always polled, owned or not: ccdad's thresholds, hysteresis and pre-emptive
  switch are all statements about the live login. `ccdad own` with no arguments
  prints the split; `--clear` gives every account back.

- **`ccdad export --base64` writes the document as one unwrapped line, and
  `import` and `bootstrap` read either form.** A GitHub Actions secret, a
  `.env` entry and most CI secret stores hold a single string, so a JSON
  document pasted into one arrives with its newlines intact and breaks the file
  it landed in. The workaround was `base64 -w0`, whose `-w0` is easy to forget
  and whose absence surfaces as a failed deployment rather than as a failed
  command.

  Nothing has to be told which form a document is in: a ccdad export is a JSON
  object and so begins with `{`, which is in neither base64 alphabet, so one
  byte decides it. The decoder is deliberately permissive about everything
  underneath — whitespace anywhere in the blob is ignored, so a document that
  went through `base64` without `-w0` and came back wrapped at 76 columns still
  imports, and the url-safe alphabet and absent padding are both read.

  `--base64` is an encoding and not encryption, and nothing about the guards
  moved: `--out` still writes `0600`, `--full` to a terminal is still refused
  (the refusal now says why base64 does not change that), and `CCDAD_IMPORT`
  still names a path or `-` and never the document, because its value is
  visible in `docker inspect` and in `/proc/<pid>/environ`.

### Changed

- **A probe now restarts a stopped clock at its rollover, not six hours later.**
  A probe is not a way to learn a reset time; it is a warm-up. A five-hour
  window is anchored at *first use* and does not stretch when more is spent
  against it, so a clock started early is elapsed time you get for free —
  exhaust a window four hours in and you wait an hour instead of five. The old
  gate was a flat six hours since the last attempt, and against a five-hour
  window that leaves the clock stopped for an hour of every cycle, in the hour
  right after the rollover where starting it is worth the most. Measured before
  the change: about 4.2–4.6 hours of stopped clock per account per day.
  A window whose reset has passed now gets exactly one probe per rollover, with
  no interval in it at all; everything else backs off 15m, 1h, 2h, 4h and then
  six hours, so an account nothing can wake is still never tried more often than
  it used to be, while a *transient* failure recovers in about an hour rather
  than six. The next poll is aimed at the rollover instead of the idle cadence.
  Whether a probe worked is decided by the next reading rather than by the exit
  code — a turn can be billed and still exit non-zero, and the two are
  indistinguishable from outside — and never by the poll one minute after the
  probe, because the measured lag from turn to reported reset is 61–62 s against
  a 60 s poll.
  **Behaviour change to watch for:** `ccdad probe` now spends a turn on a window
  whose reset has already passed, where it used to refuse with "already reports
  a reset time"; and a row can appear stopped in `ccdad hover status` for about
  a minute once per rollover, which is the gap the warm-up closes.
- **`ccdad hover status` says what will actually happen to a stopped clock.**
  It used to print "(no reset yet; a probe is queued)" from a flag that meant
  only "this window named no reset" — so it said *queued* while the gate
  forbade probing, on the live account the engine refuses by design, on a
  machine with no Claude Code, and on accounts whose probes failed every cycle.
  It now reports the state the daemon computes from the same predicate: queued
  for a named time, sent, waiting for the reading, backing off after probes that
  woke nothing, spent for this rollover, held, never, off, or impossible. The
  `--json` payload gains an additive `warmup` object; `probeWanted` keeps its
  key and its meaning. `probe.last_error` — recorded since probing existed and
  read by nothing — is finally shown, so an account whose probes fail every
  cycle no longer looks exactly like one waiting its turn.
- **A probe is declined where the turn could be billed to credits.** Nothing in
  the probe path consulted the credit axis, and a just-rolled-over five-hour
  window reads as stopped however spent the rest of the account is. A probe is
  now refused when a window is at 100% and the account's overage switch is not
  demonstrably off. Being past hover's *pace* threshold is not being out of
  quota, and does not refuse.

- **The danger band now polls at 180 s instead of 60 s, and no longer shortens
  its own freshness gate.** At or above 95% of a window the live account was
  polling every 60 s with its reading kept fresh for only 30 s — 60 requests an
  hour against an endpoint allowance of roughly 28-30. Unlike the urgent
  cadence, which needs the account to be *moving* and so ends on its own, the
  band had no such gate and no per-identity division to soften it, so an account
  parked in the band held twice the budget for as long as it sat there. A poller
  that gets rate-limited stops reporting, and an unreadable account cannot be
  ranked — so the overspend defeated exactly what the band exists for.

  The band keeps what it was actually for: it still sits ahead of the exhausted
  rule and still skips the per-identity divisor, so an account inside it polls at
  180 s where it would otherwise take 600 s. The sustained rate is now a floor
  applied after every other rule rather than a value each rule is trusted to
  respect, which is the shape both `cswap` and `quota-board` arrived at
  independently. A short `serve_ttl` already written into `~/.ccdad/usage.json`
  by an earlier version is ignored on read, so the change takes effect on the
  first tick rather than after each account's next poll.

- **`preempt_lead` now defaults to 6 minutes rather than 2.** It is two of the
  cadence an at-risk account actually polls at, and that cadence moved. Left at
  2 minutes the lead would have been shorter than one poll interval, which is
  the case the pre-emptive switch exists to prevent: the projection is overtaken
  between two readings and the switch lands after the session was already cut
  off. An explicit `preempt_lead` in `config.toml` is unaffected.

- **An account with nothing left now sorts behind every account that still has
  something, and the engine no longer parks on it.** `Spent` answered two
  different questions — *past the threshold it was given* and *has nothing
  left* — and those order a pool identically only while every window shares one
  flat threshold. `hover` derives a fresh threshold per window per tick, and the
  99% cap inverts the two at the top: an account at 100% late in its window is
  measured against 99 and reports a slack of −1, the least negative figure in a
  pool where an account with half its week unspent, but early enough that its
  threshold is 31, reports −22. Ranked on slack the empty account won outright —
  on a six-account pool the engine switched onto the one hard-limited account
  from every other account and then answered "every account is spent" while five
  accounts held between a fifth and a half of their week.

  Being out of quota is its own question now, asked of the least raw room any
  binding window has rather than of slack, and it sorts such an account behind
  every account that has something in both the headroom and the recovery orders.
  The anti-flap margin runs on slack, which saturates there, so it could hold the
  engine on an empty account forever; it no longer does. The cooldown is
  unchanged. A credit-metered seat that has spent its allowance is read the same
  way. The credit gate still reads the *configured* threshold and never hover's
  derived one — a pace target nobody typed must not authorise a purchase.

- **The pre-emptive switch can fire under `hover`.** It required a candidate with
  positive slack, which reads correctly while a threshold is a number someone
  typed: an account past its own line is not somewhere to run to. Hover types no
  numbers — it derives a pace target from how far through its window each account
  is — and under it an ordinary pool is negative across the board, so the one
  rule that exists to move a session *before* its account hits a hard limit could
  not fire at all, silently, while five accounts still held quota.

  The bar is now three narrower ones. The candidate must not be *empty*, which is
  strictly weaker than positive slack and so only adds candidates where there
  were none. It must not itself be projected to run out inside the same horizon,
  asked with the projection the live account was judged by — moving from an
  account that stops working in five minutes to one that stops in six buys
  nothing and spends the cooldown. And it is *preferably* not one whose usage
  poller is sitting on a 429: preferably, because a 429 on the usage endpoint can
  be scoped to the access token rather than to the account, so refusing on it
  would hand the rule a fresh way to fire never.

### Fixed

- **`ccdad bootstrap` no longer prints `CCDAD_IMPORT`'s value into the container
  log.** The variable holds a path, and the value reached `os.Open`, whose
  `*os.PathError` carries it back verbatim — the one error path in that command
  that did not go through its "describe nothing out of the document" rule. An
  operator who set the variable to the document rather than to a path therefore
  logged every refresh token in it. `--base64` is what made that mistake
  plausible, by producing a document that fits in a variable at all, so the
  command now recognizes a document there and refuses it by name, and reports a
  path it could not open with the errno alone.

- **The daemon no longer overwrites a Claude Code login it cannot name, and no
  longer installs one Claude Code would refresh on sight.** Together these were
  a loop that ended in being logged out of Claude Code entirely. Attribution
  matches the credentials file to an account by its refresh token; Claude Code
  rotates that token whenever it refreshes the live login itself, so the moment
  it did, the file matched no stored snapshot. That was read as *nobody is
  live* — which removes the hysteresis baseline and, with it, the anti-flap
  cooldown — so the engine installed the account's pre-rotation snapshot over
  Claude Code's fresh one. Because that snapshot's access token had already
  expired, Claude Code refreshed again immediately, and the cycle repeated
  every few minutes, re-presenting a superseded refresh token until the server
  rejected the whole family.

  Three things changed. A credentials file holding a login this store cannot
  name is now its own state rather than the same answer as an empty file, and
  an unattended swap stands down on it. The daemon resolves such a login
  against the profile endpoint first: if it belongs to a managed account, the
  rotated pair is adopted back into that account's stored snapshot and the
  engine has a baseline again; if it belongs to nobody this store manages, the
  swap proceeds; if the endpoint cannot answer, nothing is written, because
  offline is not evidence about whose login it is. And no swap will install a
  credential inside Claude Code's own five-minute refresh window — it is
  refreshed first, and refused if that is not possible.

- **ccdad now rotates the live login itself, ahead of Claude Code, instead of
  waiting for Claude Code to do it.** This is the preventive half of the fix
  above: rather than guarding against a rotation ccdad did not perform, there
  is no longer a rotation ccdad did not perform. Claude Code refreshes a
  credential only inside its own five-minute window and skips the refresh
  entirely outside it, so ccdad now refreshes the live account in the band
  between that window and thirty minutes before expiry — where nothing is
  racing — and writes the new pair to both the credentials file and its own
  stored snapshot. Inside Claude Code's window the grant is still Claude
  Code's to spend and ccdad does not touch it; a rotation that fails changes
  nothing, since the token in hand is by definition still valid.

## [0.4.2] — 2026-08-24

One fix, no new surface. Under `hover`, `ccdad hover status` could mark an
account's five-hour window `(no reset yet; a probe is queued)` while the
daemon's own probe never actually queued one - it looked at whichever window
currently bound tightest and stopped there, and that was often a different,
already-resolved window.

### Fixed

- **The daemon's automatic probe now wakes any of an account's windows that
  has never been spent against, not only the one currently binding.** An
  account can carry an untouched five-hour window beside a weekly cap that
  binds tighter and already has its own reset; `probeDue` used to look at
  `HeadroomOf`'s single binding window alone, see the weekly one already had
  a reset, and give up before ever considering the five-hour window — so an
  account `ccdad hover status` marked `(no reset yet; a probe is queued)`
  could sit that way forever under `hover`. It now scans the same candidate
  window set hover status derives its note from and probes the first unspent,
  reset-less one, in schema order — ordinarily the five-hour window, matching
  what `ccdad probe <account>` already wakes by default.

## [0.4.1] — 2026-08-24

One fix, no new surface. A switch always pointed the credentials file at the
right account; `~/.claude.json`'s cached display of who that was did not,
because Claude Code's own refresh never corrects it once an account is
cached there. Every path that installs a login fixes it now.

### Fixed

- **A switch now keeps `~/.claude.json`'s displayed account name in sync with
  the credentials file it just installed.** Claude Code caches the live
  login's profile there as `oauthAccount`, and its own token-refresh handler
  never rewrites `accountUuid`, `emailAddress`, or `organizationUuid` once one
  is cached — only cosmetic fields get refreshed, and only when the cached
  object already looks complete enough that it skips re-fetching altogether.
  A switch that only replaced the credentials file therefore left Claude Code
  displaying whoever was live before, forever: the session authenticated and
  metered as the new account from its very first request, but nothing ever
  told the user so — usage tracked one account while the name on screen still
  read another. Every path that installs a login — `ccdad switch`, `ccdad
  auto`, the daemon, and `ccdad add --activate` — now corrects it: restoring
  the real object Claude Code wrote the last time this account was live, when
  one was captured, or writing a minimal identity and letting Claude Code's
  own refresh fill in the rest when none was.

## [0.4.0] — 2026-08-24

Two fixes, no new surface. `ccdad list`'s LEFT column stopped reading `?` for
every credit-metered account — the money axis `list --json` already exposed
now renders in the human table too. And `install.ps1` leaves the window it
ran in already usable: the PATH write it makes to the registry only ever
reached terminals opened afterwards, so the one running the installer itself
needed a restart it no longer does.

### Fixed

- **`ccdad list`'s LEFT column no longer reads `?` for every credit-metered
  account.** Headroom is computed from the five subscription windows alone,
  and an enterprise or pay-as-you-go seat carries none of them, so LEFT used
  to render the same "unreadable" mark a failed poll gets — for the whole
  class, not just the accounts that actually failed to poll. It now falls
  back to the same `extra_usage` reading `list --json`'s `usage.credit`
  already carried: with both money figures on the wire it prints
  `used/limit`, e.g. `25.50/100.00 used, 74.50 left (USD)`; with only the used
  figure it says what was spent and that the account sets no limit of its
  own. `?` is still exactly what an account that failed to poll shows.

- **`install.ps1` makes `ccdad` usable in the same PowerShell window the
  installer ran in, without opening a new one.** `irm | iex` evaluates the
  script inside the caller's own session rather than a child process, but the
  installer only ever wrote the new PATH entry to the registry and broadcast
  the change — which reaches processes started afterwards, not the one
  already running the install. It now also updates that session's own
  `$env:Path`, so `ccdad --version` resolves right after the one-liner
  finishes instead of needing a fresh terminal.

## [0.3.0] — 2026-08-24

Four new commands and an axis change underneath them. `probe` wakes a window
that has never been used, `hover` derives every threshold from pace instead of
reading one from the file, `primary` lets a credit-metered seat rank alongside
the subscriptions, and `bootstrap` — with a reference `Dockerfile` — makes a
container provisionable in one step. The ranking itself now orders on slack
(`threshold − used`, per window) rather than raw headroom, which is what makes
per-window thresholds and pre-emptive switching possible; at default
configuration the order and every spent verdict are byte-identical to 0.2.0, so
an upgrade with no `[window_threshold]` table set changes nothing about which
account is chosen.

Two things worth knowing before they surprise you. `ccdad run` now refuses when
the caller's own shell already carries a credential that outranks the one it
just installed — a session that used to authenticate as the wrong account while
ccdad reported success now says so instead and exits `2`. And poll intervals are
jittered by up to a tenth now rather than exact: `NewEngine` had never actually
wired the spread `internal/pollpolicy`'s own tests assumed, so several accounts
or daemons that paused together came back together and could empty an
identity's request budget in one burst.

### Added

- **`ccdad probe`, for a window that has never been used.** Such a window
  reports no reset time, so it has no pace, no projection and nothing to rank
  on, and the only way to get one is to spend against it: the endpoint reports a
  reset only once something has. The probe runs `claude -p "hi" --max-turns 1`
  in a throwaway credential home — `ccdad run`'s own scoping, from the same code
  — then carries any login that turn refreshed back into the store and deletes
  the session directory. The live credentials file is never written, and a test
  pins that on its bytes. The daemon additionally never probes the account that
  is live, because that probe is the one that can revoke the refresh token an
  in-flight session is using. It does not poll afterwards: the probe spends
  inference budget and an unscheduled poll would spend the usage budget too for
  a reading that is not there yet, so the poll that reads what it woke replaces
  that tick's poll and lands a minute later. `probe_unknown` defaults to true
  and the first probe of an invocation says on stderr that it is spending your
  quota; the daemon writes the same fact to `ccdad daemon logs` once per daemon
  lifetime. `--model` picks the window as well as the model, by family. An account
  whose credential is a setup token or an API key is never probed — no refresh
  grant means no reading could ever be taken for it — and neither is a window
  that already reports a reset; a probe is attempted at most once every six
  hours per account, counting every attempt rather than only the failures, and
  `--force` bypasses those last two and never the first. With no `claude` on
  `PATH` the daemon warns once per daemon lifetime and the account keeps no
  reset time, which is the state this command exists to end.
- **`ccdad hover on|off|status`.** Hover computes every threshold from pace —
  the share of a window that has elapsed, plus `100 / usable accounts` — usable
  meaning not disabled, not an api-key account, carrying a reading and not
  quarantined — capped at 99, and sets its own anti-flap margins, dropping the
  multiplicative `headroom_ratio` entirely because that margin runs on raw
  headroom while the ranking orders on slack, and the two disagree hardest
  exactly where hover operates. A window with no elapsed share falls back to
  80, and where the reason is that nothing has ever been spent against it,
  hover forces `probe_unknown`
  back on so the engine's own probe path spends the turn — hover queues nothing
  itself; a primary credit seat, which has no window at all, is held to a fixed
  95. `ccdad config list` grows a `HOVER` column marking each overridden key
  `overriding` rather than hiding it. The one thing a `window_threshold` entry
  is still read for is the decision it opts into: a weekly cap scoped to a key
  this build cannot name is ranked only because that entry exists, and hover
  replaces its number rather than that. It does NOT override
  `credit.max_auto_spend`, `primary` or `disabled`: two independent opt-ins for
  unattended spending is a rule rather than a knob, and the other two are facts
  about an account. `ccdad hover status` prints every threshold it chose, the
  share elapsed and the utilization it compared against, and the slack between
  them, because an omakase mode is only acceptable if the numbers are visible;
  it answers `5` rather than `0` when hover is off and prints the table anyway,
  so `ccdad hover status >/dev/null || ccdad hover on` is correct.
- **`ccdad bootstrap`, and a reference `Dockerfile` with its entrypoint.**
  `ccdad add-token` is not enough to provision a container: a setup token and an
  API key are both stored without a `claudeAiOauth` record, there is no refresh
  grant behind either, and the daemon can therefore never read that account's
  usage — so it is stored, usable as a credential, and can never be ranked,
  which is the entire product missing. `bootstrap` reads `CCDAD_IMPORT` — a
  path, or `-` for stdin — and imports a `ccdad export --full` document
  idempotently under `ccdad import`'s own validation, including the rule that
  newer local credentials are not overwritten without `--force`. Unset and empty
  are both a silent no-op with exit `0`, so an entrypoint may call it
  unconditionally, and it never answers `3` — the store carrying the document's
  accounts is the outcome either way, and a caller under `set -e` would refuse
  to start the container on every restart after the first. The document's
  contents never reach a log or an error message, including the reason it
  refused one. The image carries node, `@anthropic-ai/claude-code` and `ccdad`,
  and sets `CCDAD_HOME` and `CLAUDE_CONFIG_DIR` both, because they are
  independent axes and setting only the first leaves the Claude Code login
  inside the layer while the store sits on the volume. The entrypoint tolerates
  exit `3` from `ccdad daemon start` and nothing else, by number and re-raising
  the rest: `3` means a daemon is already running, while `1` and `4` mean the
  container would come up with no engine behind it. Nothing publishes the image
  — there is no GHCR job and the release workflow is untouched.
- **A threshold per window, and a ranking axis that measures the right
  distance.** `[window_threshold]` in `config.toml` gives `five_hour`,
  `seven_day` and every scoped weekly cap a line of its own; a window with no
  entry of its own uses `threshold`, which is unchanged. An account is spent when
  ANY window ccdad ranks is past its own threshold, and when a weekly cap is the
  one over, that is the cap the account is reported against — it is the one that
  will not come back for days. Reporting is nearly all that rule changes: the
  figures are still taken from the window with the least slack, whichever family
  it belongs to. Not quite all, and the exception is worth knowing — once every
  account is spent the ranking asks which one comes back first, and a blown
  weekly cap is then what an account has to wait out, so it ranks behind one
  whose five-hour window is genuinely back inside the hour. The engine orders on
  `threshold − used` rather than on raw percent left, because with a tight weekly
  floor those stop being the same question: an account one point from its weekly
  floor still carries forty-one points of raw headroom. With no
  `[window_threshold]` table the axis is the old one shifted by a constant, so
  the order and every spent verdict are byte-identical to 0.2.0; a test pins
  that, because it is what lets the change land without re-tuning every anti-flap
  default. `slack` and `windowThreshold` are published per account by `ccdad
  status --json`, `ccdad list --json` and every row of `ccdad auto --json`'s
  `order[]`, so a consumer can explain the ordering rather than only observe it.
  `headroom_ratio` deliberately did NOT move onto the new axis — a ratio is not
  shift-invariant and is undefined on a negative slack — so tightening a weekly
  threshold can leave it holding back a switch the ranking wanted. Setting
  `headroom_ratio = 1` switches that margin off, and `1` is the lowest value the
  config accepts.
- **A weekly cap filed under a scope this build cannot name is kept, and can be
  opted into the ranking.** ccdad names two scope keys on a `weekly_scoped`
  entry, `model` and `surface`, because those are the two Claude Code's usage
  schema names — but that schema is not a closed contract, so a third is legal
  wire. Such a cap used to be dropped. It is now carried and shown in the
  `windows` map of `ccdad status --json`, and left OUT of the ranking, because
  ccdad cannot state what it caps. Writing `window_threshold."weekly_scoped:<scope>:<display>"`
  into `config.toml` by hand is the opt-in and the only one — `ccdad config set`
  refuses the name, since with no reading in hand it cannot tell a scope the
  server really sends from a typo. A cap ccdad cannot name at all produces no
  window and cannot be opted into; `ccdad status --json` counts those as
  `unnamableWeeklyCaps` on the account's usage object, written only when it is
  not zero, so a script must read absence as zero rather than as an older ccdad.
- **`ccdad status` names the mode the engine is in.** When every account still in
  the pool is known to be over its threshold the ranking reverses — soonest reset
  first inside an hour, most slack outside it — and the table looks exactly the
  same either way. It now says `Mode: recovery` and why, and `status --json`
  carries it as `mode`. The key is absent rather than defaulted when no ranking
  could run: `strategy.Mode`'s zero value stringifies to `headroom`, so a report
  built from a pass that never happened does not look empty, it looks wrong.
- **`ccdad primary <ACCOUNT> on|off`, for a seat that is metered in credits and
  nothing else.** A credit account is a last resort by default and
  `credit.max_auto_spend` gates it, because credits are normally overage on top
  of quota already paid for. For an enterprise seat with no subscription behind
  it that premise is false, and a gate defaulting to `0` means the account can
  never be used. `primary` ranks it alongside the subscription pool on
  `credit.threshold − extra_usage.utilization` and turns the money gate off for
  that account only — the flag IS the second opt-in, typed by a human, and
  turning it ON says what it costs before it writes. The money figures are not
  read on this path at all. The flag travels: `ccdad export` carries it, so a
  seat armed on one machine arrives armed on the next, and `ccdad doctor` names
  every account holding it.
- **`preempt_lead`, and a switch that happens before the limit rather than after
  it.** The engine projects the active account forward over its own blind
  interval — the gap between when the current reading was taken and when the
  scheduler means to poll again, plus `preempt_lead` (default `2m`) — and moves
  if any window that binds the model reaches 100% inside it while some other
  account still has room. It reads the window that RUNS OUT FIRST rather than the
  one the ranking orders on, because those differ whenever burn rates do: an
  account whose weekly cap binds thirty-eight hours out would otherwise sit
  unswitched while its five-hour cap cut the session. It runs ahead of
  `hysteresis_pct` and `headroom_ratio`, which compare two accounts as they
  stand; it is behind the cooldown, which is the only thing bounding a switch
  storm, and it cannot reach the credit pool at all. The rule self-corrects
  because the horizon is the real poll interval: at 60 s it switches late and
  wastes nothing, and at the 1800 s a `429` imposes it switches early — polling
  is blocked, the session is not. `preempt_lead = 0` turns it off, except under
  `hover`, which derives its own lead and stops reading the key.

- **ccdad can name the Claude Code that is installed, and does it without
  running one.** A new `internal/ccver` reads the version off the install layout
  — a native launcher is a symlink into `<data home>/claude/versions/<VERSION>`,
  so one `readlink` names it, and an npm install of any era resolves into
  `node_modules`, so `@anthropic-ai/claude-code/package.json` names it. The
  obvious source, `claude --version`, is deliberately not used: the native
  launcher resolves and can UPDATE itself when invoked, so that probe would
  change what it measures, and `ccdad doctor`'s first rule is that a probe must
  not disturb its subject. `~/.claude.json` is not a source either —
  `lastOnboardingVersion` lagged the installed release by 118 patch versions on
  the machine this was written on.
- **A Windows native install is named too, from the bytes of its launcher.**
  There is no symlink to read there: the installer copies the versions binary to
  `~/.local/bin/claude.exe`, and it records nowhere which one it took. ccdad
  identifies it by comparing that launcher's CONTENT against the binaries still
  in the versions directory — exact, and the only thing that is. Comparing
  *sizes* is what Claude Code itself does, in its Windows update and again in
  its orphan cleanup, and it is already wrong: two releases on the machine this
  was written on are byte-for-byte the same LENGTH and different builds. That
  size check is also why a Windows launcher can hold an older build than the
  newest one installed — the update skips the copy when the sizes match — so
  reading the bytes is the only way to name what actually runs. Without this,
  `ccdad run`'s refusal never fired on Windows: a user on 2.1.112 or earlier got
  a default-mode session that silently ran as the machine's live login, because
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` does not exist on those builds on any
  platform. Two launchers that cannot be told apart, or a launcher matching
  nothing installed, still read as "found an install and cannot name its
  version" — a wrong version is worse than an unknown one, and it is the only
  answer that makes ccdad refuse to start on a machine that works.
- **ccdad looks for Claude Code under BOTH of the home directories it can be
  installed in.** Claude Code resolves the home under `~/.local` as
  `HOME ?? os.homedir()` and the home under `~/.claude` as a plain
  `os.homedir()`. On Unix those are one directory. On Windows they part the
  moment `HOME` is set — a Git-for-Windows shell sets it by default — so an
  install performed from PowerShell lands under `%USERPROFILE%` and one
  performed from an MSYS2 or Cygwin shell lands under `$HOME`, and Claude Code
  itself installs under the first home while searching under the second. ccdad
  searches both, and on Windows compares those paths case-insensitively, which
  is how the filesystem compares them.
- **`ccdad doctor` gains a `claude-version` check, for eighteen.** It names the
  version, how Claude Code was installed and which launcher it read, and it
  **fails** on 2.1.112 or earlier — the era where a keychain item shadows every
  switch and `ccdad run`'s default scoping is ignored, so a green report would
  be telling you the machine is fine while nothing ccdad does reaches Claude
  Code. A launcher it cannot classify is a warning, not a failure: ccdad not
  being able to read an install is not the install being broken.
- **`ccdad doctor` gains three checks — `path`, `profiles` and `api-key` — for
  seventeen.** `path` answers `ccdad: command not found` by reading two facts
  rather than one: whether the binary's directory is on the PATH of the shell
  you are in, and whether `ccdad setup-path` has registered it in a startup
  file. Those disagree in the ordinary case — a registration is written for a
  NEW shell to read — and which of the two it is decides whether the remedy is
  "open a new shell" or "run a command". It is never a failure: ccdad invoked by
  its full path works exactly as well. `profiles` reports a `ccdad run
  --full-profile` directory whose account is gone, which since profiles began
  holding an api-key account's `primaryApiKey` is a stored credential nothing
  else on the machine would mention again; it is a set difference against the
  account list, so a profile in daily use is not reported at all. `api-key`
  names which of Claude Code's five API-key sources would actually win, and
  whether that win displaces the OAuth login.

- **`ccdad doctor` gains an `oauth-source` check, for nineteen.** It answers
  which OAuth-shaped credential Claude Code would authenticate a session with,
  which is a different question from `environment`'s "is anything set that could
  defeat a switch" — and it is a question that check structurally cannot answer,
  because not every source is a variable. A session host injects a token at
  `/home/claude/.claude/remote/.oauth_token`, a path compiled into Claude Code
  with nothing to unset and which `ccdad run` cannot scope around; an Anthropic
  CLI profile under `~/.config/anthropic` is a directory. Both outrank the
  login. So does `ANTHROPIC_AUTH_TOKEN`, which outranks `CLAUDE_CODE_OAUTH_TOKEN`
  as well — the reverse of what this tree assumed in three places. The row warns
  and never fails: on a hosted machine the injected token is the correct working
  state, and failing there would hand a non-zero exit to every session working as
  designed.
- **A login whose scopes do not carry `user:inference` is not a credential, and
  ccdad now says so.** Claude Code takes the credentials file's login only when
  that scope is present, so a Console sign-in leaves a well-formed record that
  authenticates nothing. ccdad used to read "is there a `claudeAiOauth` object"
  and report the account as live.
- **`ccdad doctor` gains a `credential-files` check, for twenty-one.** It names
  a stored `<uuid>.json` that `accounts.toml` does not, with its path. Such a
  file holds a live refresh token at 0600 and no command a user would reach for
  can find it: `ccdad list`, `ccdad remove` and doctor's own account rows all
  read the document, and an orphan is by definition a uuid the document does not
  carry. The store's transaction rollback closes the way a REFUSED batch makes
  one, and only that — the rollback runs from the mutator's error return rather
  than from a `defer`, and the credential file is written before the document is
  saved, so Ctrl-C during a multi-account `ccdad import` still leaves one, and
  SIGKILL and power loss cannot be closed at all. Like every row here it
  reports: it will not delete a file the store cannot explain.

### Changed

- **Near the limit, the identity spends its budget on the account that can be cut
  off.** At or above 95% used on the binding window — 95% of where the endpoint
  refuses, not of where you set `threshold` — the account Claude Code is logged
  in as polls every 60 s, is exempt from sharing its identity's allowance, and
  has the freshness gate the scheduler writes with its reading cut to 30 s
  instead of 180, without which the daemon's own gate would refuse two of every
  three polls the band just asked for. Every other account on that identity is
  held back to about thirty minutes meanwhile, and only ever later: a `429`'s own
  floor still stands. It needs a reading that was actually taken — a failed poll
  says nothing about the account and does not put it in the band. Stated
  honestly, this does spend more of the identity's allowance rather than merely
  moving it: the live account goes from at most 20 requests an hour to 60, and
  the alternates give back less than that. The exemption itself is worth exactly
  the size of the identity — twofold on two accounts, threefold on three — and
  nothing at all on an identity of one, where 60 s was already the answer. What
  it refuses to do is hand 60 s to everyone on the identity, which would be 180 requests an hour against an
  allowance of roughly 28-30 per rolling hour. 60 s stays the floor: a `429`
  imposes a 360-second floor and an estimate that multiplies by 1.5 up to 1800 s,
  and the estimate always outruns that floor, so one `429` alone earns 540 s. It
  costs more, because a failed poll is not a reading — the band lapses with it
  and the account drops back to the cadence a spent account gets, divided across
  its identity, which is thirty minutes blind on three accounts at 97%. The
  hand-held path is deliberately not shortened; `ccdad list --refresh` still
  serves any reading under 180 s old.
- **The daemon and the engine now answer one "is this account spent" question.**
  `internal/daemon/tick.go` carried its own copy of the comparison, so the moment
  a window could carry a threshold of its own the daemon would have gone on
  publishing an exhausted state for an account the engine considered fine, and
  the poll cadence would have tightened around the wrong number. There is one
  implementation now and the daemon calls it, reading the whole threshold table
  rather than the single fallback key. This is the first test coverage that state
  has ever had.
- **`strategy.SubscriptionExhausted` is now `MainPoolExhausted`.** The predicate
  that opens the last-resort credit pool has to count primary credit seats: one
  with room means the main pool is not exhausted, and opening the overage pool
  while paid-for capacity sits unused is precisely what the gate order exists to
  prevent. The rule is otherwise unchanged — an unreadable account still holds
  the pool open, and a primary seat nobody can read holds it open for everyone —
  the old function stays for one release as a deprecated alias, and the key
  `ccdad auto --json` emits keeps its spelling, `subscriptionExhausted`, so no
  script breaks.

- **`ccdad doctor`'s stale-keychain-item remedy stops asking you which Claude
  Code you are on.** The remedy inverts across 2.1.113 — after it, deleting the
  item is cleanup; before it, the item is your live login and the next token
  refresh recreates it and deletes `.credentials.json` with it — and the row
  used to print both and leave you to decide. It now reads the version and gives
  the one that applies, leading with the cost rather than the command. Only an
  install ccdad could not classify still gets both.
- **`ccdad run` launches past npm's `claude.cmd` for every invocation, not only
  the ones it had to.** Resolution shipped narrow: ccdad read the shim and ran
  `node cli.js` directly only where the alternative was refusing an argument
  `cmd.exe` would have eaten, so `ccdad run acct -p 'fix&whoami'` went to the
  interpreter and `ccdad run acct -p 'summarize this'` went through
  `cmd.exe` — the same command taking two routes depending on the text of a
  prompt. It is now the extension alone that decides: a `.cmd` or `.bat` target
  is resolved past, and Go's escaping is then exactly right for every argument
  instead of approximately right for the harmless ones. `cmd.exe` leaves the
  launch entirely. The narrow shape was deliberate — none of the parser had run
  on Windows when it shipped — and the reason expired rather than being argued
  away: the Windows leg of CI has been green with it since `72e3f61`.

  **A shim ccdad cannot read still runs.** When the shim cannot be
  resolved — a `.cmd` npm did not write, an unmodelled `%VAR%`, an interpreter
  that is not installed or that resolves to another `.cmd`, the no-shebang shape
  `cmd.exe` runs by file association — the launch goes through the shim exactly
  as it always did, and only an argument `cmd.exe` would re-interpret is still
  refused. The `note:` line naming the substituted interpreter now prints only
  on that rescue, where it explains something; the ordinary launch is silent.

- **`ccdad run` refuses in its default mode on Claude Code 2.1.112 or earlier.**
  That mode scopes with `CLAUDE_SECURESTORAGE_CONFIG_DIR`, which does not occur
  even once in 2.1.112 — the variable arrived in 2.1.113, after the keychain
  backend it nominally outranks was already gone. So on such a build the
  scoping was inert: `claude` read the machine's own credentials file and the
  session ran as the LIVE login while ccdad reported success. The refusal is a
  usage error naming `--full-profile`, which scopes `CLAUDE_CONFIG_DIR` and does
  work there. It fires only on a version ccdad actually read, and only for
  accounts whose login is a credentials file: a **setup-token** account is
  scoped by `CLAUDE_CODE_OAUTH_TOKEN`, which every era reads and prefers over
  the stored login, so those sessions still run; an **API-key** account keeps
  its own, accurate refusal. An install ccdad cannot classify starts as before.

### Fixed

- **A killed `ccdad import` or `ccdad add` could leave a live refresh token no
  account named.** `add` writes an account's credential file before the
  document that names it is saved, and the reversal for a refused transaction
  ran from `mutate`'s error return rather than from a `defer` — so a process
  that left any other way, a panic or Ctrl-C partway through a multi-account
  import being the ordinary form, kept the file: invisible to `ccdad list`,
  `ccdad remove` and every account row `ccdad doctor` prints, because all of
  them read `accounts.toml` and an orphan is by definition a uuid it does not
  carry. The reversal now runs from leaving the transaction, and it holds
  SIGINT for the span of its own write — the door users actually use — so
  Ctrl-C there is a reversal and exit 130, not a leak. `root.go`'s refusal to
  trap SIGINT process-wide stands: the span is disk-only work with the
  cross-process lock already held across it, and it ends before the
  transaction returns. SIGKILL and a power cut are not closable at any price;
  `doctor`'s `credential-files` row still reports whatever gets past this.

- **A deleted `accounts.toml` read as a store with no accounts, and `doctor`
  told a user to delete every login they had left because of it.** Every check
  that takes a set difference against the account list — `profiles`,
  `primary-accounts`, `credential-files` — read the empty list produced by a
  missing document as the truth, so a deleted document turned every stored
  credential file into an orphan and the `credential-files` row's remedy is
  "Delete them once you have looked". A new `accounts-file` row notices the
  document is gone while credential files still sit beside it, fails loudly
  with the opposite advice — put the document back, do not touch the files —
  and gates the three rows below it so they report "cannot be trusted" rather
  than a reassuring answer built on no evidence. A fresh install, which has
  neither the document nor any credentials, is unaffected: that row reads `ok`
  and the three below it answer normally, exactly as before.

- **`ccdad config list` called the one unknown key it reads "ignored".** A weekly
  cap filed under a scope key this build cannot name is carried but left out of
  the ranking, and a `[window_threshold]` entry naming that window is the opt-in
  that puts it in — the only one, since `ccdad config set` refuses a scope it
  cannot verify against a reading it does not have. So the key is unknown to the
  config surface and live to the engine at once, and it was being reported under
  the note that says such keys "are being ignored (not deleted)". A user who had
  just hand-written the line ran `config list` to check their work and was told
  by ccdad that it does nothing. It now gets a note of its own saying it is read
  and when it takes effect, and the sentence about ignored keys keeps naming the
  ones that really are — a misspelled window name still lands there, because no
  reading ever produces it. `usage.ErrUnknownScope` existed for exactly this
  distinction and had no consumer; it has one now.

- **Every poll cadence was exact arithmetic, so a fleet that paused together came
  back together.** `internal/pollpolicy` spreads each interval it returns by up
  to a tenth either way, and it takes the sample as an argument so the whole
  policy stays a pure function -- `Next`'s own comment says the caller passes
  `rand.Float64()`. No caller did. `NewEngine` left the field nil, and a nil
  source is not "no jitter": the accessor answers with the midpoint, at which the
  spread is the identity. Every guard in that package was unreachable code while
  reading, in every comment, as though it were working. The one construction site
  a shipped binary reaches now supplies `math/rand/v2`'s `Float64`, which is safe
  for the concurrent use polling makes of it. What this was always for: several
  daemons restarted across machines, or a laptop waking with three accounts on
  one identity, share a budget of roughly 28-30 requests per rolling hour, and
  returning in lockstep empties it in one burst. Two tests that assert an exact
  next-poll deadline now fix the sample beside the clock they already fixed,
  which is the midpoint they were silently relying on, so what they assert is
  unchanged.

- **`doctor`'s credential-home drift warning blamed a cause the tree prevents.**
  It named `ccdad run --full-profile` as how a daemon comes to be driving a
  different Claude Code credential home from the shell reading the report —
  which auto-start has refused since it gained its containment test. The cause
  that IS still reachable, and deliberately so, is a credential home the user
  pointed somewhere themselves, through `CLAUDE_SECURESTORAGE_CONFIG_DIR` or
  `CLAUDE_CONFIG_DIR`. That mattered beyond the wording: the comment above the
  check was the only written reason it exists, so a reader who noticed the cited
  cause was prevented had an argument for deleting a check that is still needed.
  The row also told a user inside a `ccdad run` session to restart the daemon,
  where the daemon is not the side that moved and `ccdad daemon restart` is
  refused in that very shell; it now says that instead.
- **A host-injected API key was invisible to every command.** Claude Code reads
  `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR` **or**, when that is unset,
  `/home/claude/.claude/remote/.api_key` — one branch, two routes. ccdad modelled
  only the variable, so on a machine with the file and no variable a key that
  displaces the login was reported by nothing: `doctor`'s `api-key` row said no
  key resolved, `ccdad which` named the login's account, and a switch wrote a
  login nothing would read.
- **`CLAUDE_CODE_SIMPLE=0` put ccdad in bare mode.** Claude Code parses that
  variable with a four-spelling truthiness test (`1`, `true`, `yes`, `on`) and
  ccdad tested it for non-emptiness, so setting it to `0` — the natural way to
  turn something off — made ccdad report that no credential resolves on a machine
  that has one.
- **Every message about a displaced switch prescribed the wrong fix.** They said
  "Unset CLAUDE_CODE_OAUTH_TOKEN" — for `ccdad switch`, for `ccdad auto`, for the
  daemon's log, and for the note after an api-key switch. Three of the sources
  that displace a switch have no variable at all, so that sentence sent a user
  after something that is not set. All four now print Claude Code's own
  per-source remedy; for a host-injected token that is "check the host session".
- **`CLAUDE_CODE_REMOTE=0` made ccdad think it was inside a session host.** Claude
  Code reads that variable through a typed accessor that declares it a boolean —
  the same four-spelling test as `CLAUDE_CODE_SIMPLE` — and ccdad tested it for
  presence. Believing a session is hosted SUPPRESSES `ANTHROPIC_AUTH_TOKEN` and
  the `apiKeyHelper` and disqualifies an Anthropic CLI profile, so ccdad reported
  the login as the winner while one of them was deciding the session. The same
  accessor trims every string variable, so a variable set to spaces is now
  correctly read as not set.
- **`ccdad auto` and the daemon stand down for the whole displacing set.** The
  unattended gate was keyed on `CLAUDE_CODE_OAUTH_TOKEN` alone, so on a machine
  where anything else outranks the credentials file the engine switched, reported
  success, and changed nothing about what a session authenticates as — on every
  evaluation.

- **A Windows uninstall that could not schedule its own cleanup no longer
  reports that the binary could not be removed.** Removing the running binary
  is two steps on Windows and they fail separately: renaming the .exe aside —
  which is what stops `ccdad` resolving — needs no privilege, while the
  reboot-time delete that tidies the leftover writes to a machine-scoped
  registry key and needs administrator rights. The second step failing is the
  ordinary outcome for every install that did not need elevation in the first
  place, and the line printed for it began `… could not be removed`, which
  sends a user looking for a binary that is no longer at that path, or
  reinstalling over a machine that is already clean. It now says the binary is
  gone from that path and names the single leftover file, which is all that is
  actually left.

- **The legacy keychain item is derived the way the builds that WROTE one
  derived it.** ccdad was using the formula carried by today's Claude Code,
  where `CLAUDE_SECURESTORAGE_CONFIG_DIR` outranks `CLAUDE_CONFIG_DIR` and the
  account is validated against `^[a-zA-Z0-9._-]+$`. That code is dead — it has
  never written an item — and the variable does not occur even once in 2.1.112,
  the last release that read the Keychain at all. Two false "no legacy item"
  answers came out of it: inside a `ccdad run` session, which sets that variable
  by design, `doctor` hashed the session's credential directory and looked for an
  item that cannot exist; and on a machine whose username has a space or a
  non-ASCII letter in it, `doctor` looked under `claude-code-user` while the real
  item sat under the real name.

- **The legacy item is looked for under both spellings a keychain-era Claude
  Code could have written.** 2.1.38 started NFC-normalizing `CLAUDE_CONFIG_DIR`
  before hashing it into the item's name; 1.0.30 through 2.1.37 hashed the bytes
  as they came. A decomposed value therefore has an item under each digest
  depending on which build wrote it, and `doctor` probed only the composed one —
  a third false "no legacy item", from the same cause as the other two. Both are
  derived now, composed first, and they collapse to a single lookup whenever the
  variable is unset or already composed, which is every ordinary machine.

- **`ccdad doctor` says which machine the removal command is for, before it
  offers the command.** Claude Code 2.1.112 and earlier read the keychain item
  *before* `.credentials.json` and fall back to the file when it is absent, so
  deleting it does redirect those builds — but not durably. From 1.0.36 a
  successful keychain write deletes the credentials file whenever the pre-write
  keychain read was empty, which after a deletion it always is, so the next
  access-token refresh recreates the item and unlinks ccdad's file with it. On
  2.1.113 or later the removal is clean-up nothing can undo; on 2.1.112 or
  earlier it is not a fix at all and the item *is* the live login, so the answer
  there is to upgrade Claude Code. The check now names both version numbers and
  says all of this ahead of the `security` invocation.

- **`ccdad doctor` no longer reports `ok environment` while a switch is being
  defeated.** The `environment` check looked at two variables —
  `CLAUDE_CODE_OAUTH_TOKEN` and `ANTHROPIC_API_KEY` — and printed "nothing set
  that would make a switch a no-op" for a machine with `ANTHROPIC_AUTH_TOKEN` or
  `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR` set, both of which ccdad's own code
  already counted as displacing an existing login. All four are named now. The
  two remaining sources are not variables at all — the `apiKeyHelper` setting
  and the key stored in `~/.claude.json` — and the new `api-key` check covers
  them by running the resolver rather than by listing them, because the stored
  key is the one source that does NOT displace a login and `ccdad switch` writes
  it for every api-key account: a hazard list would have warned about ccdad's
  own steady state forever.

- **`ccdad daemon logs --follow` no longer goes silent at a log rotation on
  Windows, and the rotation on the other side of that race is no longer a coin
  flip.** Opening a file while a rename is in flight is where Windows answers
  `ERROR_SHARING_VIOLATION`, `ERROR_ACCESS_DENIED` or `ERROR_LOCK_VIOLATION` —
  an antivirus scanner or the search indexer holding it for a moment, measured
  at roughly 44% of replaces. None of the three is "not found", so the follower
  treated them as fatal and ENDED: someone watching the log got nothing from the
  rotation onwards, with the command already gone. It waits for the next poll
  now, and holds its position rather than guessing the file was replaced, which
  would replay the whole log for a rotation that never happened.

  The rotation itself had the same gap and a worse consequence. It closes the
  log's descriptor BEFORE renaming, and a rename that failed returned without
  reopening — so every later line went to a closed descriptor and vanished, and
  `RotateIfLarge` failed on the `Stat` it starts with, which meant the next tick
  could not recover either. One rename that lost a race cost the daemon its log
  for the rest of its life. The renames are retried now, on the same bounded
  policy `cclink` already used for credential writes, and the log is reopened
  whatever happens: a failed rotation costs one tick instead of the log. The
  errno set both sides consult is one copy, in `internal/winerr`, so they cannot
  drift.

  Three comments in the tree said Go passes `FILE_SHARE_DELETE` on `os.Open` and
  `os.OpenFile`. It does not — `syscall.Open` asks for `FILE_SHARE_READ` and
  `FILE_SHARE_WRITE` only — and that false premise is what excused both missing
  retries. They now say what the standard library does.

## [0.2.0] — 2026-08-23

Three things that were quiet in 0.1.0 answer back now, and an upgrade is where
you meet them. Bare `ccdad` was a no-op that exited 0; it is the dashboard now,
so without a terminal on both ends it is a usage error. Nine commands refuse to
run inside a `ccdad run` session rather than acting on that session's private
copy of Claude Code's state. And `switch --model`, which 0.1.0 documented as
having no effect, now narrows the ranking and rejects a model name it cannot
place. Two smaller ones: `ccdad uninstall` takes back the `PATH` entry ccdad
registered, and `ccdad doctor` gained a `credential-home` check that can fail a
machine 0.1.0 passed.

### Added

- **One engine per Claude Code login.** `CCDAD_HOME` and `CLAUDE_CONFIG_DIR` are
  independent axes, and moving only the first is the trap: two shells with
  different stores and the same credential root each took their own daemon
  singleton and both rewrote the same `.credentials.json`. Nothing was
  corrupted — the writes are serialised — the two engines simply undid each
  other's switches, and no command anywhere said so. ccdad now takes a second
  exclusion on the CREDENTIAL home (`<credential home>/.ccdad/engine.lock`, with
  `engine.owner` beside it naming the store that holds it, unlocked so it stays
  readable on Windows where locks are mandatory). A second store's daemon
  refuses to start and names the store that has it; `ccdad auto` refuses with
  exit `4`; `ccdad auto --once`, which holds no lock of its own, stands down
  inside the switch executor; auto-start stops spawning children that would
  immediately die. An attended `ccdad switch` is never refused — a human typed
  it — but it says which engine is about to undo it. A filesystem that cannot
  lock the credential home DEGRADES rather than refusing: the engine keeps
  running unguarded, and `ccdad doctor` names that. Neither file is ever
  removed, including by `ccdad uninstall`, because deleting a lock file splits
  the exclusion it provides and another ccdad store may still be using it.

- **`ccdad doctor` gains a `credential-home` check** — the fourteenth. It reports
  which credential home this shell resolves, whether an engine is driving it and
  which store that engine belongs to, and it catches the case nothing else can:
  a running daemon whose recorded credential home differs from the one you
  resolve, which is what a daemon started from inside `ccdad run --full-profile`
  looks like from outside.

- **`ccdad setup-path` puts the binary's directory on your `PATH`.** It is the
  answer to `ccdad: command not found` right after `curl | bash`, which has the
  installer's own script on stdin and so cannot ask permission to edit a startup
  file. It writes a marker-fenced block into the files your shell actually
  reads — for bash that is `~/.bashrc` *and* your login file, because a login
  shell reads only one of them and a terminal-emulator shell reads only the
  other — and running it twice leaves one block. `--print` emits the same bytes
  without writing. Exit `3` means nothing was written; it is keyed on what is
  registered, never on the live `$PATH`, so a directory that is on `$PATH` only
  because you pasted an `export` line still gets a durable registration. On
  Windows there is no startup file: the directory goes into `HKCU\Environment`
  with its value kind preserved and the change is broadcast, which is the same
  write `install.ps1` performs. It never creates `~/.bash_profile` (that would
  stop bash login shells from ever reading `~/.profile` again), and it refuses
  rather than guessing for csh, for an unknown `$SHELL`, and for a Homebrew or
  Scoop install whose `PATH` the package manager owns.

- **Bare `ccdad` is the dashboard, behind a TTY gate.** With a terminal on both
  stdout and stdin it renders exactly what `ccdad status` renders, followed by a
  one-line footer of the top verbs, and it auto-starts the daemon for the same
  reason `ccdad status` does. Anywhere else — a pipe, a redirect, cron — it
  prints usage on stderr and exits `2`, and starts nothing. The slot is promised
  to a TUI, so an answer no script can build on is what keeps that change a
  widening rather than a break. `ccdad -- list`, `ccdad -` and `ccdad ""` are
  usage errors as well: those tokens are dropped before dispatch, so the command
  written after them would never have run.

- **`ccdad switch --model <name>` narrows the ranking.** The weekly caps scoped
  to other models stop counting against an account, so one whose Opus week is
  spent can still be chosen for a Sonnet session. It only ever raises an
  account's headroom, it narrows `consume-first` as well as `headroom`, and a
  model name `ccdad` cannot place is now a usage error rather than a flag that
  silently did nothing.


- **`ccdad run --full-profile` serves API-key accounts.** Claude Code reads an
  API key from `primaryApiKey` in its global config, not from a credential
  home, so the default mode — which shares that file with the live session on
  purpose — still refuses, and now names the flag that works instead of only
  pointing at `ccdad switch`. Under `--full-profile` the key is written to the
  profile's own global config and nothing outside the profile moves. The
  `ANTHROPIC_API_KEY` route was measured and not taken: it is read outright by
  `claude -p` but gated for an interactive session on an approval list in that
  same config, and `--bare` bypasses the gate — one flag away from a different
  answer.

### Changed

- **`ccdad uninstall` now removes the `PATH` entry ccdad registered**, on both
  platforms. On Windows this was owed before `setup-path` existed:
  `install.ps1` has written `HKCU\Environment\Path` since it shipped, and every
  one-liner install left an entry pointing at a directory uninstall had just
  emptied. What it removes is only what ccdad can prove it added: on Unix that
  is what lies between ccdad's own markers, so a `PATH` line you wrote yourself
  is never touched; on Windows there are no markers, so `setup-path` and
  `install.ps1` now record the directory they added under
  `HKCU\Software\ccdad`, and an entry with no such record is left in place and
  named. That matters for a `go install` or a zip install, where the directory
  is one you put on `PATH` yourself and holds your other tools. A startup file
  whose fence is unterminated or doubled is reported and left alone rather than
  guessed at, and the remaining files are still cleaned.

- **`install.sh` points at `ccdad setup-path`** — by absolute path, because that
  message only appears when the install directory is off `PATH` and a bare
  `ccdad` would not resolve. The `export PATH=…` line is still printed
  underneath, for the shell you are standing in.

- **The per-model and per-surface weekly caps the usage endpoint reports in
  `limits[]` now rank, pace and display.** They were parsed and then ignored, so
  an account whose Fable or Cowork week was gone read as healthy; they now bind
  headroom like any other window, feed `consume-first`, carry a pace reading,
  and appear in `status --json` under `windows`. A cap of any other kind, or one
  whose scope names neither a model nor a surface, is still ignored.
- **A `limits[]` entry with no readable `percent` is unknown rather than 0%.**
  The schema writes the field non-null, but a body that omits it would otherwise
  have read as a window with everything left.

### Fixed

- **`ccdad run` on Windows no longer refuses an argument npm's shim would have
  mangled.** When `claude` on PATH is npm's `claude.cmd`, `ccdad run acct -p
  'fix&whoami'` was a usage error: Go emits an argument with no space or quote
  RAW, and `cmd.exe` reads the ampersand as a command separator. ccdad now
  reads the shim, and launches the interpreter it names — `node cli.js` —
  directly, which takes `cmd.exe` out of the launch entirely. It does this only
  where the refusal would otherwise fire, so a shim it cannot parse, an
  interpreter that is not installed, or one that itself resolves to a `.cmd`
  all keep the old behaviour rather than failing in a new way.

- **`ccdad switch` typed inside a `ccdad run` session rewrote the session, not
  the live login.** A session is a whole Claude Code, and everything typed in
  it inherits the session's `CLAUDE_SECURESTORAGE_CONFIG_DIR`, so the switch
  wrote `<session>/.credentials.json`, printed `Switched to`, changed nothing
  outside the session, and replaced the session's own login with another
  account's — in a directory `run` deletes on the way out. The commands that
  write Claude Code's state, delete the store the session lives in, or leave a
  daemon behind carrying the session's scope now exit `2` and name the session:
  `switch`, `auto`, `add`, `add-token`, `remove`, `uninstall`, `daemon start`
  and `daemon restart`. Reads still run, and `ccdad doctor` now says which
  session it is inside rather than reporting the session's credentials file as
  the live login.

- **Auto-start could still spawn a daemon inside a `--full-profile` session.**
  The existing guard read `CLAUDE_SECURESTORAGE_CONFIG_DIR`, which that mode
  removes rather than sets — so a daemon born there was pinned to the profile
  and managed it for the rest of its life. An ordinary `CLAUDE_CONFIG_DIR` of
  your own is still not a reason to refuse; only a config home ccdad created
  for a run is.

- **`ccdad export --include-mcp` no longer speaks for the machine from inside a
  session.** Neither when there are none to include, nor — the half that
  matters — when there ARE: it used to write the session's own MCP logins into
  the file and label them "this machine's", so a backup taken from inside a
  session held the wrong secrets under the right name.

- **`ccdad remove` now deletes the account's `--full-profile` profile.** Since
  `ccdad run --full-profile` began writing an API-key account's key into
  `<store>/profiles/<uuid>/.claude.json`, an orphaned profile is an orphaned
  CREDENTIAL rather than stale configuration — and nothing cleaned one:
  `uninstall` removes the whole store and `doctor` scans sessions only. The
  profile is named in the confirmation, because it also holds the MCP logins
  and trust answers that account accumulated.

- **A nested `ccdad run --full-profile` no longer seeds one account's profile
  from another's.** A profile is seeded from `CLAUDE_CONFIG_DIR`, resolved when
  the profile is created — which inside a `--full-profile` session is the outer
  account's profile, not the machine's configuration. Creating a profile there
  copied that account's settings and its `primaryApiKey` into a second
  account's profile, at a path nothing lists and `ccdad remove` does not clean.
  Only the CREATE is refused, so a nested run of an account whose profile
  already exists still works, and a nested run inside a default-mode session —
  which does not redirect `CLAUDE_CONFIG_DIR` — is unaffected.


- **`ccdad auto --json` no longer reports a stand-down as a completed switch.**
  The outcome switch had no `default`, so any outcome it did not name fell
  through to the success path and emitted `{"kind":"switched"}` with exit `0` —
  to the one consumer that cannot see the machine. It now names every outcome
  and fails loudly on one it does not know.

- **`ccdad auto` no longer discards a failure to release the daemon singleton.**
  Its release ran in a deferred closure assigning to an UNNAMED return value, so
  the assignment went to a dead local and the error vanished. A lock that could
  not be given back is precisely the error the next invocation trips over.

- **`install.ps1` no longer appends a duplicate `PATH` entry** on a machine
  whose user `PATH` holds `%LOCALAPPDATA%\Programs\ccdad` unexpanded. It reads
  the value raw — correctly, so that `%VAR%` references are not frozen to
  today's expansion — but compared each component only as stored, against a
  fully expanded install directory, so the entry never matched and a second copy
  was appended on every install.

## [0.1.0] — 2026-08-23

The first release the install one-liners can reach: `v0.1.0-rc1` was a
prerelease, so `releases/latest` had no answer for it and both installers
aborted fail-closed. That is fixed by this tag existing, not by any code in it.

### Added

- **The background daemon.** A self-managed detached process with a `flock`
  singleton, a pidfile, a 1 Hz tick loop and a `status.json` publisher, started
  automatically by any `ccdad` command. `ccdad daemon start|stop|status`
  drives it directly.
- **`ccdad auto`** — the auto-switch engine: the poller fleet, the measured
  poll policy with AIMD congestion control, and the swap executor.
- **`ccdad status`** — the engine dashboard, read entirely from disk. It never
  dials out.
- **`ccdad doctor`** — thirteen checks over the layout ccdad depends on: the
  store, permissions, locks, pidfile, status file, usage cache, engine state,
  config, leftover session directories, Claude Code's credential file, its
  top-level keys, a stale legacy macOS keychain item, and the environment
  variables that would make a switch a no-op. It reports; it repairs nothing,
  and it never creates what it is checking for.
- **`ccdad run`** — start a Claude Code session as a chosen account without
  changing the live login, by scoping that session's credential directory.
- **`ccdad list --refresh`** — take a fresh usage reading before listing, where
  the poll policy allows one, and two columns (`LEFT`, `RESETS IN`) to show it.
  The endpoint allows roughly 28–30 requests per identity per rolling hour on a
  sliding window, so a reading under three minutes old is served as it stands
  and no request is made; it says on stderr when it did nothing and why.
- **`ccdad config`** — read and write `~/.ccdad/config.toml`.
- **`ccdad export` / `ccdad import`** — a portable JSON document of the account
  store, with a three-flag gate on including MCP credentials.
- **`ccdad disable`, `enable`, `alias`, `move`** — the remaining account verbs.
- **`ccdad uninstall`** — stops the daemon, deletes the store, removes the
  binary. Both installers already pointed at it.
- **API-key account activation**, modelling Claude Code's own resolution order
  for `ANTHROPIC_API_KEY`, `apiKeyHelper` and `primaryApiKey`.
- **CI**: `gofmt` and `go vet`, `go test -race` on Linux, macOS and Windows,
  and a gate that fails if any of the six release targets needs cgo. The
  release workflow now refuses to publish a tag whose commit has no green CI
  run. Pull requests from forks get their own run; a branch in this repository
  keeps the run its push already triggered.
- **The files a public repository is expected to have**: a README, `LICENSE`,
  `NOTICE`, `THIRD-PARTY-LICENSES.txt`, `SECURITY.md`, `CONTRIBUTING.md`, a
  code of conduct, issue and pull request templates and a dependabot config.
  Every release now also carries `LICENSE`, `NOTICE` and
  `THIRD-PARTY-LICENSES.txt` as assets, hashed into the same `sha256sums.txt`
  and covered by the same attestation as the binaries — BSD-3-Clause and
  Apache-2.0 require the notice to accompany a *binary* distribution.

### Changed

- Every path resolver returns an error instead of an empty string, so an
  unresolvable `$HOME` fails loudly rather than putting a credential store in
  the current directory.
- Store writes take a cross-process lock and re-read inside it.

### Fixed

- A credential rotated by a running session is adopted back only into an
  account that actually has one.
- `ccdad uninstall` no longer leaves behind the per-session directories
  `ccdad run` creates.
- Several Windows paths that no test had ever compiled, let alone run, found by
  putting Windows in the CI matrix.

## [0.1.0-rc1] — 2026-08-22

First published build. **A prerelease**, which means GitHub's
`releases/latest` deliberately has no answer for it: the install one-liners in
the README abort fail-closed until a non-prerelease tag exists. To install this
one, pin it — see the README's *Installing a specific version*.

### Added

- **Accounts and login.** `ccdad add` (browser OAuth with the dual-path
  loopback/paste race), `ccdad add-token`, `ccdad list`, `ccdad which`,
  `ccdad switch`, `ccdad remove`.
- **The credential swap.** Claude Code's three locks taken in its own order,
  the file re-read under the lock, an atomic rename, and a **deny-list** of the
  five account-scoped keys so that machine-scoped keys — `mcpOAuth` above all —
  survive a switch instead of being destroyed.
- **Usage and ranking.** The usage API client with its cache, the headroom and
  pace model, account ranking, the credit gate behind two independent opt-ins,
  and account reclassification.
- **Anti-flap.** Hysteresis, cooldown and quarantine, so a switch has to be
  worth making rather than merely momentarily better.
- **The release pipeline.** Six static targets built `CGO_ENABLED=0`, an
  enforced `sha256sums.txt`, a keyless build-provenance attestation, and both
  installers.

[Unreleased]: https://github.com/Kweiza/ccdaddy/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0-rc1...v0.1.0
[0.1.0-rc1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.1.0-rc1
