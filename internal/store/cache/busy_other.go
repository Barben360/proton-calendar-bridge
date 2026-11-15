//go:build !windows

package cache

// fileBusy: on Unix a file is removed whoever holds it open.
func fileBusy(error) bool { return false }
