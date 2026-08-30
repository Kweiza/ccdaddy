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

### Fixed

- **A silent `security` failure is classified by its exit code.** 0.9.5 made the
  code visible; this reads it. `security` exits with the OSStatus truncated to
  its low byte — not a guess, it is why `securityNotFoundCode` is 44 for
  errSecItemNotFound (-25300) — so 36 is errSecInteractionNotAllowed, the
  keychain refusing a lookup it cannot ask a human about. ccdad already had the
  right sentence for that and could not reach it, because `security` writes
  nothing to stderr on that path. `said-nothing (exit 36)` now reads
  `interaction-not-allowed (exit 36)`, and `doctor` prints the remedy.
  Only the keychain band is read and only where an existing sentence means what
  the code means: errSecInteractionRequired (29) and errSecNotAvailable (53) are
  deliberately left unnamed, because a bare number a reader can look up beats a
  name that is nearly right. stderr still outranks the code — the stderr
  classifier mirrors Claude Code's own and the two must not disagree about why
  one spawn failed.
- **The refused-lookup sentence tells a daemon what to do.** It was written for
  a person running `doctor`, who can move to another shell. A daemon cannot: it
  inherited the refusing session at startup and every successor it spawns
  inherits the same one, so 0.9.5's automatic replacement cannot fix this
  particular failure and the restart has to come from a session that can read
  the keychain. That is why one wedged daemon survived a restart and another
  did not.

## [0.9.5] — 2026-08-30

The release that lets a broken daemon say so, and then fix itself.

A daemon spent three hours and twenty minutes failing every tick — 11,300 in a
row, each one a switch that did not happen — while `ccdad doctor` printed ok on
every row it had, and its own log said the same eleven-thousand-line sentence
about a `security` spawn that had named neither its exit code nor its subject.
Nothing here is a fix for that spawn, whose cause is still unidentified. All
three are fixes for not being able to tell.

### Added

- **The daemon replaces itself when its tick loop wedges.** An unbroken run of
  failing ticks lasting five minutes ends the loop, and the entrypoint starts a
  successor — after `Run` has given the singleton back, which is why the
  hand-off is there and not inside it. The evidence is what picked replacement
  over retry: a wedged daemon failed 11,300 consecutive ticks over three hours
  and twenty minutes without recovering, and a fresh process was healthy on its
  first tick, spawning the same `security` with the same argv. Bounded at three
  replacements, carried in the child's environment as `CCDAD_DAEMON_RECOVERY`,
  because an earlier replacement of that same daemon came up and wedged again —
  so a machine can be in a state a new process does not fix, and the rule has to
  end somewhere that still has a daemon running. A process that ever completed a
  tick starts the successor's count over, so a daemon that wedges after a week
  still gets its replacements.
- **`doctor` has a `tick-health` row.** It is the difference between a daemon
  that is running and one that is working. Every existing daemon row asked about
  liveness, and all of them printed `ok`, truthfully, for the whole three hours
  and twenty minutes: the singleton was held, the pidfile named the process, and
  the status document was fresh, because a failing tick still publishes. The row
  reads the streak, its age and its cause out of three new status fields
  (`tickFailures`, `tickFailingSince`, `lastTickError`), and reports a daemon
  that has published nothing as skipped rather than ok.

### Fixed

- **A failing tick is no longer logged once a second.** The first failure of a
  run is logged, a failure whose error has CHANGED is logged straight away, and
  a run still going is logged once per five minutes with its count and age;
  recovery gets a line of its own naming what it cost. The run that prompted
  this wrote 11,300 identical lines and 900 KB in three hours, none of which
  carried anything the first one did not — and rotation threw away the context
  around it.
- **A silent `security` failure now names its exit code.** A spawn that exits
  non-zero and prints nothing was reported as `security find-generic-password:
  empty`, which names no code, no store and no remedy — and "empty" reads as a
  verdict on the ITEM, which is the one thing it never means: an item holding a
  zero-length blob exits 0 and is a value, not a failure. It classified an empty
  STDERR. `KeychainError` now carries the exit status and the failure is spelled
  `said-nothing`, so the message reads `security find-generic-password:
  said-nothing (exit 60)` and `doctor` prints the number rather than "the lookup
  failed without saying why". Found on a machine where the daemon logged 11,300
  identical copies of the old sentence over three hours, none of which said
  enough to tell that failure apart from any other.

## [0.9.4] — 2026-08-30

The release that makes the reports agree with each other.

0.9.2 and 0.9.3 taught ccdad to read and write the store Claude Code actually
reads. What they did not do is teach it to SAY so: three reports went on naming
the credentials file from a constant, and on a machine whose login is the
keychain item, `ccdad doctor` contradicted itself two lines apart.

### Fixed

- **`ccdad which` names the store that answered.** `cclink.LoadWithSource` is
  `Load` plus which store it came from, and `switcher.AttributeLogin` now carries
  it into `via`. It read `via the Claude Code credentials file` while the
  keychain item was what had been read — on exactly the machines where which
  store it is was the reason someone ran the command.
- **`doctor`'s `oauth-source` row does the same.** It said "Claude Code would
  authenticate with the login in the credentials file" directly beneath a
  `keychain` row saying the item is read first. Same report, opposite claims.
- **`which` no longer calls an unattributable login unmanaged.** Attribution
  matches on the refresh token, and Claude Code rotates that on every refresh, so
  the commonest cause of "cannot attribute" is one of your own accounts moments
  after a refresh — the engine has always treated it that way. The notice now
  says what ccdad could not do and why, and points at `ccdad add`. The
  environment axis keeps the blunt sentence, because a token supplied through
  `CLAUDE_CODE_OAUTH_TOKEN` is not rotated behind ccdad's back and there the
  claim is one that can be made.

### Changed

- `ccver.LastKeychainEra` is now `LastPreSecureStorageDir`, `Install.KeychainEra()`
  is `PreSecureStorageDir()`, and `refuseKeychainEra` is `refuseUnscopedRun`. The
  constant carried two facts — "reads the keychain" and "does not know
  `CLAUDE_SECURESTORAGE_CONFIG_DIR`" — which were bisected as one, and only the
  variable half held. Every release reads the keychain, so there is no era to be
  on. Each caller was re-read rather than swept: `run` and `probe` gate scoping on
  it and `doctor` prints it, and all three want the variable.

## [0.9.3] — 2026-08-30

The release that makes 0.9.2 actually work on the machine it was written for.

0.9.2 taught ccdad that the macOS Keychain item is the login and had it install
into that item on every switch. Both halves were wrong in practice, and both
were found within minutes of shipping — by running it, not by testing it.

### Fixed

- **A switch decided `AlreadyOn` from the credentials file**, which is the store
  Claude Code consults *second*. `writeMerged`'s base is not merely what gets
  merged onto; it is the blob every `ActivateWith` caller decides from, and
  `switcher.Execute` computes `AlreadyOn` out of it. With the item holding one
  account and the file another — the exact shape 0.9.2 exists to repair —
  `ccdad switch <the file's account>` answered "Already on" and wrote nothing.
  Since the keychain install lives on the write path, no write meant no repair:
  a deadlock rather than a delay, on precisely the machine that needed it. The
  base is now the store Claude Code reads, with the file half still taken from
  the directory the locks cover.
- **The item was written with the credentials file's indented bytes.**
  `security -w` — the read Claude Code performs, and the one
  `LoadKeychainCredentials` performs — returns HEX for any value containing a
  newline, and both readers parse the output as JSON. So the item ccdad wrote
  came back as hex, failed to parse, and the combinator fell through to the file
  as though no item existed; the reader's own catch swallows the error. The file
  stays indented, matching what Claude Code writes there, and the item is now
  compact, matching what Claude Code writes *there*.

### Changed

- Machine-scoped keys (`mcpOAuth`, the device key) are now carried forward from
  the keychain item rather than the file when an item exists. Claude Code writes
  the primary and skips the fallback on success, so on such a machine the file
  stops being updated and merging onto it would put back keys the item had
  already moved past.

## [0.9.2] — 2026-08-30

The release that found out a switch was never reaching Claude Code on macOS.

ccdad wrote `.credentials.json` and stopped. On macOS that is the store Claude
Code consults *second*: its credential store is a `keychain-with-plaintext-fallback`
combinator whose read returns the keychain item and only falls back to the file
when there is none. So on any machine with an item, every switch moved the file
and left the login alone — the file changed, `ccdad which` read it back, the
daemon logged a switch, and every request went on authenticating as whoever the
item named.

ccdad believed otherwise because of a measurement recorded in
`internal/cclink/keychain.go`: that 2.1.113 removed the keychain backend, making
the item inert on anything installable today. It was measured again against
2.1.234, 2.1.238 and 2.1.251 — one of them named in the original list — and all
three carry a whole keychain backend that spawns
`security find-generic-password`. The original search looked for
`name:"plaintext"`, found it, and stopped; the combinator is named after *both*
members, so the fallback's name is present in every build that has a keychain
primary.

### Fixed

- **A switch now installs into the keychain item**, when one is there, so the
  swap reaches Claude Code rather than only the file. An absent item is left
  absent: with no item the file already is the login, and creating one would
  introduce a second store and make it the one consulted first.
- **`Load` reads the keychain item first on macOS**, so every command that asks
  who is live — `which`, `doctor`, attribution, the engine — asks the store
  Claude Code actually reads. `ccdad which` could previously name one account
  with no hedge while the work was being metered to another.
- **`ccdad doctor`'s `keychain` row no longer reports a live credential store as
  a leftover.** It said "nothing is broken right now. Removing it is cleanup",
  which is how the item that was shadowing every switch got read as harmless.
  It now says the item is the login and that ccdad keeps it up to date, on every
  release rather than per era.
- **The `claude-version` row drops its keychain half.** It named a keychain
  shadow as one of two defeats of Claude Code 2.1.112 and earlier; every release
  has that shadow, so it was never what put that era on the far side of
  anything. The `CLAUDE_SECURESTORAGE_CONFIG_DIR` half is real and stays.

### Changed

- `ccver.LastKeychainEra` now means only "the last release that does not know
  `CLAUDE_SECURESTORAGE_CONFIG_DIR`". The two facts were bisected as one and
  only the variable half held. The name is left alone for now, deliberately:
  renaming it means re-reading every caller to be sure none is still reading it
  for the keychain.

## [0.9.1] — 2026-08-27

The release that stops pretending a wordmark is text.

The dashboard's own name and its four mascots have been seven-bit ASCII
wherever ccdad drew them — one flat colour over a hand-typed shape. This
release draws them instead: two grids traced from the design mockup, one
character per pixel, folded two rows to a cell with the upper and lower
half-block glyphs so a foreground and a background paint one terminal cell
as two independent pixels. The colour comes from the palette that already
existed for everything else on the page — no new role, no new gate — because
the glyph is chosen by which half of a cell is drawn, never by which theme is
active. That is also what keeps `NO_COLOR` and a piped or redirected page
legible: every colour can be stripped and a recognisable silhouette survives,
dithered rather than solid.

### Added

- **The wordmark and the four creatures are drawn as coloured pixel art**,
  with the same typed ASCII this page has always drawn kept as an exact,
  byte-for-byte fallback for the one case the new art cannot safely draw in:
  a console whose width engine measures ambiguous characters as two columns.
  That fallback fires even when a user has explicitly asked for the Unicode
  glyph set, because the frame around the art is what would break, not the
  art's own good looks. Nothing about the page's layout, its column widths,
  or its behaviour on any command changes — this is decoration, and it
  degrades the same way every other coloured surface on this page already
  does.

## [0.9.0] — 2026-08-27

The release that learns a seat can be metered in money rather than quota, and
the one that makes every document ccdad writes agree about what time it is.

The first began as a live account ccdad could see and could not use. A
`claude_enterprise` seat metered only in `extra_usage` credits reports every
plan window `null` and an empty `limits[]` — money is its only meter — and
every layer above that read the plan-window axis as though it were the only
one. It was filed as a subscription at add time, because the profile field that
names the billing describes the ORGANIZATION and the organization really does
hold a contracted subscription; the repair written for exactly this case was
never called by anything; ranking then measured two such seats as equally
unknown and switched off the one with 60% of its balance left onto the one that
was drained; and `status`, `list` and the dashboard printed `?` in the USED
column for an account the engine was ranking on a perfectly good percentage.
One reading now produces one answer for all of them, `ccdad runway` measures
the credit burn for a fleet with no windows to simulate, and `seat_tier` — the
one field that says how a SEAT is metered rather than how its organization is
billed — is stored, re-read when it goes stale, carried through
`export`/`import`, and written into Claude Code's own cached profile on a
switch, which is where it decides the model tier. Two `claude_max` bodies are
pinned beside the enterprise one as a control, and they settle four fields that
looked like tier discriminators and are not.

The second is what time it is. `status.json` published `nextPollAt` two ways —
an account on the ordinary cadence carried the machine's offset, one pulled to
a window rollover carried the `Z` it was parsed off the wire with — and read
against a local clock the `Z` row looked nine hours overdue when it was four
minutes in the future. `history.json` had the same split: 292 timestamps at
`+09:00` beside 870 at `Z`, describing one afternoon. The zone belongs to the
DOCUMENT now rather than to the field, applied where each document is
serialised rather than at every writer that computes a moment, and it is the
machine's, because every reader of these documents is on the machine that wrote
them. Every instant is unchanged and every value is still RFC 3339 with its
offset written out. Two documents are deliberately outside the rule and now say
why: `ccdad export` keeps UTC because it is written to be carried to a machine
whose offset is not the writer's, and the `snapshot` object inside `usage.json`
is a verbatim mirror of the endpoint's body, whose whole job is being
comparable against a recorded response.

The third is smaller and shows up on every screen. Once a day the daemon asks
the host the machine installed from what the newest release is, carrying the
running version and nothing else, and it publishes the READING rather than a
verdict: an upgrade replaces the binary while the old daemon keeps publishing,
so an "update available" boolean computed there would spend a day telling the
new binary to install what it is already running. `update_check` switches the
request off for a machine that may not call out. Underneath all of it, the
checks this repository gates on were re-measured and found answering questions
they had not been asked: `scripts/ci.sh` returns 0, 1 or 2 and nothing else, a
failing check closes its fold in the Actions log, and three checks that
reported success having looked at nothing were fixed — one of them a whole arm
of `cites` that macOS's git had quietly turned into a no-op.

### Added

- **A pro/max control group is pinned beside the enterprise wire.** Every live
  recording this repository had was an enterprise seat from one organization, so
  nothing in the enterprise body could be told apart from what that one org
  happens to send. Two verbatim `/api/oauth/usage` bodies from a `claude_max`
  seat are pinned now — the same seat with its overage meter off and on — and
  read as a pair against the enterprise one. The pair settles four fields that
  looked like tier discriminators and are not: `can_toggle` and
  `can_purchase_credits` are false on both, `spend.cap` takes the same shape on
  both once the meter is configured, and `member_dashboard_available` does not
  move when the meter does. The one structural difference is the one already
  keyed on — a plan seat has windows and a money-metered seat has none. The
  overage-on body is also the first live response to reach the `Blocked` branch:
  `out_of_credits` is refused while `spend_limit_reached` is false at 78% of a
  cap, so the monthly cap and the credit balance are two exhaustion axes and
  must not be collapsed into one.

- **The credit runway is measured for a fleet with no plan windows, and
  `ccdad runway` shows it.** The credit axis was handed the account list the
  window simulation had already filtered, and that filter drops every account
  with nothing to simulate — so a fleet metered only in money could not reach
  the figure at all, and adding one irrelevant plan window to the same account
  made it appear. The page was gated on the window basis too, so such a fleet
  got no table and was told there was not enough history to measure a burn rate
  while its burn rate sat measured in the same document. The two window rows are
  now printed only when a window basis exists: two rows of `-` above a real
  credit row invite the reader to conclude the fleet burns no quota.

- **`seat_tier` is stored, and it crosses `export`/`import`.** It is the one
  profile field that says how a *seat* is metered, as opposed to how its
  organization is billed, and it was fetched and then dropped. Carrying it
  through a move between machines matters because `Kind` and the primary flag
  are both *derived* from it: an import without it lands an account that behaves
  correctly and can no longer say why, and the next thing to re-derive either
  from the profile fields, on a machine that never saw the profile, gets a
  different answer.

- **The daemon says when a newer ccdad is out.** Once a day it asks the
  releases page what the newest release is — one request per day per store, to
  the host the machine already used to install ccdad, carrying the running
  version as its user agent and nothing else. What it heard shows up as an
  `Update:` line in `ccdad status`, on the dashboard's daemon screen, as an
  `update-check` row in `ccdad doctor`, and as four keys in the `daemon` block
  of `ccdad status --json` and `ccdad daemon status --json`. The daemon
  publishes the *reading* and never a verdict: upgrading replaces the binary
  while the old daemon keeps publishing, so an "update available" boolean
  computed there would spend one day per upgrade telling the new binary to
  install what it is already running. The comparison happens in whatever is
  reading, against its own version, and `dev` builds are silent because their
  version compares with nothing. The new `update_check` key switches the
  request off for a machine that may not call out; it defaults to `true`, and
  it does **not** gate `ccdad update`.

### Changed

- **Every `--json` document renders its timestamps in the machine's zone, and
  so do the three absolute moments `ccdad doctor` prints.** This is visible to a
  consumer, and it is the other half of the fix above: a document that agreed
  with itself only inside the daemon's half would still print the engine's poll
  times beside the endpoint's window rollovers in two zones. Six payload sites
  that formatted their own moment with `.UTC()` — `auto`'s `retryAt`, `at`,
  `recoversAt` and `weeklyResetsAt`, and `hover status`'s `lastAttemptAt` and
  `at` — now ask for the same location, so one rule covers the documents that
  let the encoder render a `time.Time` and the ones that spell their own layout.
  `ccdad doctor` joins them because it reads its stamps out of the published
  document and prints them to a PERSON: on the day of an upgrade the old daemon
  is still running and still publishing, so a new binary reads a document whose
  stamps carry whatever zone each writer handed them, and `generated
  2026-08-22T05:00:00Z` under a local clock is the nine-hour stall this started
  as. Every instant is unchanged and every value is still RFC 3339 with its
  offset written out, so a conformant parser reads the same moment it read
  before; a consumer that compared these strings BYTEWISE was already wrong,
  because the zone it was matching on varied from row to row. `ccdad export` is
  deliberately not included: that file is written to be carried to another
  machine, where the writer's offset is not the reader's, and it keeps UTC.

- **`scripts/ci.sh cites` reads every file git knows about, not five
  extensions.** The pathspec was `*.go *.sh *.ps1 *.yml *.md`, which is a
  narrower rule than the one `CONTRIBUTING.md` states and than the check's own
  name claims: `Dockerfile`, `ccdad-entrypoint`, the `.cmd` shim fixtures, the
  `.js` generator, `THIRD-PARTY-LICENSES.txt`, both plugin manifests, `LICENSE`
  and `NOTICE` were among thirty-seven tracked files it never looked at. It now
  reads everything git tracks plus everything written and not yet staged —
  running it before `git add` used to be a green that meant nothing, because a
  new file gave 0 and the same file after `git add -N` gave 1 — minus anything
  `.gitignore` covers and anything git treats as binary. Measured before the
  change: widening it costs zero new failures on this tree. The rule is also
  stated as *every line* rather than *every comment* now, which is what it has
  always enforced: a Go string literal pointing at a section of a document
  nobody outside this machine has fails it, and so does prose in `README.md`.
  That is the right scope — an unreachable citation in a user-visible error
  string is worse than one in a comment, not better — so the wording moved
  rather than the code.

- **A citation to a published standard is subtracted from a line, rather than
  used to excuse the whole line.** The exemption matched one exact spelling and
  then dropped the whole line, and it was wrong in both directions. A citation
  with a comma after the RFC number, and one with the section written before the
  number rather than after it, are both forms `CONTRIBUTING.md` permits, and
  both failed the build — a gate that fails on correct prose is a gate somebody
  switches off, and the spelling it pushes people towards, with the word
  *section* written out and no symbol at all, is invisible to this check in both
  directions. In the other direction, one well-formed RFC citation anywhere on a
  line exempted that line from *all three* of the patterns this check looks for:
  a line that named an internal work item and also cited a real RFC section
  passed, while either half of it alone failed. The accepted citation is now
  removed from a copy of the line and what is left is re-judged, which is the
  rule anybody would state in words. The pointing
  arm also catches a name written in capitals, and the three-segment minimum
  that keeps it from reporting ordinary English is written down with the
  measurement behind it: at two segments the same shape reports `per rate-limit
  window` and six other correct lines of this repository.

- **A comment that names a test this tree does not define fails the build.**
  Rename a test and every comment naming it goes false while the tree stays
  green, because nothing anywhere read those names — and it had already
  happened here. The check is a Go test rather than a seventh arm of `cites`,
  because what a comment is, what a string literal is and what a test
  declaration is are questions `go/ast` answers exactly and a regex over every
  tracked file answers approximately; `cites` carries a pointer to it, since
  that is where a reader looks for the citation rule. It reads Go comments and
  Go string literals — a `-test.run=` argument in a test that re-executes
  itself is a citation too, and a rename there leaves a subprocess that runs no
  test and exits 0 — plus shell scripts, because `ci.sh`'s own comments cite
  tests. A name in a `-run` argument is judged as the unanchored pattern it is,
  because one deliberately selects two tests. Five sites were wrong when it
  landed, and three of the five had the name truncated at a line boundary.

### Fixed

- **A stale profile is re-read, so the tier fields stop being frozen at add
  time.** `Tier`, `RateLimitTier`, `SeatTier` and `OrganizationUUID` were
  written once by `ccdad add` and by nothing else for the life of the
  installation — the daemon fetches a profile on every attribution pass but
  deliberately keeps only the uuid. That was invisible until a switch began
  writing `oauthAccount.seatTier` from the stored value: an account added before
  this tree read `seat_tier` at all carries the empty string, which Claude Code
  cannot tell from a pro or max seat that genuinely has none, so a money-metered
  enterprise seat silently loses the Opus tier its own predicate would grant it.
  The warning `ccdad add` prints when a profile lookup fails — "the tier will
  fill in on the first usage refresh" — was untrue until now. A poll that has
  already recorded its usage reading now re-reads the profile when the stored
  one is older than a day, on both paths that poll: the daemon's tick and
  `ccdad list --refresh`. The day is Claude Code's own figure for the same
  cached profile (`XH=86400000`), copied rather than invented so the two do not
  re-read on different schedules. A new `profile_fetched_at` stamp is what makes
  "never measured" distinguishable from "measured, and the answer was none" —
  without it the empty seat tier is ambiguous in exactly the way that decides
  behaviour. The re-read deliberately does NOT revise `Kind`: the usage axis
  owns that through `ApplyUsage`, and re-running add-time classification would
  overwrite a decision made on real window-and-overage evidence with the guess
  that preceded it. A failed profile lookup costs the poll nothing and does not
  move the stamp.
- **One document, one time zone, and it is the machine's.** `status.json`
  published `nextPollAt` two ways. An account on the ordinary cadence gets
  `now.Add(interval)` and carries the machine's offset; an account whose next
  look is pulled to a window rollover takes the instant
  `strategy.NextResetAmong` returns, which is parsed off the wire with an
  explicit `.UTC()` and comes back ending in `Z` — and `warmClamp` returns that
  target verbatim, because choosing an instant is its whole job. Observed on a
  live store: five poll times at `+09:00` beside one at `Z`. Read against a
  local wall clock the `Z` row looked nine hours overdue and stalled; it was
  four minutes in the FUTURE, and that account had just been polled. Nothing
  was ever wrong about the instants, and none of them moved. What changed is
  that the zone now belongs to the DOCUMENT rather than to the field, applied
  where each document is serialised rather than at every writer that computes a
  moment. `internal/zone` walks a value and renders every timestamp inside it
  in one location; the daemon's status writer and every `--json` payload go
  through it. The zone is the machine's, because every reader of these
  documents is on the machine that wrote them. An unset moment is the one thing
  left alone, and it has to be: a real location's offset in year 1 is its LMT
  and carries seconds — `Asia/Seoul` is `+08:27:52` — which RFC 3339 has
  nowhere to write, so a zero `time.Time` handed a zone came back from JSON
  fifty-two seconds away from zero and `IsZero` said false everywhere
  downstream. An instant with no moment in it has no zone to be in.

- **The usage history series carries one zone too, and the retention bound is
  restated for the machine it actually runs on.** `history.json` had the same
  split for the same reason: a sample's `at` is the reading's fetch time and
  carries the machine's offset, while the `reset` beside it came off the wire
  and carries `Z`. Measured on a live series before the fix — 292 timestamps at
  `+09:00` beside 870 at `Z`, describing one afternoon. It is rendered at its
  one serialiser now, like the others. This costs SIZE, and the cost is the
  reason the note is here rather than only in the code: an offset is six
  characters where `Z` is one, so a sample carrying three timestamps is 20
  bytes larger and the document at its retention cap goes from 1.33 MB to
  1.44 MB. That figure is `maxSamples`' whole justification, and it was
  previously measured on whichever zone happened to run the test — small in CI,
  where nothing sets `TZ`, and large everywhere else. It is now pinned to a
  non-UTC zone and stated as the worse of the two, because every machine that
  is not a CI runner is the worse of the two.

- **The usage cache renders ccdad's own moments in the machine's zone, and
  leaves the mirrored wire body exactly as it arrived.** `usage.json` is the one
  document in the store that CANNOT carry a single zone, and saying so is the
  point of the entry. Half of it is not ccdad's to render: the `snapshot` object
  is a verbatim mirror of the endpoint's body, written as the endpoint's own
  shape and read back through the parser a live response goes through, which is
  what lets a stored cache be compared against bodies recorded from the
  endpoint. A `resets_at` in a local offset would still parse and would end
  that. So the split stays and becomes a rule instead of an accident:
  `fetched_at`, `next_poll_at`, `stand_down_until` and the stamps inside `poll`
  and `probe` are the reader's, everything under `snapshot` is the wire's, and
  the boundary is an object a reader can see. It holds structurally rather than
  by a list of field names — every moment inside a snapshot lives in an
  unexported field behind that codec, so nothing that renders the document can
  reach one by accident. The one place those moments ARE rendered is the
  snapshot codec's own `fromTime`, and rendering it in anything but UTC fails
  the test that pins the mirror.

- **A switch carries the seat tier into Claude Code's own cached profile.**
  Claude Code decides which model tier a session defaults to with
  `Zu(){return Xe()==="enterprise"&&dO()==="enterprise_usage_based"}`, and the
  seat half of that reads `~/.claude.json`, not the credentials file:
  `dO(){return Dn()?.seatTier??null}` over
  `Dn(){return Zt()?k().oauthAccount:void 0}` (2.1.246). The minimal
  `oauthAccount` object a switch writes for an account with no captured
  snapshot named the organization but never the seat, so a money-metered
  enterprise seat fell out of the Opus tier Claude Code grants it alongside max
  and team-5x, and stayed out until its own next token refresh happened to
  repair the field — which can be most of an access token's lifetime away.
  `seatTier` is deliberately NOT one of the four fields whose combined presence
  makes that refresh skip re-fetching the profile, so writing it does not
  suppress the correction that follows. A seat that reports no tier — every pro
  and max account measured — still gets no key rather than an empty string.

- **The switch log line says why, not only what.** A daemon alternated between
  two accounts every 121 seconds for twenty-five minutes, and afterwards there
  was no way to say which margin kept clearing or on what numbers: the log
  recorded `switched to X` and nothing else, and the readings behind it aged out
  of the series before anyone looked. 121 s is `HoverCooldown` plus one tick, so
  the shape already said the ranking wanted to move on every evaluation — but
  the half that would have identified the cause was never written down, while
  sitting one line away in the same scope. The line now carries the account
  being left, the reason the ranking gave, and the binding window with its slack
  against the threshold it was measured on. An unreadable headroom renders as
  unreadable rather than as zeroes, because `slack=0 thr=0` reads like a
  measurement and is not one.

- **The login-surface flags name the surface instead of the billing.** `--console`
  said "for a credit-billed account", and an enterprise seat metered only in
  extra_usage credits reads itself as exactly that — while being a claude.ai seat
  whose meter happens to be money. Picking Console for it mints a credential from
  a different issuer, and the login SUCCEEDS, so the mistake surfaces later as an
  account that will not behave. `--console` now names `platform.claude.com` and
  says it does not mint claude.ai credentials; `--claudeai` names the seats it
  covers, enterprise included.

- **`ccdad add-token` says what it costs before somebody reaches for it.** It
  described itself as the thing to use on a headless machine, which is the
  sentence a person in a container reads right before creating an account that
  can never be ranked — it carries no refresh grant, so the daemon skips it on
  every poll and `ccdad list` says nothing. Headless was never the distinction
  either: `ccdad add --no-browser` completes a real login from a pasted code. The
  refusal a container hits when it has neither a browser nor a terminal now names
  the missing `-t` first, and the ranking cost second.

- **The README documents logging in inside a container.** It described exactly one
  way to provision a rankable account — `ccdad export --full` — so a reader with
  two environments arrived at copying one grant to both, which is the one shape
  that makes two holders race over a token only one of them can spend. A login
  per environment is now the first option, with the `-t` requirement, the
  five-minute default timeout, and the per-identity poll allowance two
  environments share.

- **An enterprise seat metered in credits is recognised, ranked and rendered.**
  Measured against two live `claude_enterprise` seats on 2026-08-26. Such a seat
  reports `seat_tier` `enterprise_usage_based`, `rate_limit_tier`
  `default_claude_zero`, every plan window `null` and an empty `limits[]`: money
  is its only meter. It was filed as a *subscription* at add time and stayed
  that way, and three separate things went wrong from there.

  `ccdad add` never calls the usage endpoint, so classification had only the
  profile — and the profile's `billing_type` on such a seat is
  `stripe_subscription_contracted`, one of the four values Claude Code itself
  reads as a subscription. The organization really does hold a contracted
  subscription; it is the *seat* that is metered per unit, so no allowlist on
  that field could ever have reached the account. Classification now asks
  whether the profile grants any plan-window entitlement at all, which is the
  same test Claude Code makes, and a seat that has none starts with the primary
  flag already set — a ceiling that gates spending *past* paid quota has nothing
  to gate on an account that has no quota, and at its default of `0` it would
  otherwise have gated the account shut forever.

  A misfiled account also stayed misfiled: `store.ApplyUsage` was written and
  tested for exactly this repair and was never called by anything. The daemon
  now applies it on every poll that produced a reading, which is also the only
  thing that has ever written the stored credit balance.

  Ranking then moved the wrong way rather than not at all. With no plan window
  to read, two such seats both measured as *unknown*, and the engine switched
  off the seat with 60% of its balance left onto the one that was fully drained.
  Separately, a fleet with no subscription accounts at all was frozen: the
  branch that keeps an engine from returning to a spent subscription pool also
  fired when there was no such pool to return to, so the credit pool was ranked,
  gated and then never consulted — permanently, while reporting a shortage of
  accounts the user does not have. `credit.max_auto_spend` still gates every
  move and still refuses at its default of `0`.

  Everything that renders read the plan-window axis unconditionally, so
  `status`, `list` and the dashboard printed `?` in the USED column for a seat
  the engine was ranking on a perfectly good percentage, the daemon published
  `unknown` for every account in the fleet, and the poll scheduler — whose two
  urgency bands both require a known reading — left a seat at 99% of its balance
  on the lazy cadence. One reading now produces one answer for all of them.
  `list`'s LEFT column keeps showing the balance and the account's own cap for
  such a seat rather than the bare percentage: `40%` and `795.23 left of 2000.00
  (USD)` are the same fact, and only one of them says whether to top up.

- **`scripts/ci.sh` answers 0, 1 or 2 and nothing else.** 2 means a check name
  the script does not have; a check that ran and found a real problem reports 1,
  whatever the tool underneath it exited with. It did not before, and not only
  in one place: `gofmt -l` exits 2 for a Go file it cannot parse, `go test
  -race` exits 2 with "-race is not supported on linux/386", `go build` exits 2
  on an unsupported `GOOS/GOARCH` pair, `claude plugin validate` propagates
  whatever it likes, a tool missing from `PATH` is 127 and a `git` that refuses
  is 128 — and every one of those left the script as the script's own exit
  code. One unparseable Go file made `ci.sh fmt` report the code that means
  "you typed the check name wrong".

- **A failing check no longer leaves its `::group::` fold open in the Actions
  log.** Of seven `group`/`endgroup` pairs, five could be aborted between the
  two halves *on the check's ordinary failure path* — an ordinary `vet` or
  `test` failure included — so the one line the reader came for sat inside a
  section that never closed and swallowed everything after it.

- **Three checks that reported success having looked at nothing.** `ci.sh fmt`
  built its file list through a process substitution, whose exit status `set
  -e` cannot see, so a `git` that refused printed its own `fatal:` and then
  `ci: no Go files to format` and exited 0. `cites` read its tracked list
  through a bare assignment and left with git's 128. And an arm of `cites`
  whose pattern the platform's regular-expression library rejects searched
  nothing and reported a clean tree: `git grep` exits 1 for "no matches" and
  128 for "I could not read that pattern", and both were being read as "no
  matches". That last one is not hypothetical here — macOS's git reads `\b` as
  a literal `b` rather than a word boundary, which once turned a whole arm of
  this check into a no-op on that leg while every test expecting a *miss* still
  missed.

## [0.8.0] — 2026-08-26

The release that puts `ccdad update` in users' hands, and the one that stops the
terminal being plain.

`ccdad update` finishes what 0.7.0 started. That release published a signature
over every release's checksums and nothing consumed it, so the point of shipping
one was still ahead of it: every upgrade in the field still meant re-running an
installer that checks a checksum and no signature. This one consumes it, at the
place it matters most — the command that overwrites the binary you are running.
The order is the design. The signature is verified before the checksum file is
read for anything, the staged binary is run once before it is renamed over the
live one, and every answer ccdad does not like is a refusal rather than a
replacement. There is no `--no-verify` and no `--insecure`, because a mirror
that does not carry the signature and an attacker who removed it are the same
bytes on the wire.

The second thing is colour. Every page ccdad draws — the dashboard, `list`,
`status`, `doctor`, `daemon status` — is painted from one palette and one glyph
set rather than from nine inline decisions, and `tui.theme` and `tui.glyphs` are
the two keys that say what that means on a given terminal. This one changes what
every existing user sees on every command, which is why both keys are opt-outs by
name and separately. Three things did not move: the layout, down to the column;
the rule that colour is never the only thing carrying a distinction; and
`--json`, which bypasses the colour writer everywhere it exists.

### Added

- **`ccdad update` replaces this binary with the latest signed release.**
  `ccdad update` resolves the latest release, downloads `sha256sums.txt` and
  `sha256sums.txt.minisig`, verifies that signature against a public key
  compiled into the binary, and only then reads the checksum row for this
  platform — the order matters, because a checksum file whose shape has been
  inspected is still a file somebody else wrote. The asset is then staged beside
  the binary, checked for size and digest, run once, and renamed over it. This
  is the first thing in ccdad that consumes the signature 0.7.0 started
  publishing; nothing did before it.
  `ccdad update --check` answers everything a full run answers except the three
  things only the download can tell you, and replaces nothing.
  `ccdad update --version <tag>` pins a release, including an older one — naming
  the tag is the consent for a downgrade, and without it a release older than
  the one running is refused. `upgrade` is an alias. There is no `--no-verify`
  and no `--insecure`: a mirror that does not carry the signature and an
  attacker who removed it are the same bytes on the wire. A build with no pinned
  key, a development build, and a Homebrew- or Scoop-owned install are each
  refused rather than replaced, as is an install directory ccdad cannot write
  to. A release whose signature does not verify is refused with a message that
  deliberately does **not** say to re-run the installer, because neither
  installer checks a signature and that path would accept the altered release on
  checksums the same attacker controls. The daemon is stopped first and started
  again from the new binary; inside a `ccdad run` session it is stopped and left
  stopped, and the next ccdad command in a normal shell brings it back.

  **The exit code is the contract, and `--check`'s is the opposite of the
  obvious guess.** `ccdad update --check` exits **0 when an update is
  available** and **3 when this machine is already on the newest release** — 3
  being "the world is already how you asked", the same code `ccdad daemon stop`
  answers with nothing to stop, and what makes `ccdad update --check && ccdad
  update` compose. A poller that reads any nonzero as broken will raise an
  alarm about a machine that is simply up to date. Beyond those two: **4** is
  every refusal ccdad *decided* on — each one named above, plus a signature
  that does not verify, a signature made with another key or for another
  release, a sums file whose shape or hash algorithm is wrong, a checksum row
  that is missing or does not match, an asset whose size does not match, a
  staged binary that would not run, and a downgrade nobody consented to. **1**
  is everything ccdad *could not do*: a discovery that produced no tag, a
  download that failed, a daemon that would not stop, a rename that failed. A
  reason nobody listed takes 1, so a refusal added later reports "ccdad could
  not do this" rather than accusing an origin.

  `--json` writes that same answer as an object on stdout instead of the
  sentences: `schemaVersion` 1, `currentVersion` and `updated` always, `reason`
  on anything but success, and `tag`, `targetVersion`, `resolvedLatest`,
  `updateAvailable`, `path`, `installDir`, `onPath` and `daemonRestarted` where
  the run got far enough for each to mean something — absent rather than false
  where it did not, which is the rule `unnamableWeeklyCaps` and `hover` already
  follow. It changes the representation and never the answer: the exit code is
  identical with and without it, the human words are suppressed rather than
  printed beside the payload, and every progress line goes to stderr, so stdout
  carries the object and nothing else.
- **Two more keys for what the terminal output looks like, `tui.theme` and
  `tui.glyphs`.** They join `mcp_switch_without_elicitation` as keys that
  govern a surface rather than the engine, and hover honours both rather than
  deriving them — hover is a policy for which account is live, and nothing
  about how a screen is painted follows from that. `tui.theme` takes `auto`,
  `dark`, `light`, `ansi` or `none`; `tui.glyphs` takes `auto`, `unicode` or
  `ascii`. Both default to `auto`.

  `theme=auto` does not mean the same thing on every surface. `ccdad tui` on
  an owned terminal asks — once, through bubbletea's own background-colour
  request — and resolves to `dark` or `light`. `ccdad list`, `status`,
  `doctor`, `daemon status`, and a redirected `ccdad tui` never ask: they take
  the dark default outright. A live dashboard can afford one query because it
  asks on the way in and then runs for minutes; a listing cannot, because the
  same query costs four seconds on a terminal that stays silent and each of
  these commands is its own process, so nothing carries a cached answer from
  one invocation to the next. `ccdad config set tui.theme light` is the
  standing opt-out for a light terminal, and it costs one line, once.
  `theme` never resolves to `ansi`: fitting a 24-bit colour to whatever a
  lower-colour terminal can show already happens on every render, so a
  terminal that cannot carry the full palette gets a downgrade of the same
  design rather than a different one. `ansi` is the opt-in for a user who
  would rather their own terminal theme owned the sixteen standard slots, and
  `none` emits no escape byte at all — the same thing `NO_COLOR` gets on a
  terminal, and what `ccdad mcp` gets unconditionally, regardless of what the
  environment says.

  `glyphs=auto` resolves to `ascii` on a Windows console whose output code
  page is not 65001, and whenever `RUNEWIDTH_EASTASIAN` is set — that
  variable makes eight of the frame and gauge glyphs two columns wide, and
  every page here is drawn to a measured width. An explicit value wins in
  both directions: `unicode` on a console that cannot carry it ships
  mojibake, and that is the user's own choice to make; `ascii` on a UTF-8
  console ships the plain `+--+` frame on request. Detection only ever
  resolves `auto`.

### Changed

- **The dashboard and every one-shot table are in colour now, and the frame,
  the gauges and the state markers are drawn with box-drawing characters.**
  This is the deliverable, not a side effect: `tui.theme` defaults to `auto`,
  which resolves to the dark palette on the overwhelming majority of
  terminals, and the glyph set changes under every existing user whose
  console can carry UTF-8. `ccdad config set tui.theme none` and
  `ccdad config set tui.glyphs ascii` put the old page back, separately.

  Two written comments held the plain page in place for several releases and
  both were false. The first said lipgloss v2 has no auto-adaptive fallback;
  what v2 actually removed is the global renderer, so the background-darkness
  boolean has to be threaded in from the program rather than consulted
  implicitly by a style, and every piece needed to do that was already in the
  pinned versions. The second said this repository emits no non-ASCII byte.
  Measured over string literals in shipped non-test Go, it emits 106 em
  dashes and 4 ellipses from 101 lines, most of them out of `ccdad doctor`
  and `ccdad run`. The real rule was package-local to `internal/tui` and
  `internal/view`, enforced by golden fixtures comparing bytes, and it is
  written down in `CONTRIBUTING.md` now rather than inferred from a diff.

  **The layout did not move.** The width ladder and the height ladder keep
  every rung and every constant they had, because the fixtures compare the
  coloured render with its escape sequences stripped, and that strip is
  byte-for-byte identical to the uncoloured render — through the styled
  table, a bordered frame around styled content, and the keybar. The one
  setting that would widen these glyphs to two columns selects the ASCII set
  instead of widening the frame.

  **Colour is never the only thing carrying a distinction.** Every state
  keeps a glyph everywhere and keeps its word wherever the STATE column
  survives — which is not everywhere: `ccdad list` and `ccdad status` carry
  no STATE column at any width, so the exhausted verdict is not painted at
  all rather than painted where nothing else says it. That is the reason
  there is no daltonised theme here, and the reason it is a defensible one:
  under simulated protanopia, deuteranopia and tritanopia at full severity,
  every pair of the five state roles stays at least 10 dE00 apart in both
  palettes. Deuteranopia binds tightest, at 10.25, and it clears without the
  glyph's help.

  **`text/tabwriter` is gone from the one-shot tables that used it**,
  replaced by a column helper that measures display width instead of rune
  count. This is not cosmetic and it is the reason the change is larger than
  a palette: tabwriter cannot see an SGR escape, so the moment one cell is
  coloured its column is padded for the escape bytes too, and the wrong way
  round — a styled cell counts as wide and gets less padding than the bare
  cell beside it. Measured on three ACCOUNT cells of which only the header
  was styled: the column came out 9 display columns wide on the header row
  and 31 on both data rows, a destroyed table, and in the piped case rather
  than the terminal one, because colour is stripped downstream of the
  layout — a redirected invocation got the wrecked padding with none of the
  colour that would explain it. The replacement is also East-Asian-width
  aware, which fixes a second, older bug: measured on an ACCOUNT column
  holding a fifteen-character ASCII address beside a three-character Hangul
  name, it starts the next column at display column 17 on both rows;
  tabwriter started it at 17 on the ASCII row and 20 on the Hangul one,
  hanging that row three columns right of the ones around it.

  **On Windows the console code page is read and never written.**
  `GetConsoleOutputCP` operates on the process's attached console, not on a
  handle, so a redirected `ccdad list > out.txt` launched from a console
  still has one — and setting the code page would reinterpret every
  non-ASCII byte every other process writes to that window for the rest of
  its life. A Korean user on `chcp 949` running `ccdad status` once would get
  mojibake out of everything else in that shell afterwards, including
  whatever `ccdad run` execs. So a non-UTF-8 console gets the ASCII glyphs
  and keeps its code page. A successful read proves capability and not
  outcome: a legacy conhost with a raster font draws boxes at 65001 exactly
  as it did at 437, which is what `tui.glyphs=ascii` is for; a redirected
  file that would have carried Unicode perfectly well is the cost of the
  fallback going the other way, and `tui.glyphs=unicode` overrides it by
  name.

  Nothing machine-readable changed. `--json` bypasses the colour writer on
  every command that has one, `ccdad mcp` resolves the `none` theme at the
  command tree's root rather than by being excluded by name — `NO_COLOR`
  does not beat `CLICOLOR_FORCE` off a terminal, so an environment carrying
  the force variable would otherwise have put escape bytes into MCP tool
  results — and stderr stays plain everywhere.

### Fixed

- **`ccdad status`'s labelled block ran off the side of a narrow terminal.**
  Measured on an 80-column terminal against a live fleet: `Runway:` 139 display
  columns, `Mode:` 124 in recovery, `Hover:` 100 whenever hover is on. The
  terminal folded each of them wherever its own right edge fell — mid-word,
  mid-sentence, and on the runway line inside `2026-08-26 17:21 KST`.

  All four labelled lines — `Daemon:`, `Active:`, `Hover:`, `Mode:` — and the
  `Runway:` line now wrap to the terminal and hang every line after the first
  under the value, nine columns in, so a continuation cannot be read as another
  label. `ccdad list` folds its own runway line the same way.

  The two wraps are separate on purpose. The labelled lines are prose and break
  at spaces. The runway line is a row of values whose spaces are inside them —
  a break at the one in `2026-08-26 08:19 KST` would produce a date that is not
  a date — so it breaks only at its own `·` separators, and a line that
  continues ends on the separator it broke at, which is how a continued line
  can be told from a finished one.

  Nothing is dropped at any width. A word, or a runway clause, wider than the
  terminal takes its own line and overflows rather than being cut: cutting
  produces a shorter value that reads as real, which is the one thing worse
  than a line that does not fit. A destination that is not a terminal — a pipe,
  a redirect, the file behind `>` — has no width, and receives every one of
  these lines exactly as it did before, byte for byte. The dashboard has always
  cut this line at its own edge and is unchanged: a page has a right edge it
  cannot spend, and a scrollback has one it can.

  The account table below the block is NOT covered by this. Its rows measured 84
  columns on the same run and it is built by a tabwriter: narrowing a table
  means dropping columns, which is a different decision.

- **0.7.0's own notes said the MCP server could not be launched. It can, and
  has been able to since 0.6.0.** The entry above describes the new `runway`
  measurement as reaching "a `runway` tool in the MCP server — which still has
  no verb to launch it", and README's *What is not here yet* carried the same
  claim in a stronger form. Both are false: `ccdad mcp` is registered on the
  root command, is printed by `ccdad --help`, serves sixteen tools over a real
  stdio handshake, and is documented in this file's own [0.6.0] entry and in
  README's commands table. The 0.7.0 entry is left as it was published — a
  changelog that edits its own history is worth less than one that admits to
  it — and README, which describes the present rather than the past, is
  corrected: the bullet is gone, because nothing it claimed to be missing is.

  The sentence was written against a survey of the tree taken about six hours
  before the commit that used it, and `ccdad mcp` landed in between. What made
  it survive review is that no test reads this file and none reads README, so a
  false sentence about a shipped verb costs nothing until somebody believes it.

- **The MCP tool table said fifteen tools and did not list `runway`.** The
  release's headline feature was absent from the only table describing what a
  Claude Code session can call. Sixteen, and it is a read. The count in the
  gate's own map and its test were correct throughout; only the prose was not.

- **`ccdad list` said LEFT was measured against hover's derived thresholds.** It
  is not, and never was: LEFT is `100 −` the reported window's utilization, with
  no threshold in it. What the derived thresholds decide is *which* window binds
  and therefore which one each row reports. A reader who did the subtraction the
  old sentence invited would have read a row showing 38% as 38 points of margin
  against a threshold of 99. The note now says the true thing, and a test pins
  the arithmetic rather than the wording.

- **The 0.7.0 entry's disk figure named the wrong cadence, and its retention
  derivation was a step short.** 250–430 KB for six accounts is what the series
  costs at the *fastest* cadence the poll policy permits — the 180 s floor — not
  at the normal one; at the 600 s idle cadence it is roughly a quarter of that,
  and the live store measured 32 KB for six accounts over 3 h 51 m. The eight
  hours of retention is not "the longest gap the poll policy can leave a
  six-account identity" but the four-hour measurement span *plus* that gap
  (3 h 18 m), rounded up to the next whole hour.

- **Every released heading in this file now has a link definition, and
  `[Unreleased]` compares against the last tag.** The reference block had not
  moved since 0.2.0, so `[Unreleased]` offered a diff five releases wide and the
  headings for 0.3.0 through 0.7.0 rendered as literal bracketed text linking
  nowhere. A test now fails on a heading with no definition, which is the only
  thing that will keep it from drifting again.

## [0.7.0] — 2026-08-25

Two things ccdad could not do before. It can now measure how fast the fleet is
actually spending and say when that runs out, rather than only reporting how
much of a window is gone; and a release now carries a claim about who made it,
not only that it arrived intact.

The first is a rate where there was only ever a level. A level cannot tell you
whether you are an hour from a stop or three days from one, and the daemon was
already taking the readings that would answer it — it just was not keeping them.
It keeps them now, and `ccdad runway` runs the rotation forward against the
measured rate rather than dividing burn by replenishment, because the two
disagree exactly where it matters. The same simulation answers how many accounts
would be enough. Unknown stays unknown throughout: a machine recording for ten
minutes is told it has no basis rather than handed a runway of forever.

The second is minisign. Every release now publishes `sha256sums.txt.minisig`
beside its checksums, and ccdad carries the public half in a constant a link
line cannot patch. Checksums say a download is intact; the signature says it is
ours — and the release it names, which matters because `sha256sums.txt` carries
no version of its own and an old release's checksums and signature would
otherwise stay a genuine, correctly signed pair forever. Nothing in ccdad
consumes the signature yet; it ships first so that signed releases exist before
anything requires one.

### Added

- **`ccdad runway` measures how fast the accounts are actually spending, and
  says when that runs out.** Every surface before this one reported a level —
  how much of a window is gone right now — and a level cannot tell you whether
  you are an hour away from a stop or three days. The daemon was already taking
  the readings; nothing was keeping them. It now appends each one to
  `~/.ccdad/history.json`, and `runway` measures the last four hours of them.
  A history file that cannot be read or parsed costs the rates and nothing else
  — every row still renders — but `ccdad status` and `ccdad list` now say so on
  stderr where they never had anything to say about this file, and `status` also
  carries it into `Notices`, so the dashboard spends a row on it. The series is
  kept for eight hours, which is the longest gap the poll policy can leave a
  six-account identity; that is 250-430 KB on disk for six accounts at the
  normal cadence and up to about 1.3 MB at the retention cap, rewritten on every
  poll and read whole by every command that forecasts.
  No extra request is made against the usage endpoint and no cadence changed:
  the file is written from the one place a fresh reading was already being
  stored.

  The answers come from running the rotation forward — one live login at a
  time, spending at the measured rate, taking each window's rollover as it
  arrives — rather than from comparing a burn rate against a replenishment
  rate. The two disagree often enough to matter, because replenishment counts
  accounts that are already out on the other axis and windows that reported no
  reset at all, and a run of the rotation reaches neither. So an axis whose
  arithmetic looks comfortable can still empty, and the table prints the
  comparison beside the verdict rather than as it.

  Unknown is never zero anywhere in this. A machine that has been recording for
  ten minutes is told it has no basis, rather than handed a burn of 0.0 and a
  runway of forever; the rate is absent from `--json` rather than present as a
  zero. Money fails closed the same way: a credit figure that cannot be
  assembled — two accounts billing in different currencies, an uncapped account
  that is spending — refuses instead of defaulting, because every default there
  makes the runway longer than it is.

  **One measurement is worth writing down, because it would have passed the
  whole suite.** The first design detected a window's rollover by asking whether
  its `resets_at` had changed. It changes on every poll. Two readings of the
  same account and the same window, taken 72 minutes apart on 2026-08-25, agreed
  on the minute and the second and disagreed in the microseconds — `.308482` and
  `.320288`. Three windows read out of a single response cluster their fractions
  within a few hundred microseconds of each other although their anchors are
  days apart, so the sub-second part is coming from the server's clock at
  request time and not from the window. Under an
  equality test every consecutive pair straddles a boundary, every segment holds
  one reading, and the measured rate is 0.0 pp/h for every account and every
  window, permanently, while the command prints "holds" over a pool being
  drained. Recorded resets are therefore truncated to the minute, and a rollover
  is a percentage that fell or a reset that moved forward by at least half the
  window's length.

  **It also answers how many accounts would be enough.** An `Accounts:` line
  under the axis block reads `5 usable, 9 needed to hold at this rate  (4
  more)`, and a fleet that already holds gets the same search downward — `3
  needed … (2 to spare)` — so slack is a figure rather than a feeling. The count
  comes from the same simulation the verdicts do, re-run with seats the fleet
  does not have yet appended, and never from burn divided by replenishment: two
  mechanisms would be free to print "runs dry" and "you have enough accounts" on
  adjacent lines. It is measured at the upper end of the band for the reason
  "holds" is, so it is the number that is provably enough; which of the two axes
  asked for the extra seat is measured too, and reported. The search stops at
  256 and says `more than 256`, because past that the answer is a measurement
  problem rather than a purchase.

  Making that answerable fixed a real fleet as well as a hypothetical one. A
  window at 0% whose reading carried no `resets_at` has never been spent
  against, and the model froze it — supplying its hundred points once and never
  again. That is what a fresh account is made of, and it is also an account
  added an hour ago and not yet used: the runway understated a pool its owner
  had just enlarged. Such a window now starts its cycle when something first
  burns it, which is what the endpoint does. A window *above* zero with no reset
  is the other state and is unchanged — that is a `resets_at` this build could
  not read, not an unused window, and freezing it can only shorten the runway.

  The same measurement appears as one `Runway:` line under `ccdad status`, under
  `ccdad list` and on the terminal dashboard, as a `forecast` object in all
  three `--json` payloads, and as a `runway` tool in the MCP server — which
  still has no verb to launch it. That one-line summary gains `· need 9 (4
  more)` when the fleet is short and nothing when it holds, and the `fleet`
  object gains `accountsUsable`, `accountsNeeded`, `accountsNeededBy` and
  `accountsNeededCapped`, the middle two absent rather than zero when there was
  no basis to search from. Read the last one: when it is true the search hit its
  ceiling and `accountsNeeded` is a bound rather than a count, so a program that
  ignores it reads `256` as "buy 256" instead of "more than 256" — the human
  form says so in words and the machine form says so in that flag.
  `ccdad runway --json --out PATH` writes the document to a file at mode `0600`
  with nothing on stdout, the way `ccdad export --out` does; `--out` without
  `--json` is a usage error, because this command has two representations and a
  destination does not choose between them. `ccdad doctor` gains
  a `history` check with three levels rather than two: an absent file is fine,
  an unparseable one is a warning, and one that cannot be READ at all — a
  permission problem, a directory in its place, an I/O error — is a **failure**,
  so `ccdad doctor` now exits 1 on it where the same machine passed before. That
  is deliberately harsher than the usage cache, whose identical breakage is only
  ever a warning: nothing rewrites a series it could not read first. Anyone with
  `ccdad doctor` in a health check should know that a file which did not exist
  before this release can now turn it red. And `ccdad uninstall` counts the file among the markers that identify a ccdad
  store, so a directory holding only this one is still recognised rather than
  refused as somebody else's.

- **Releases are signed.** Every release now publishes
  `sha256sums.txt.minisig` beside its checksums: a minisign signature over the
  sums file, made with a key whose public half is committed at the repository
  root as `ccdaddy.pub`. Checksums say a download is intact; the signature says
  it is ours. Check it with the stock `minisign -Vm sha256sums.txt -p
  ccdaddy.pub` — and read the `Trusted comment:` line, which names the release
  the signature was made for. That field exists because `sha256sums.txt` carries
  no version of its own, so without it an old release's checksums and signature
  would stay a genuine, correctly signed pair forever and could be served as a
  new release. Do not pass `-H`: it requires a prehashed signature and rejects
  the legacy form published here. One signature over the sums file rather than
  six over the binaries, because six signatures are six chances to ship five.
  Nothing in ccdad consumes the signature yet; the verifier ships now so that
  signed releases exist before anything requires one.

### Fixed

- **The dashboards name hover rather than the strategy hover overrode.** Hover
  derives every threshold for itself and forces the ranking strategy to
  `headroom`, and `ccdad config list` has always marked the key `overriding`.
  The dashboard header did not: bare `ccdad` and `ccdad tui` printed
  `Strategy: consume-first` for a value nothing was reading, which is precisely
  what hover being OFF looks like. The header now reads `Strategy: hover`.
  `ccdad status` gains a `Hover:` line, placed above the `Mode:` line it
  explains — the mode reads `headroom` under hover because hover forced it, so a
  reader who configured consume-first was being shown a mode they never asked
  for with no reason for it anywhere on the page. `ccdad status --json` carries
  `hover: true` while it is on, under the rule `unnamableWeeklyCaps` already
  follows: absent when it is the boring default, so the contract stays additive
  and `schemaVersion` stays 1. `ccdad list` says on stderr that its LEFT column
  was measured against thresholds hover derived, because a row held to 93 with
  `threshold = 80` in the file is otherwise indistinguishable from a defect.

- **`ccdad switch --strategy` says when hover discarded it.** Hover derives the
  ranking for itself, and `Options.withHover` overwrites the strategy with
  `headroom` before the pass runs — so the flag ranked exactly as though it had
  never been typed, and the command printed only `Switched to …`. It now names
  the override on stderr. This is the rule `switch` already applies to an
  unplaceable `--model`, and it bites harder here: `--strategy` is *required* by
  the targetless grammar, so under hover every attended `ccdad switch` types a
  value the engine drops. Refusing the flag is not available for that same
  reason — it would leave no way to run a targetless switch at all while hover is
  on — so the note points at the two things that do work: name an account, or
  turn hover off.

## [0.6.1] — 2026-08-25

Hover promised to hold every account to the share of its own window that had
elapsed, and a clamp on the derived threshold meant it did not. The clamp landed
on whichever account was furthest through its own window -- the one whose quota
expires soonest, and the one the pace target exists to send work to -- and above
it the elapsed term was thrown away and the pool was ordered on raw utilization
instead. Measured on three accounts resetting one, three and five days out,
taking the clamp off halved how far the fleet drifted from its own pace lines,
on fewer switches rather than more.

### Fixed

- **Hover no longer clamps a derived threshold, and that is what makes it hold
  the pace line it promises.** The threshold is `elapsed% + 100/usable`; it used
  to be clamped to 99. The clamp fires when the elapsed share plus the pool share
  passes the cap — which is to say on whichever account is furthest through its
  own window, exactly the account whose quota expires soonest. Above it the
  elapsed term was gone and slack collapsed to `99 − utilization`, so the pool
  was ordered on raw utilization for precisely the accounts that mattered most.
  Measured on three accounts resetting one, three and five days out, driven
  through the engine on thirty-minute ticks for five days: the clamp was live on
  240 of 240 ticks, and the fleet's final-day drift from its own pace lines
  halved when it came out — mean spread `20.14 → 9.89` — on **fewer** switches
  (12 → 9). `TestHoverHoldsEveryAccountNearItsOwnPaceLine` is that measurement.
  A pace target above 100 is meaningful: it means no restraint, because there is
  nobody to hand the work to. `ccdad hover status` still stops its THRESHOLD
  column at `100%` and prints a footer naming the rows where that is the ceiling
  rather than the derived figure; `--json` carries the true number, because
  `slack` is measured against it.
- **An account with nothing left counts as spent even when its pace target is
  above 100.** `Spent` now reads `slack < 0 || MinPct <= 0`. This used to fall
  out of the clamp — `100 > 99` guaranteed a used-up window reported negative
  slack — and with the clamp gone it has to be said. Without it, one empty
  account would report positive slack, land in the roomy tier ahead of every
  account that actually has quota, and, because `allOver` is built from this
  predicate, take recovery mode away from the whole pool.
- **A weekly window with nothing left in it counts as the account's floor even
  when its pace target is above 100.** `Headroom.Floor` — the weekly that has to
  clear before an account is usable again — was selected by "past the number it
  was given", which under hover is a *pace* target rather than a stop line, and
  one that runs past 100 late in a window. So a blown weekly reported positive
  slack and was not a floor at all, and `RecoversAt` then named whichever window
  happened to bind: an account whose week is gone for thirteen hours was reported
  as coming back in forty-five minutes, on the strength of a five-hour window.
  The test is now "past its number **or** empty", which is purely additive — every
  window that was a floor still is, so a configured `window_threshold` behaves
  exactly as before — and an empty floor outranks a merely-tripped one, because
  it is the empty one that decides when the account returns.
- **`usage.PaceOf` no longer reports an exhaustion in the past for a very
  distant projection.** The seconds were converted to a `time.Duration` before
  being multiplied by `time.Second`, so the product overflowed `int64` past
  ~9.22e9 s and wrapped: a seven-day window 65% elapsed at 0.003% used reported
  exhaustion in **1857** and `willLastToReset = false`, the exact opposite of the
  truth, and that answer reaches a switch decision through the pre-emptive rule.
  It saturates now.

### Changed

- **Hover's hysteresis margin is `3` points of slack, down from `5`.** Five sat
  above the spread a real pool shows. Two accounts whose binding windows are the
  same length have thresholds that rise at the same rate, so the gap between
  their slacks is frozen against the clock — the only thing that closes it is
  the live account spending another point, which is a point spent on whichever
  of the two has less left. A fleet of six measured on 2026-08-25 sat four
  points apart under the five-point margin, holding the engine on an account
  with ten points of raw room while the candidate held thirty. Nothing else
  moves: `HoverCooldown` is still what bounds the flap rate, which is the job a
  margin does badly. Only `hover` is affected; `hysteresis_pct` outside it keeps
  its `10` default and whatever you set.

## [0.6.0] — 2026-08-25

Three things the design spec named "extras, in order: TUI, MCP, GUI widget" —
the first two ship, none of the priority order changed. A terminal dashboard,
an MCP server most of whose tools can rewrite the live login or the daemon,
and the plugin that wires the server into Claude Code without shipping a
binary through it.

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

[Unreleased]: https://github.com/Kweiza/ccdaddy/compare/v0.9.5...HEAD
[0.9.5]: https://github.com/Kweiza/ccdaddy/compare/v0.9.4...v0.9.5
[0.9.4]: https://github.com/Kweiza/ccdaddy/compare/v0.9.3...v0.9.4
[0.9.3]: https://github.com/Kweiza/ccdaddy/compare/v0.9.2...v0.9.3
[0.9.2]: https://github.com/Kweiza/ccdaddy/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/Kweiza/ccdaddy/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/Kweiza/ccdaddy/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/Kweiza/ccdaddy/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/Kweiza/ccdaddy/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/Kweiza/ccdaddy/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/Kweiza/ccdaddy/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/Kweiza/ccdaddy/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/Kweiza/ccdaddy/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/Kweiza/ccdaddy/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/Kweiza/ccdaddy/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Kweiza/ccdaddy/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0-rc1...v0.1.0
[0.1.0-rc1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.1.0-rc1
