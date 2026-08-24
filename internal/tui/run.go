package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"
)

// program is the construction, behind a package var.
//
// A real terminal, and now a real event loop, is not something a test can
// arrange -- and the decision that hangs on this one is not what it renders
// but WHAT IT ASKS FOR, which is a thing a stub can see and a pty cannot.
var program = func(m tea.Model, opts ...tea.ProgramOption) error {
	_, err := tea.NewProgram(m, opts...).Run()
	return err
}

// Run drives the dashboard until the user quits.
//
// THE OPTION LIST IS EMPTY, AND THAT IS THE POINT. The two that are missing
// are WithoutSignalHandler and WithoutSignals, and leaving them out is a
// shutdown property rather than a default nobody thought about. With the
// library's own handler in place, a SIGTERM arriving in raw mode is turned
// into an ordinary quit: the loop returns, the terminal is put back to echo,
// canonical input and signal generation, and this returns nil. With the
// handler removed, the same SIGTERM kills the process outright with status 15
// and leaves the terminal in raw mode -- the shell the user goes back to has
// no echo, no line editing and no Ctrl-C.
//
// Ctrl-C is an ordinary key while nothing is released: the terminal is in raw
// mode, so it arrives as a keystroke rather than a signal, there is no scoped
// trap to fight, and signals are not being ignored. During the add key's
// released span both signals are dropped, which is documented where that key
// is written.
//
// A panic inside the model is caught by the program's own recover, the
// terminal is restored, and this returns the killed-program error. The
// dashboard still dies, which is correct: a renderer that panicked has nothing
// useful left to show.
//
// AN INTERRUPT IS NOT A FAILURE, and this is where that is decided. The
// library routes SIGTERM to an ordinary quit and SIGINT to an interrupt, and
// the two take the same shutdown path and leave the terminal in the same
// state -- the only difference is that one comes back as an error. Passing it
// on would make `kill -INT` print a message and exit non-zero where `kill
// -TERM` is silent and exits 0, for two ways of asking the same program to
// stop. It is worded the way the login's own interrupted exit is worded: the
// user's decision, not a fault.
//
// Options.Out is not passed through, and the reason is not that the library
// cannot take a writer -- it can. It is that Out is where the ONE-SHOT render
// writes, downstream of a colour decision package cli has already made, and a
// program makes that decision for itself by probing the terminal it was given.
// Handing it a writer that has already answered would be two answers to one
// question.
func Run(o Options) error {
	err := program(newApp(o))
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}
