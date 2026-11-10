// Package doctor runs the health checks behind `pcb doctor`.
package doctor

import "context"

// Status is the outcome of a check.
type Status string

// The statuses, from best to worst (skip is neutral).
const (
	StatusOK    Status = "ok"
	StatusWarn  Status = "warn"
	StatusError Status = "error"
	StatusSkip  Status = "skip"
)

// Result is what one check found. Fix, when set, is a command that repairs it.
type Result struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Check is one health check.
type Check interface {
	Name() string
	Run(ctx context.Context) Result
}

// Run runs the checks in order and returns their results in the same order.
func Run(ctx context.Context, checks []Check) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		results = append(results, c.Run(ctx))
	}
	return results
}

// Worst returns the most severe status: error, then warn, else ok.
func Worst(results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		switch r.Status {
		case StatusError:
			return StatusError
		case StatusWarn:
			worst = StatusWarn
		}
	}
	return worst
}
