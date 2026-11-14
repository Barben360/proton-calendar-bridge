//go:build (!linux && !windows) || !cgo

// Command pcb-ui is the desktop app of pcb. Only the Linux and Windows
// builds with cgo exist for now.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pcb-ui is not supported on this system yet")
	os.Exit(2)
}
