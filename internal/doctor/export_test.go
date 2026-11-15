package doctor

// SetDevice replaces the device lookup and returns a restore function.
func SetDevice(f func(string) (uint64, bool)) (restore func()) {
	old := deviceOf
	deviceOf = f
	return func() { deviceOf = old }
}

// SetDPAPICheck replaces the DPAPI key's unprotect check and returns a
// restore function.
func SetDPAPICheck(f func(string) error) (restore func()) {
	old := dpapiCheck
	dpapiCheck = f
	return func() { dpapiCheck = old }
}
