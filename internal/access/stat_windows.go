//go:build windows

package access

import (
	"os"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
)

// statFile stats path through an open handle. os.Stat on Windows defers the
// file's identity (volume serial and file index) until os.SameFile asks,
// then reads it by path: a FileInfo kept from before a replacement would
// take the new file's identity and never differ from a fresh one. A handle's
// Stat reads it at once (GetFileInformationByHandle). The handle shares
// read, write and delete, so it never holds up a writer.
func statFile(path string) (os.FileInfo, error) {
	f, err := atomicfile.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	return f.Stat()
}
