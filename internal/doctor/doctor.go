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
// Transient marks an error that may pass without a person (pcb doctor
// exits 20 when every error is one, else 10).
type Result struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Message   string `json:"message"`
	Fix       string `json:"fix,omitempty"`
	Transient bool   `json:"-"`
}

// Check is one health check.
type Check interface {
	Name() string
	Run(ctx context.Context) Result
}

// Multi is a check that finds any number of results, none included (one
// per item it looks at). Run calls RunAll instead of Run.
type Multi interface {
	Check
	RunAll(ctx context.Context) []Result
}

// Run runs the checks in order and returns their results in the same order.
// onResult, when not nil, is called with each result as soon as its check
// returns, before the next check starts.
func Run(ctx context.Context, checks []Check, onResult func(Result)) []Result {
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		rs := []Result(nil)
		if m, ok := c.(Multi); ok {
			rs = m.RunAll(ctx)
		} else {
			rs = append(rs, c.Run(ctx))
		}
		for _, r := range rs {
			if onResult != nil {
				onResult(r)
			}
			results = append(results, r)
		}
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
