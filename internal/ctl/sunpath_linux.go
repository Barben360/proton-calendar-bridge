//go:build linux

package ctl

// sunPathLen is the size of sockaddr_un's sun_path on Linux, NUL included.
const sunPathLen = 108
