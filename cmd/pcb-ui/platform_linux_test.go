//go:build linux && cgo

package main

import (
	"net/url"
	"testing"
)

// On Linux "Open logs" and "Open config" go to xdg-open as file:// URLs,
// escaped: a space or a # in the path survives.
func TestOpenPathIsAnEscapedFileURL(t *testing.T) {
	var got []string
	open := openPath(func(u *url.URL) error { got = append(got, u.String()); return nil })
	if err := open("/home/my user/.local/state/pcb #1/logs"); err != nil {
		t.Fatal(err)
	}
	if want := "file:///home/my%20user/.local/state/pcb%20%231/logs"; len(got) != 1 || got[0] != want {
		t.Errorf("OpenURL got %q, want %q", got, want)
	}
	back, err := url.Parse(got[0])
	if err != nil || back.Path != "/home/my user/.local/state/pcb #1/logs" {
		t.Errorf("round trip: %v, %v", back, err)
	}
}
