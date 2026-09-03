package cli

import (
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// codexRoot is the store root the codex pointer lives under.
func codexRoot() (string, error) { return ccpath.StoreHome() }

// codexServingAccount is the account codex is served from, resolved against the
// store.
//
// It is the ONE reader of the pointer on the command side, and `list`,
// `status`, `which`, the dashboard and `doctor` all go through it. Three
// spellings of "read the file and look the uuid up" would agree until the day
// one of them changed -- which is the same argument view.Row makes about the
// cells, applied to a second document.
//
// A pointer naming an account the store no longer has answers NO ACCOUNT rather
// than a placeholder, and that is deliberate: it is exactly what the proxy does
// with such a pointer, so the surface and the request path give one answer.
func codexServingAccount(accounts []store.Account) (store.Account, bool) {
	root, err := codexRoot()
	if err != nil {
		return store.Account{}, false
	}
	uuid, ok := codexswitch.ReadServing(root)
	if !ok {
		return store.Account{}, false
	}
	for _, a := range accounts {
		if a.UUID == uuid && a.Provider == provider.Codex {
			return a, true
		}
	}
	return store.Account{}, false
}

// daemonIsRunning is whether a daemon holds the singleton right now.
//
// "Cannot determine" answers FALSE, and that is the safe direction here: the
// only thing this decides is whether a repoint's sentence adds "once the daemon
// runs", and telling a user about a daemon that is in fact running costs them a
// glance, while staying silent about one that is not costs them a codex session
// billed to the account they thought they had moved off.
func daemonIsRunning() bool {
	held, err := singletonHeld()
	return err == nil && held
}

// codexPointerPath names the pointer file for a MESSAGE. It takes the two-value
// resolution apart the way namePath's callers do, because Go makes a two-value
// call unusable inside a format string.
func codexPointerPath() (string, error) {
	root, err := codexRoot()
	if err != nil {
		return "", err
	}
	return codexswitch.ServingPath(root), nil
}
