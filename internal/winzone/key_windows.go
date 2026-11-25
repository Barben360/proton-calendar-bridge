//go:build windows

package winzone

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetDynamicTimeZoneInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDynamicTimeZoneInformation")

// dynamicTimeZoneInformation is DYNAMIC_TIME_ZONE_INFORMATION (x/sys has no
// wrapper for the call).
type dynamicTimeZoneInformation struct {
	windows.Timezoneinformation
	TimeZoneKeyName             [128]uint16
	DynamicDaylightTimeDisabled uint8
}

// Key is the Windows zone key of the process: the TimeZoneKeyName of
// GetDynamicTimeZoneInformation, such as "W. Europe Standard Time". Not ok
// when the call fails or names none.
func Key() (string, bool) {
	var info dynamicTimeZoneInformation
	r, _, _ := procGetDynamicTimeZoneInformation.Call(uintptr(unsafe.Pointer(&info)))
	if uint32(r) == 0xFFFFFFFF { //nolint:gosec // G115: GetDynamicTimeZoneInformation returns a DWORD in a register (TIME_ZONE_ID_INVALID)
		return "", false
	}
	key := syscall.UTF16ToString(info.TimeZoneKeyName[:])
	return key, key != ""
}
