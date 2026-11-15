// Package acltest sets and checks who may use a file in tests, so the
// packages whose checks refuse a file open to others share one way to make
// one and one way to check what pcb made: modes on Unix, access lists on
// Windows (with Allow, Deny and SetOwner for what only Windows has).
package acltest

// Everyone is the SID of Everyone (S-1-1-0).
const Everyone = "S-1-1-0"
