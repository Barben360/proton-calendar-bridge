package main

import (
	"net/url"
	"strings"
)

// fileURL is the file:// URL "Open logs" and "Open config" hand to
// fyne.App.OpenURL (xdg-open on Linux, ShellExecute on Windows). A Windows
// path (a drive letter, or a backslash) gets forward slashes and a leading
// slash, so C:\Users\Zoë O'Brien\… is file:///C:/Users/Zo%C3%AB%20O%27Brien/…;
// a UNC path \\server\share\… names its server as the host; a Unix path is
// unchanged. It is OS-neutral so the Linux tests cover the Windows form.
func fileURL(path string) *url.URL {
	if !windowsPath(path) {
		return &url.URL{Scheme: "file", Path: path}
	}
	p := strings.ReplaceAll(path, `\`, "/")
	if rest, ok := strings.CutPrefix(p, "//"); ok {
		host, share, _ := strings.Cut(rest, "/")
		return &url.URL{Scheme: "file", Host: host, Path: "/" + share}
	}
	return &url.URL{Scheme: "file", Path: "/" + p}
}

// windowsPath reports a path that starts with a drive letter (X:) or holds a
// backslash; one that starts with a slash is a Unix path, whatever it holds.
func windowsPath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return false
	}
	if len(p) >= 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z') {
		return true
	}
	return strings.Contains(p, `\`)
}
