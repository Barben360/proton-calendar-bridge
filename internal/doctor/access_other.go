//go:build !windows

package doctor

// aclProblem is Windows'; elsewhere checked is false and callers judge the
// Unix mode.
func aclProblem(string, bool) (message, fix string, checked bool) { return "", "", false }
