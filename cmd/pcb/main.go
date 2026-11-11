// Command pcb is the Proton Calendar to CalDAV bridge.
package main

import (
	"os"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
)

func main() {
	os.Exit(int(cli.Execute(os.Args[1:], os.Stdout, os.Stderr)))
}
