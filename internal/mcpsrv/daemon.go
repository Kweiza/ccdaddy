package mcpsrv

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// addDaemonTools registers the four controls over the background process that
// outlives the session: daemon_start, daemon_stop, daemon_restart and
// daemon_status. All four already have a class in this package's class map;
// this is where their schemas and handlers go, and until they arrive the map is
// what refuses them.
func addDaemonTools(srv *mcp.Server, e view.Exec) error { return nil }
