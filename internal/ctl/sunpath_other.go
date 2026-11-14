//go:build !linux

package ctl

// sunPathLen is the size of sockaddr_un's sun_path on macOS and the BSDs,
// NUL included (Windows has no control socket: unused there).
const sunPathLen = 104
