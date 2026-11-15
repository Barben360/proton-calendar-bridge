package cache

import (
	"errors"

	"golang.org/x/sys/windows"
)

// fileBusy reports a sharing violation or an access denied: another handle
// holds the file.
func fileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
