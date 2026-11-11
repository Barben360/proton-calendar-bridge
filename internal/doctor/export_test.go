package doctor

// SetDevice replaces the device lookup and returns a restore function.
func SetDevice(f func(string) (uint64, bool)) (restore func()) {
	old := deviceOf
	deviceOf = f
	return func() { deviceOf = old }
}
