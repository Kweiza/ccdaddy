// Command ccdad manages multiple Claude Code accounts.
package main

import (
	"os"

	"github.com/Kweiza/ccdaddy/internal/cli"
)

func main() {
	os.Exit(int(cli.Execute()))
}
