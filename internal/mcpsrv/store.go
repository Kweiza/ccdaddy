package mcpsrv

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// addStoreTools registers the five verbs that write ccdad's own account file:
// enable, disable, alias, move and primary. All five already have a class in
// this package's class map; this is where their schemas and handlers go, and
// until they arrive the map is what refuses them.
func addStoreTools(srv *mcp.Server, e view.Exec) error { return nil }
