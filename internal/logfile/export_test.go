package logfile

// CloseHandle closes f's open handle behind its back, so the next rotation's
// Close fails (os.ErrClosed), as a failing file system would.
func CloseHandle(f *File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.f.Close()
}
