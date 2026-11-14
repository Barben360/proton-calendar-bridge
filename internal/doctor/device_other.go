//go:build !unix

package doctor

// device is unknown off Unix, so volumes are never reported as shared.
func device(string) (uint64, bool) { return 0, false }
