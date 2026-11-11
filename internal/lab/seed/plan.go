package seed

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Operations of a Command.
const (
	OpCreate = "create"
	OpEdit   = "edit"
	OpDelete = "delete"
)

// Placeholders that Apply substitutes in Command.Args.
const (
	// PlaceholderID is the proton-cal event id of the series the command Ref names.
	PlaceholderID = "{id}"
	// PlaceholderCal is the proton-cal calendar id of the data set calendar key
	// the command Calendar names.
	PlaceholderCal = "{cal}"
)

// Command is one proton-cal invocation (argv without the binary name). Args
// never carry a secret: credentials stay in proton-cal's own login.
type Command struct {
	ID       string // dataset id; "<id>.e" for an edit, "<id>.d" for a delete
	Op       string
	Ref      string // dataset id whose created event ID the args need ({id}); empty for creates
	Calendar string // dataset calendar key behind {cal}
	Args     []string
}

// Existing is an event already on the account that carries a pcb-seed summary.
type Existing struct {
	ID      string    // dataset id parsed from the summary ("s042", "s341.e")
	EventID string    // proton-cal event id
	Start   time.Time // start of the listed occurrence
}

type listedEvent struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Start   string `json:"start"`
}

// ParseExisting reads `proton-cal events -o json` and keeps the rows whose
// summary starts with "pcb-seed <id> ". Rows of other events are ignored.
func ParseExisting(r io.Reader) ([]Existing, error) {
	var rows []listedEvent
	if err := json.NewDecoder(r).Decode(&rows); err != nil {
		return nil, fmt.Errorf("events listing: %w", err)
	}
	var out []Existing
	for _, row := range rows {
		rest, ok := strings.CutPrefix(row.Summary, Prefix+" ")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, " ")
		if id == "" {
			continue
		}
		start, _ := time.Parse(time.RFC3339, row.Start) // a zero start never matches an occurrence
		out = append(out, Existing{ID: id, EventID: row.ID, Start: start})
	}
	return out, nil
}

func summary(id, title string) string { return Prefix + " " + id + " " + title }

// Plan lists the proton-cal commands still needed, in the order they must run.
// What already exists (matched by its pcb-seed id) is skipped, so a rerun after
// a crash or a rate limit only does the rest.
func Plan(ds Dataset, existing []Existing) []Command {
	have := map[string]bool{}
	for _, e := range existing {
		have[e.ID] = true
	}
	var cmds []Command
	for _, e := range ds.Expand() {
		if !have[e.ID] {
			cmds = append(cmds, Command{ID: e.ID, Op: OpCreate, Calendar: e.Calendar, Args: createArgs(e)})
		}
		if e.Kind != KindSeries || e.Edit == nil {
			continue
		}
		if !have[e.ID+".e"] {
			cmds = append(cmds, Command{ID: e.ID + ".e", Op: OpEdit, Ref: e.ID, Calendar: e.Calendar, Args: editArgs(e)})
		}
		if !have[e.ID] || occurrenceListed(e, existing) { // a listed series lacking the occurrence had it deleted already
			cmds = append(cmds, Command{ID: e.ID + ".d", Op: OpDelete, Ref: e.ID, Calendar: e.Calendar, Args: deleteArgs(e)})
		}
	}
	return cmds
}

// occurrenceListed reports whether the series still lists its to-delete occurrence.
func occurrenceListed(e Entry, existing []Existing) bool {
	at, ok := e.instant(e.Delete.Occurrence)
	if !ok {
		return true
	}
	for _, x := range existing {
		if x.ID == e.ID && x.Start.Equal(at) {
			return true
		}
	}
	return false
}

func (e Entry) instant(s string) (time.Time, bool) {
	var t time.Time
	var err error
	if e.AllDay {
		t, err = time.Parse(dateLayout, s)
	} else {
		loc, lerr := time.LoadLocation(e.tz())
		if lerr != nil {
			return t, false
		}
		t, err = time.ParseInLocation(timedLayout, s, loc)
	}
	return t, err == nil
}

func createArgs(e Entry) []string {
	a := []string{"create", "event", summary(e.ID, e.Title)}
	if e.AllDay {
		a = append(a, "--all-day")
	}
	a = append(a, "--start", e.Start)
	if e.End != "" {
		a = append(a, "--end", e.End)
	}
	if !e.AllDay {
		a = append(a, "--tz", e.tz())
	}
	if e.Description != "" {
		a = append(a, "--description", e.Description)
	}
	if e.Location != "" {
		a = append(a, "--location", e.Location)
	}
	for _, r := range e.Reminders {
		if r == "none" {
			a = append(a, "--no-reminders")
		} else {
			a = append(a, "--reminder", r)
		}
	}
	if e.Color != "" {
		a = append(a, "--color", e.Color)
	}
	if e.RRule != "" {
		a = append(a, "--rrule", e.RRule)
	} else if e.Repeat != "" {
		a = append(a, "--repeat", e.Repeat)
		if e.Every > 1 {
			a = append(a, "--every", fmt.Sprint(e.Every))
		}
		if e.Count > 0 {
			a = append(a, "--count", fmt.Sprint(e.Count))
		}
		if e.Until != "" {
			a = append(a, "--until", e.Until)
		}
	}
	return tail(a)
}

func editArgs(e Entry) []string {
	a := []string{"update", "event", PlaceholderID, "--occurrence", e.Edit.Occurrence, "--start", e.Edit.Start}
	if e.Edit.End != "" {
		a = append(a, "--end", e.Edit.End)
	}
	a = append(a, "--summary", summary(e.ID+".e", e.Title+" (edited)"))
	if !e.AllDay {
		a = append(a, "--tz", e.tz())
	}
	return tail(a)
}

func deleteArgs(e Entry) []string {
	a := []string{"delete", "event", PlaceholderID, "--occurrence", e.Delete.Occurrence}
	if !e.AllDay {
		a = append(a, "--tz", e.tz())
	}
	return tail(a)
}

func tail(a []string) []string {
	return append(a, "--calendar", PlaceholderCal, "-o", "json")
}
