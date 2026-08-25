package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/view"
)

// contentCadence and reloadCadence are the two clocks the dashboard runs on,
// and the split between them is a cost decision rather than a preference.
//
// The content clock does no I/O at all: it advances the moment the page
// describes, so every age and every countdown on screen moves, and redraws
// from the snapshot already in hand. The reload clock is the only thing on a
// timer that reads anything.
//
// A naive one-second full refresh costs roughly six file reads, two chmod
// calls and a kernel flock EVERY SECOND for as long as the dashboard is open:
// store.Open does a MkdirAll plus two Chmod(0700) on every call, and the
// daemon probe is a file read plus a shared flock with up to three attempts a
// hundred milliseconds apart. Ten seconds is a tenth of that, and it is the
// cadence the daemon indicator wants -- which is the one fact a dashboard
// exists to answer, and the one that goes wrong without anybody pressing a
// key.
//
// A DEPARTURE, stated rather than left to be discovered. The design this was
// built from budgets four different cadences for four different sources: the
// cheap documents every second and a half, the store no oftener than every
// thirty seconds, the daemon probe every ten, and the ranking pass at startup
// and after a switch and NEVER on a timer. The seam this package is given
// cannot separate them -- Options.Load is ONE call and it does all four -- so
// the loop makes that call at the cadence of the most urgent thing inside it
// and pays for the rest. What that costs against the budget: the store is
// opened three times as often as its own row allows, and the ranking pass runs
// on a timer, which its row forbids outright. What it buys: a dashboard whose
// daemon line cannot be thirty seconds out of date, and a load that is one
// read of one authority rather than four reads this package assembled itself.
// Splitting them for real means splitting the seam, which is not this file's
// to split.
//
// Nothing here ever reaches the network. The usage endpoint allows roughly
// 28-30 requests per identity per rolling hour on a sliding window, and quota
// only moves every minute to ten anyway, so one dashboard left open would
// saturate an account for a full hour and learn nothing for it.
const (
	contentCadence = 1500 * time.Millisecond
	reloadCadence  = 10 * time.Second
)

// logLines is how much of the daemon log the [D] screen carries. It is the
// screen's own bound and not the reader's: TailLog also bounds the BYTES it
// looks at, and both bounds are needed -- the seek is what makes the read
// cheap on a timer, and this is what the screen has room for.
const logLines = 10

// tick is tea.Tick behind a package var.
//
// Every poll is rescheduled from Update through this rather than driven by a
// goroutine ticker, so at most one of each is ever outstanding. A hand-rolled
// `go func() { for { …; p.Send(…) } }()` piles up for the whole span the add
// key releases the terminal -- the event loop is blocked until the login exits
// -- and then floods the loop the moment it comes back.
//
// It is a var rather than a direct call because the alternative is a test that
// waits ten real seconds to find out whether a successor was scheduled.
var tick = tea.Tick

// contentMsg is the cheap clock: no read, just a newer now.
type contentMsg time.Time

// reloadMsg is the expensive clock: one Load, and the next one scheduled.
type reloadMsg time.Time

// refreshMsg is one Load with no clock behind it -- the startup read, sent
// from Init because Init returns a command and not a model, and the in-flight
// bookkeeping below lives on the model.
type refreshMsg struct{}

// loadedMsg is one Load's whole result, error included. The error is carried
// rather than dropped because a failed refresh is a thing the page says out
// loud -- see Model.AfterLoad, which is the rule for it.
type loadedMsg struct {
	snap view.Snapshot
	err  error
}

// ranMsg is one command's whole outcome, on its way back from the goroutine
// the executor ran in.
type ranMsg struct{ res result }

// logMsg is the bounded log read, on its way back from the same.
type logMsg struct {
	lines []string
	err   error
}

// screen is what the terminal is showing. Every one of them except screenPage
// is drawn INSTEAD of the page rather than over it, and that is what keeps
// them out of the height ladder: a picker composed into the page would have to
// be budgeted for by a ladder that was measured without it, and at the sizes
// where the ladder is already dropping blocks there is nothing left to take.
type screen int

const (
	screenPage screen = iota
	screenPicker
	screenPanel
	screenDaemon
	screenHelp
)

// App is the dashboard as a running program: the page, the injected world, and
// which of the five screens is showing.
//
// The page itself is Model, which is pure and stays pure. Everything with a
// clock, a terminal or a signal disposition is here.
type App struct {
	m    Model
	opts Options

	scr  screen
	pick picker // valid while scr is screenPicker
	res  result // a command's own bytes, while scr is screenPanel
	note string // a sentence this package wrote, while scr is screenPanel
	// back is where dismissing the panel goes. A daemon key runs its command
	// from the daemon screen, and landing on the page afterwards would make
	// the reader find their way back to the screen they were reading.
	back screen

	// log and logErr are the [D] screen's bounded log read, kept here because
	// that screen is handed its lines rather than a path -- nothing inside it
	// may reach the filesystem.
	log    []string
	logErr error

	// loading is whether a read is already in flight and pending is whether
	// something asked for another while it was. One outstanding read at a time
	// is what stops a held-down refresh key from reproducing the very cost
	// profile the split cadence exists to avoid, and it is what lets
	// Options.Load stay a function nobody ever promised was safe to call from
	// two goroutines at once.
	loading bool
	pending bool

	// running is whether a command is already in flight. A terminal repeats a
	// held key, and one held x on the daemon screen would otherwise be N
	// concurrent `ccdad daemon stop` invocations, each of them a fresh command
	// tree reaching for the same locks.
	running bool

	// hasLoaded is whether any read has ever succeeded. Until one has there
	// are no last good numbers, and a failure has to say something else.
	hasLoaded bool
}

// neverLoaded is what a failure says while there is nothing behind it. The
// ordinary failed-refresh notice promises the last good numbers, and an empty
// table under that promise reads as "you have no accounts" -- unknown as zero,
// which is the one thing this binary refuses everywhere.
const neverLoaded = "could not read the accounts, and there is no earlier reading to show: "

// newApp is the one construction site. The page starts empty and at zero size:
// the first WindowSizeMsg gives it a terminal and Init's own Load gives it
// something to draw, and until both have arrived the floors render what the
// page needs rather than a made-up size.
func newApp(o Options) App {
	return App{m: newModel(view.Snapshot{}, 0, 0), opts: o}
}

// Init loads once and starts both clocks.
//
// The startup read goes through a message rather than straight to the seam,
// because Init hands back a command and not a model -- and the flag that says
// a read is in flight lives on the model.
func (a App) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return refreshMsg{} }, a.tickContent(), a.tickReload())
}

// Update is the whole event loop. Every arm that schedules ends by returning
// the next tick, which is what makes "at most one outstanding" a property of
// the code rather than of the timing.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.m.Width, a.m.Height = msg.Width, msg.Height
		a.m = scrolled(a.m)
		return a, nil

	case contentMsg:
		// No read. The snapshot's own moment is what every age and countdown
		// on the page is measured against, so advancing it is the whole of a
		// content refresh -- and it stays honest while a reload is failing,
		// because a reset time is absolute and an age is supposed to grow.
		a.m.Snap.Now = a.opts.Now()
		return a, a.tickContent()

	case reloadMsg:
		a, cmd := a.reloading()
		cmds := []tea.Cmd{cmd, a.tickReload()}
		if a.scr == screenDaemon {
			cmds = append(cmds, a.tailLog())
		}
		return a, tea.Batch(cmds...)

	case refreshMsg:
		return a.reloading()

	case loadedMsg:
		a.loading = false
		switch {
		case msg.err == nil:
			a.hasLoaded = true
			a.m = a.m.AfterLoad(msg.snap, nil)
		case a.hasLoaded:
			a.m = a.m.AfterLoad(msg.snap, msg.err)
		default:
			// One line, replaced by the next attempt rather than stacked, and
			// gone the moment a read succeeds and replaces the snapshot whole.
			a.m.Snap.Notices = []string{neverLoaded + msg.err.Error()}
		}
		a.m = scrolled(a.m)
		if a.pending {
			a.pending = false
			return a.reloading()
		}
		return a, nil

	case logMsg:
		a.log, a.logErr = msg.lines, msg.err
		return a, nil

	case ranMsg:
		// A command that ran is a command that may have changed something, so
		// the page is re-read now rather than at the next tick -- and so is the
		// log, when the key that ran it was one of the daemon screen's.
		a.running = false
		next, cmd := a.showing(msg.res).reloading()
		if next.back == screenDaemon {
			return next, tea.Batch(cmd, next.tailLog())
		}
		return next, cmd

	case addFinishedMsg:
		// An account may have been added, and the login's own prose is already
		// gone from the terminal it was holding -- so what is left to say is
		// how it ended, and the rows are read again to find out whether it
		// added anybody.
		return a.saying(addOutcome(msg.err)).reloading()

	case tea.KeyPressMsg:
		next, cmd, _ := a.key(msg)
		return next, cmd
	}
	return a, nil
}

// View is two lines, and that split is the whole reason the page is a pure
// string: everything above this point can be asserted with a string
// comparison, and nothing below it can be.
//
// AltScreen is a declarative field on the returned view. There is no
// WithAltScreen option to pass to the program, and inline is the default.
func (a App) View() tea.View {
	v := tea.NewView(a.body())
	v.AltScreen = true
	return v
}

// body is which screen is drawn. The page is Model's; the other four are cut
// to the terminal here.
func (a App) body() string {
	switch a.scr {
	case screenPicker:
		return fit(a.pick.Body(a.m.Width), a.m.Width, a.m.Height)
	case screenPanel:
		text := a.note
		if text == "" {
			text = a.res.Body(a.m.Width)
		}
		return fit(text+"\n\nesc  back", a.m.Width, a.m.Height)
	case screenDaemon:
		return a.daemonScreen().Body(a.m.Width, a.m.Height)
	case screenHelp:
		return fit(a.m.Help.FullHelpView(a.keys().FullHelp())+"\n\nesc  back", a.m.Width, a.m.Height)
	}
	return a.m.Body()
}

// daemonScreen is the [D] screen with everything it may not read for itself
// handed to it.
//
// The credential home arrives in TWO pieces, and that is what makes the
// warning it feeds safe to turn on. One is this process's own resolution,
// which is an environment read and therefore package cli's to make. The other
// is the comparison: ccdad manufactures two spellings of that path itself --
// every daemon it spawns is handed an absolute, symlink-resolved one, while a
// shell's own spelling comes back untouched -- so a trailing slash or a
// symlink makes two names for one directory, and answering honestly means
// asking the filesystem. `doctor` asks the same question through
// internal/credhome and says beside it what a string compare would cost: the
// warning printed on every run forever, at a user whose daemon is driving
// exactly the right directory. Neither half may be produced here, so both come
// down through Options and this file only carries them across.
func (a App) daemonScreen() daemonScreen {
	return daemonScreen{
		Report:         a.m.Snap.Report,
		Rows:           a.m.Snap.Rows,
		Log:            a.log,
		Now:            a.m.Snap.Now,
		LogErr:         a.logErr,
		CredentialHome: a.opts.CredentialHome,
		SamePath:       a.opts.SamePath,
	}
}

// keys is the key map as it stands right now.
//
// The start key is disabled when the lock could not be probed, and that is
// applied WHATEVER is showing rather than only on the screen that offers it:
// it is a fact about the lock and not about the screen, and applying it only
// there would let the one full help view in this program advertise a key that
// does nothing. An advertised key that does nothing is worse than one that is
// not advertised.
func (a App) keys() KeyMap {
	return a.daemonScreen().Keys(a.m.Keys)
}

// key routes one keypress, and reports whether anything in this program
// answers to it.
//
// The third return is not for Update, which ignores it. It is what makes
// Handles below a question asked of this switch rather than a second copy of
// it: a predicate that re-listed the keys would go on passing for a dashboard
// that had stopped handling any of them.
func (a App) key(msg tea.KeyPressMsg) (App, tea.Cmd, bool) {
	k := a.keys()

	// ctrl+c is checked before any screen gets a look. While nothing is
	// released there is no scoped trap to fight and signals are not being
	// ignored, so it is an ordinary key here -- but it is the key every
	// terminal user tries first, and a screen that ate it would strand
	// somebody inside a full-screen program.
	if msg.String() == "ctrl+c" {
		return a, tea.Quit, true
	}

	switch a.scr {
	case screenPicker:
		return a.pickerKey(msg, k)
	case screenPanel, screenHelp:
		switch {
		case key.Matches(msg, k.Quit):
			return a, tea.Quit, true
		case key.Matches(msg, k.Esc, k.Enter):
			return a.dismissed(), nil, true
		case a.scr == screenHelp && key.Matches(msg, k.Help):
			return a.dismissed(), nil, true
		}
		return a, nil, false
	case screenDaemon:
		return a.daemonKey(msg, k)
	}
	return a.pageKey(msg, k)
}

// pickerKey is the two-keystroke confirm. An open picker eats every key: one
// keystroke never moves a credential, and a key that fell through to the page
// underneath would act on something the user cannot see.
func (a App) pickerKey(msg tea.KeyPressMsg, k KeyMap) (App, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, k.Enter):
		argv := a.pick.Chosen()
		a.scr = screenPage
		if argv == nil {
			// An empty store is a real state, and enter on it chooses
			// nothing rather than running a command with no target.
			return a, nil, true
		}
		a, cmd := a.starting(argv)
		return a, cmd, true
	case key.Matches(msg, k.Esc):
		a.scr = screenPage
		return a, nil, true
	case key.Matches(msg, k.Up):
		a.pick = a.pick.Move(-1)
		return a, nil, true
	case key.Matches(msg, k.Down):
		a.pick = a.pick.Move(1)
		return a, nil, true
	}
	return a, nil, false
}

// daemonKey is the [D] screen's own three, plus the two ways off it. Each one
// fires once per keypress, through the command tree, with the outcome reported
// inline: no retry and no timer, because a lock that could not be probed is
// not an invitation to keep trying.
func (a App) daemonKey(msg tea.KeyPressMsg, k KeyMap) (App, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, k.Start):
		a, cmd := a.starting([]string{"daemon", "start"})
		return a, cmd, true
	case key.Matches(msg, k.Stop):
		a, cmd := a.starting([]string{"daemon", "stop"})
		return a, cmd, true
	case key.Matches(msg, k.Restart):
		a, cmd := a.starting([]string{"daemon", "restart"})
		return a, cmd, true
	case key.Matches(msg, k.Esc):
		a.scr = screenPage
		return a, nil, true
	case key.Matches(msg, k.Quit):
		return a, tea.Quit, true
	case key.Matches(msg, k.Refresh):
		a, cmd := a.reloading()
		return a, tea.Batch(cmd, a.tailLog()), true
	}
	return a, nil, false
}

// pageKey is the dashboard's own keys.
func (a App) pageKey(msg tea.KeyPressMsg, k KeyMap) (App, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, k.Quit):
		return a, tea.Quit, true

	case key.Matches(msg, k.Add):
		if !a.opts.StderrTTY {
			return a.saying(addNeedsStderr), nil, true
		}
		c, err := addChild()
		if err != nil {
			return a, func() tea.Msg { return addFinishedMsg{err: err} }, true
		}
		return a, tea.ExecProcess(c, func(err error) tea.Msg { return addFinishedMsg{err: err} }), true

	case key.Matches(msg, k.Switch):
		a.pick = switchPicker(a.m.Snap.Rows, a.m.Cursor)
		a.scr = screenPicker
		return a, nil, true

	case key.Matches(msg, k.Strategy):
		a.pick = strategyPicker(a.m.Snap.Strategy)
		a.scr = screenPicker
		return a, nil, true

	case key.Matches(msg, k.Daemon):
		a.scr = screenDaemon
		return a, a.tailLog(), true

	case key.Matches(msg, k.List):
		if a.m.Set == SetStatus {
			a.m.Set = SetList
		} else {
			a.m.Set = SetStatus
		}
		a.m = scrolled(a.m)
		return a, nil, true

	case key.Matches(msg, k.Up):
		a.m.Cursor--
		a.m = scrolled(a.m)
		return a, nil, true

	case key.Matches(msg, k.Down):
		a.m.Cursor++
		a.m = scrolled(a.m)
		return a, nil, true

	case key.Matches(msg, k.Refresh):
		a, cmd := a.reloading()
		return a, cmd, true

	case key.Matches(msg, k.Help):
		a.scr = screenHelp
		return a, nil, true

	case key.Matches(msg, k.Esc):
		// Already home. The key is answered rather than ignored so that the
		// help view does not advertise a binding that does nothing here.
		return a, nil, true
	}
	return a, nil, false
}

// Handles reports whether the dashboard does something with a binding, on any
// of its screens.
//
// It exists because a keybar that offers a key the model ignores is a string
// painted on a terminal that nothing else in this repository reads, and the
// test that catches it lives in the package that registers the command. It
// runs the binding's own keys through the same switch Update runs rather than
// re-listing them: a predicate built from a second list would go on passing
// for a dashboard that handled nothing.
//
// It asks about every screen because the help view advertises keys that live
// on one of them -- enter belongs to a picker, and three of them belong to the
// daemon screen and are offered nowhere else.
func Handles(b key.Binding) bool {
	for _, a := range probes() {
		for _, name := range b.Keys() {
			if _, _, ok := a.key(keyPress(name)); ok {
				return true
			}
		}
	}
	return false
}

// probes is one App in each state a keypress can arrive in.
//
// The daemon screen is probed with a daemon that IS running, because an
// unprobed lock disables the start key -- which is a fact about that moment
// rather than about whether the key exists at all.
func probes() []App {
	base := newApp(Options{Now: func() time.Time { return time.Time{} }})
	base.m.Snap.Report.State = daemon.DaemonRunning

	pick := base
	pick.scr, pick.pick = screenPicker, switchPicker(nil, 0)
	panel := base
	panel.scr = screenPanel
	engine := base
	engine.scr = screenDaemon
	long := base
	long.scr = screenHelp
	return []App{base, pick, panel, engine, long}
}

// namedKeys is every non-printable key this program binds, by the name the
// binding spells it with. It is not a general parser: a name it does not know
// produces a message that matches nothing, which is the honest answer for a
// key this program could not have bound in the first place.
var namedKeys = map[string]rune{
	"up":    tea.KeyUp,
	"down":  tea.KeyDown,
	"esc":   tea.KeyEscape,
	"enter": tea.KeyEnter,
}

// keyPress is the message a terminal would send for one key name. Matching is
// done on the string form, so what matters is that String() comes back as the
// name it went in as.
func keyPress(name string) tea.KeyPressMsg {
	if rest, ok := strings.CutPrefix(name, "ctrl+"); ok && len([]rune(rest)) == 1 {
		return tea.KeyPressMsg{Mod: tea.ModCtrl, Code: []rune(rest)[0]}
	}
	if code, ok := namedKeys[name]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	if r := []rune(name); len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: name}
	}
	return tea.KeyPressMsg{}
}

// reloading asks for one whole read of the documents the page draws.
//
// It is one call because that is the seam: the snapshot is built by the
// command tree, which owns the read order and the notice stream, so a loop
// that assembled its own would be a second place a number could be derived
// differently.
//
// At most one is ever in flight. A second request while one is running is
// REMEMBERED rather than dropped -- the thing that asked usually asked because
// something just changed, and the read already running may have started before
// it did -- and issued the moment the first comes back.
func (a App) reloading() (App, tea.Cmd) {
	if a.loading {
		a.pending = true
		return a, nil
	}
	a.loading = true
	load, now := a.opts.Load, a.opts.Now
	return a, func() tea.Msg {
		snap, err := load(now())
		return loadedMsg{snap: snap, err: err}
	}
}

// starting puts one command through the injected executor, on the command
// goroutine rather than the event loop's, so a slow command cannot stop the
// page from redrawing or the quit key from arriving.
//
// One at a time, for the reason the running flag gives: a held key repeats,
// and the keys that come through here are the ones that change something.
func (a App) starting(argv []string) (App, tea.Cmd) {
	if a.running {
		return a, nil
	}
	a.running = true
	ex := a.opts.Exec
	return a, func() tea.Msg { return ranMsg{res: run(ex, argv)} }
}

// tailLog is the bounded read behind the [D] screen.
//
// It is the one read in this package that does not arrive through Options, and
// it is an exception on purpose. The rule that keeps this package pure is
// about FIGURES: every number on every screen comes from one authority, read
// once, through the snapshot. These are the daemon's own lines, shown as
// lines, and nothing on any screen is computed from them -- so there is
// nothing here for a second source to disagree with.
func (a App) tailLog() tea.Cmd {
	return func() tea.Msg {
		lines, err := daemon.TailLog(logLines)
		return logMsg{lines: lines, err: err}
	}
}

func (a App) tickContent() tea.Cmd {
	return tick(contentCadence, func(t time.Time) tea.Msg { return contentMsg(t) })
}

func (a App) tickReload() tea.Cmd {
	return tick(reloadCadence, func(t time.Time) tea.Msg { return reloadMsg(t) })
}

// showing puts a command's own output on screen, verbatim.
func (a App) showing(r result) App {
	a.back = returnTo(a)
	a.res, a.note, a.scr = r, "", screenPanel
	return a
}

// returnTo is where dismissing the panel about to open should go.
//
// It is never the panel itself. A second outcome can land while one is already
// showing -- a login finishing behind a command's result, say -- and a panel
// whose way out is the panel is one esc can never leave.
func returnTo(a App) screen {
	if a.scr == screenPanel {
		return a.back
	}
	return a.scr
}

// saying puts a sentence this package wrote on screen, for the two occasions
// where there was no command to run: the add key refusing a redirected stderr,
// and how a login that held the terminal ended.
//
// It is a panel rather than a line on the notice rung, and that is a departure
// worth naming. The notice rung belongs to the snapshot, and a reload replaces
// the snapshot whole -- so a sentence written there would be taken away by the
// next successful refresh, on a timer, whether or not anybody had read it.
// This one stays until it is dismissed.
func (a App) saying(text string) App {
	a.back = returnTo(a)
	a.res, a.note, a.scr = result{}, text, screenPanel
	return a
}

// dismissed closes whatever is showing. A panel goes back to the screen its
// command was run from; the picker and the help view are only ever opened from
// the page, so they go there.
func (a App) dismissed() App {
	to := screenPage
	if a.scr == screenPanel {
		to = a.back
	}
	a.res, a.note, a.scr = result{}, "", to
	return a
}

// scrolled keeps the cursor on a row that is actually drawn.
//
// The visible count is asked of the renderer's own window rather than
// recomputed here: the height ladder's scrolling rung spends its last line on
// the count of what is off the page, EXCEPT where that would leave no account
// row at all, and a second spelling of that exception would agree with the
// first until the day one of them changed.
func scrolled(m Model) Model {
	n := len(m.Snap.Rows)
	if n == 0 {
		m.Cursor, m.Top = 0, 0
		return m
	}
	if m.Cursor >= n {
		m.Cursor = n - 1
	}
	if m.Cursor < 0 {
		m.Cursor = 0
	}

	// From the top, so this is the window's CAPACITY rather than what it
	// happens to hold at the current offset.
	probe := m
	probe.Top = 0
	shown, _ := probe.window(Plan(m.Set, m.Width, m.Height, n,
		len(m.Snap.Notices) > 0, m.runwayLine() != ""))
	room := len(shown)
	if room < 1 {
		room = 1
	}

	if m.Top > m.Cursor {
		m.Top = m.Cursor
	}
	if m.Cursor >= m.Top+room {
		m.Top = m.Cursor - room + 1
	}
	if m.Top > n-room {
		m.Top = n - room
	}
	if m.Top < 0 {
		m.Top = 0
	}
	return m
}

// fit cuts a block to the terminal it is drawn in and says how many rows it
// took away, rather than ending at the last row in silence -- a screen that
// stops at the bottom is one a reader takes for the whole of it.
func fit(s string, width, height int) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = truncate(lines[i], width)
	}
	if height > 0 && len(lines) > height {
		lines = append(lines[:height-1], truncate(fmt.Sprintf("+%d more rows than fit", len(lines)-height+1), width))
	}
	return strings.Join(lines, "\n")
}
