//go:build windows && cgo

package main

import (
	"net/url"
	"testing"

	"golang.org/x/sys/windows"
)

// "Open logs" and "Open config" on a profile path with a space, an
// apostrophe (' or ’) and a non-ASCII letter: Windows gets the path itself,
// as typed, through ShellExecute. A file:// URL would not do: Windows
// decodes %C3%AB as two Windows-1252 characters (ZoÃ«) and opens nothing.
func TestOpenPathHandsShellExecuteThePath(t *testing.T) {
	var got []string
	old := shellExecute
	shellExecute = func(_ windows.Handle, verb, file, args, dir *uint16, show int32) error {
		if v := windows.UTF16PtrToString(verb); v != "open" || args != nil || dir != nil || show != windows.SW_SHOWNORMAL {
			t.Errorf("ShellExecute(verb %q, args %v, dir %v, show %d), want open, nil, nil, SW_SHOWNORMAL", v, args, dir, show)
		}
		got = append(got, windows.UTF16PtrToString(file))
		return nil
	}
	t.Cleanup(func() { shellExecute = old })
	open := openPath(func(u *url.URL) error {
		t.Errorf("OpenURL(%s): a file:// URL does not survive Windows' decoding", u)
		return nil
	})
	paths := []string{
		`C:\Users\Zoë O'Brien\AppData\Local\pcb\logs`,
		`C:\Users\Zoë O’Brien\AppData\Roaming\pcb\config.toml`,
		`\\server\share\pcb #1\logs`,
	}
	for _, p := range paths {
		if err := open(p); err != nil {
			t.Errorf("open(%q): %v", p, err)
		}
	}
	if len(got) != len(paths) {
		t.Fatalf("ShellExecute got %q, want %q", got, paths)
	}
	for i := range paths {
		if got[i] != paths[i] {
			t.Errorf("ShellExecute got %q, want %q", got[i], paths[i])
		}
	}
}
