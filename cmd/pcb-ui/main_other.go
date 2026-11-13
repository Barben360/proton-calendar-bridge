//go:build !linux || !cgo

// Command pcb-ui is the desktop app of pcb. Only the Linux build with cgo
// exists for now.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pcb-ui is not supported on this system yet")
	os.Exit(2)
}
