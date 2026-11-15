//go:build !linux

package ctl

// sunPathLen is the size of sockaddr_un's sun_path on macOS and the BSDs,
// NUL included (unused on Windows, whose control channel is a named pipe).
const sunPathLen = 104
