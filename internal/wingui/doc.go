// Package wingui is what pcb-ui needs from Windows beyond Fyne: printing
// --version, --help and usage errors in the terminal that started it (a
// windowsgui binary has no console of its own), and the session-end
// messages (logoff, shutdown) that let it drain before Windows ends it.
// Every file but this one is Windows only.
package wingui
