package mcpsrv

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// Register adds every ccdad tool.
//
// The four groups are the four security classes, one file each, and this list
// is deliberately fixed: a group is FILLED in its own file rather than appended
// here, so that adding a tool cannot change the composition and three authors
// can work on three groups without editing one file between them.
//
// It is exported for the totality test, which needs a fully composed server
// with no transport under it.
func Register(srv *mcp.Server, e view.Exec) error {
	for _, add := range []func(*mcp.Server, view.Exec) error{
		addReadTools,
		addStoreTools,
		addSwitchTool,
		addDaemonTools,
	} {
		if err := add(srv, e); err != nil {
			return err
		}
	}
	return nil
}
