package config

// CheckBackend exposes checkBackend.
func CheckBackend(goos, backend string) error { return checkBackend(goos, backend) }
