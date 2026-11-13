package ui

// PasswordBuf is the wizard's password buffer itself (not a copy), so a test
// can look at it after the wizard zeroed it.
func (w *Wizard) PasswordBuf() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pw
}
