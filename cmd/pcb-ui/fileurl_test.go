package main

import (
	"net/url"
	"testing"
)

func TestFileURLEscapesSpacesAndSpecials(t *testing.T) {
	u := fileURL("/home/my user/.local/state/pcb #1/logs")
	if got, want := u.String(), "file:///home/my%20user/.local/state/pcb%20%231/logs"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	back, err := url.Parse(u.String())
	if err != nil || back.Path != "/home/my user/.local/state/pcb #1/logs" {
		t.Errorf("round trip: %q, %v", back.Path, err)
	}
}

// "Open logs" on a profile path with a space, an apostrophe and a non-ASCII
// letter: a drive-letter path becomes file:///C:/… with forward slashes and
// a UNC path file://server/share/…, which ShellExecute opens; a Unix path is
// unchanged.
func TestFileURLOnWindowsPaths(t *testing.T) {
	for _, tc := range []struct{ in, host, path, str string }{
		{`C:\Users\Zoë O'Brien\AppData\Local\pcb\logs`, "", "/C:/Users/Zoë O'Brien/AppData/Local/pcb/logs",
			"file:///C:/Users/Zo%C3%AB%20O%27Brien/AppData/Local/pcb/logs"},
		{`D:\pcb’s state\config.toml`, "", "/D:/pcb’s state/config.toml", ""},
		{`C:/Users/u/logs`, "", "/C:/Users/u/logs", "file:///C:/Users/u/logs"},
		{`\\server\share\pcb`, "server", "/share/pcb", "file://server/share/pcb"},
		{"/home/u/.local/state/pcb/logs", "", "/home/u/.local/state/pcb/logs", "file:///home/u/.local/state/pcb/logs"},
		{`/home/u/a\b:c`, "", `/home/u/a\b:c`, "file:///home/u/a%5Cb:c"},
	} {
		u := fileURL(tc.in)
		if u.Scheme != "file" || u.Host != tc.host || u.Path != tc.path {
			t.Errorf("fileURL(%q) = scheme %q host %q path %q, want file, %q and %q", tc.in, u.Scheme, u.Host, u.Path, tc.host, tc.path)
		}
		if tc.str != "" && u.String() != tc.str {
			t.Errorf("fileURL(%q) = %s, want %s", tc.in, u.String(), tc.str)
		}
		back, err := url.Parse(u.String())
		if err != nil {
			t.Errorf("fileURL(%q) = %s does not parse: %v", tc.in, u, err)
		} else if back.Host != tc.host || back.Path != tc.path {
			t.Errorf("fileURL(%q) round trip: host %q path %q", tc.in, back.Host, back.Path)
		}
	}
}
