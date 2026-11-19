package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The constraints of the input schemas: tools/list gives an agent the
// allowed values and bounds, and the SDK refuses a call outside them
// before a handler runs (refusedInvalid then gives that refusal the tool
// error's envelope). The schemas are inferred from the argument types; a
// tuner adds what a tag cannot say.

// tuner adds constraints to a tool's inferred input schema.
type tuner func(root *jsonschema.Schema)

// at is the property of s at path, nil when there is none.
func at(s *jsonschema.Schema, path ...string) *jsonschema.Schema {
	for _, p := range path {
		if s == nil {
			return nil
		}
		s = s.Properties[p]
	}
	return s
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

// datePattern is a calendar date as start_date and end_date take it.
const datePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`

// Alarm bounds, in minutes before the start: Proton keeps at most ten
// notifications, none after a timed event's start nor a day or more after
// an all-day one's, each within 999 weeks (internal/ical's alarms).
const (
	maxAlarms   = 10
	minAlarmMin = -1439
	maxAlarmMin = 999 * 7 * 24 * 60
)

// scopeEnum limits scope, at the root or under path, to the two scopes.
func scopeEnum(root *jsonschema.Schema) {
	if p := at(root, "scope"); p != nil {
		p.Enum = []any{string(service.ScopeOccurrence), string(service.ScopeSeries)}
	}
}

// limitBounds limits limit to 1 to service.MaxAgendaLimit.
func limitBounds(root *jsonschema.Schema) {
	if p := at(root, "limit"); p != nil {
		p.Minimum, p.Maximum = fp(1), fp(service.MaxAgendaLimit)
	}
}

// dateFields give start_date and end_date their pattern, under path.
func dateFields(path ...string) tuner {
	return func(root *jsonschema.Schema) {
		for _, name := range []string{"start_date", "end_date"} {
			if p := at(root, append(path[:len(path):len(path)], name)...); p != nil {
				p.Pattern = datePattern
			}
		}
	}
}

// alarmBounds bounds the alarms under path.
func alarmBounds(path ...string) tuner {
	return func(root *jsonschema.Schema) {
		p := at(root, append(path[:len(path):len(path)], "alarms")...)
		if p == nil || p.Items == nil {
			return
		}
		p.MaxItems = ip(maxAlarms)
		p.Items.Minimum, p.Items.Maximum = fp(minAlarmMin), fp(maxAlarmMin)
	}
}

// tuned is the input schema of In with the tuners applied.
func tuned[In any](tuners []tuner) (*jsonschema.Schema, error) {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, err
	}
	for _, t := range tuners {
		t(s)
	}
	return s, nil
}
