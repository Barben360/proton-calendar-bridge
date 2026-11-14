// Package seed builds the synthetic data set of the Proton test account and
// applies it by driving the proton-cal CLI as an external tool. Our own write
// path comes later (M4); nothing of proton-cal is copied here.
//
// The data set file holds generator parameters and explicit special cases;
// Expand turns them into events deterministically (same file, same events).
package seed

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Entry kinds.
const (
	KindTimed  = "timed"
	KindAllDay = "allday"
	KindSeries = "series"
	KindText   = "text"
)

// Prefix starts every seeded summary: "pcb-seed <id> <title>".
const Prefix = "pcb-seed"

const (
	timedLayout = "2006-01-02 15:04"
	dateLayout  = "2006-01-02"
)

// Weighted is one value of a weighted choice.
type Weighted struct {
	V string `yaml:"v"`
	W int    `yaml:"w"`
}

// Calendar is a calendar the data set uses. The one flagged Default stands for
// the account's default calendar, whatever it is called.
type Calendar struct {
	Key     string `yaml:"key"`
	Name    string `yaml:"name"`
	Color   string `yaml:"color"`
	Default bool   `yaml:"default"`
}

// Occurrence names one occurrence of a series by its original start, in the
// series' time zone ("2006-01-02 15:04", or a date for an all-day series).
type Occurrence struct {
	Occurrence string `yaml:"occurrence"`
	Start      string `yaml:"start"` // edits only: the new start
	End        string `yaml:"end"`   // edits only: the new end
}

// Entry is one event (or series) to create. Series and text cases are written
// out in the file; timed and all-day entries come from the generators.
type Entry struct {
	ID    string `yaml:"-"`
	Kind  string `yaml:"-"`
	Title string `yaml:"title"`

	Calendar         string   `yaml:"calendar"` // a Calendar.Key
	Start            string   `yaml:"start"`
	End              string   `yaml:"end"`
	TZ               string   `yaml:"tz"`
	AllDay           bool     `yaml:"all_day"`
	Description      string   `yaml:"description"`
	DescriptionBytes int      `yaml:"description_bytes"` // generated filler of that length
	Location         string   `yaml:"location"`
	Reminders        []string `yaml:"reminders"` // none, 15m, 1d, email:1h; empty = calendar default
	Color            string   `yaml:"color"`

	Repeat string `yaml:"repeat"` // daily|weekly|monthly|yearly
	Every  int    `yaml:"every"`
	Count  int    `yaml:"count"`
	Until  string `yaml:"until"`
	RRule  string `yaml:"rrule"`

	Edit        *Occurrence `yaml:"edit"`
	Delete      *Occurrence `yaml:"delete"`
	DSTCrossing bool        `yaml:"dst_crossing"` // documentation, checked by the tests
}

// Every1 is the repeat interval, 1 when unset.
func (e Entry) Every1() int {
	if e.Every < 1 {
		return 1
	}
	return e.Every
}

type timedGen struct {
	Count        int                 `yaml:"count"`
	Hours        [2]int              `yaml:"hours"`
	DurationsMin []int               `yaml:"durations_min"`
	Timezones    []Weighted          `yaml:"timezones"`
	Calendars    []Weighted          `yaml:"calendars"`
	Reminders    []Weighted          `yaml:"reminders"`
	Colors       []Weighted          `yaml:"colors"`
	Titles       map[string][]string `yaml:"titles"`
}

type alldayGen struct {
	Count         int        `yaml:"count"`
	MultiDayShare int        `yaml:"multi_day_share"` // percent
	MaxDays       int        `yaml:"max_days"`
	Calendars     []Weighted `yaml:"calendars"`
	Reminders     []Weighted `yaml:"reminders"`
	Colors        []Weighted `yaml:"colors"`
	Titles        []string   `yaml:"titles"`
}

// Dataset is the parsed data set file.
type Dataset struct {
	Seed  uint64 `yaml:"seed"`
	Range struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
	} `yaml:"range"`
	Calendars []Calendar `yaml:"calendars"`
	Timed     timedGen   `yaml:"timed"`
	AllDay    alldayGen  `yaml:"allday"`
	Series    []Entry    `yaml:"series"`
	Text      []Entry    `yaml:"text"`
}

// Load reads and validates a data set file.
func Load(path string) (Dataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, err
	}
	var ds Dataset
	if err := yaml.UnmarshalWithOptions(raw, &ds, yaml.Strict()); err != nil {
		return Dataset{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := ds.validate(string(raw)); err != nil {
		return Dataset{}, fmt.Errorf("%s: %w", path, err)
	}
	return ds, nil
}

func (ds Dataset) validate(raw string) error {
	// The file ends up in proton-cal's argv and in logs: it must never hold a secret.
	for _, bad := range []string{"pass://", "PCB_LAB_", "PROTON_CAL_", "otpauth://"} {
		if strings.Contains(raw, bad) {
			return fmt.Errorf("contains %q: the data set must not hold anything secret-looking", bad)
		}
	}
	from, err := time.Parse(dateLayout, ds.Range.From)
	if err != nil {
		return fmt.Errorf("range.from: %w", err)
	}
	to, err := time.Parse(dateLayout, ds.Range.To)
	if err != nil || to.Before(from) {
		return fmt.Errorf("range.to %q is not a date on or after range.from", ds.Range.To)
	}
	keys := map[string]bool{}
	defaults := 0
	for _, c := range ds.Calendars {
		if c.Key == "" || c.Name == "" || keys[c.Key] {
			return fmt.Errorf("calendar %+v: key and name are required, key unique", c)
		}
		keys[c.Key] = true
		if c.Default {
			defaults++
		}
	}
	if defaults != 1 {
		return fmt.Errorf("%d calendars flagged default, want exactly 1", defaults)
	}
	check := func(what string, ws []Weighted, ok func(string) bool) error {
		for _, w := range ws {
			if w.W < 1 || !ok(w.V) {
				return fmt.Errorf("%s: bad weighted value %+v", what, w)
			}
		}
		return nil
	}
	isKey := func(v string) bool { return keys[v] }
	isTZ := func(v string) bool { _, err := time.LoadLocation(v); return err == nil }
	any := func(string) bool { return true }
	if ds.Timed.Count > 0 {
		t := ds.Timed
		if len(t.DurationsMin) == 0 || t.Hours[0] >= t.Hours[1] {
			return fmt.Errorf("timed: durations_min and hours are required")
		}
		for _, c := range []struct {
			n  string
			ws []Weighted
			ok func(string) bool
		}{{"timed.timezones", t.Timezones, isTZ}, {"timed.calendars", t.Calendars, isKey}, {"timed.reminders", t.Reminders, any}, {"timed.colors", t.Colors, any}} {
			if len(c.ws) == 0 {
				return fmt.Errorf("%s is required", c.n)
			}
			if err := check(c.n, c.ws, c.ok); err != nil {
				return err
			}
		}
		for _, c := range t.Calendars {
			if len(t.Titles[c.V]) == 0 {
				return fmt.Errorf("timed.titles has none for calendar %q", c.V)
			}
		}
	}
	if a := ds.AllDay; a.Count > 0 {
		if len(a.Titles) == 0 || a.MaxDays < 2 || len(a.Calendars) == 0 || len(a.Reminders) == 0 || len(a.Colors) == 0 {
			return fmt.Errorf("allday: titles, calendars, reminders, colors and max_days >= 2 are required")
		}
		for _, c := range []struct {
			n  string
			ws []Weighted
			ok func(string) bool
		}{{"allday.calendars", a.Calendars, isKey}, {"allday.reminders", a.Reminders, any}, {"allday.colors", a.Colors, any}} {
			if err := check(c.n, c.ws, c.ok); err != nil {
				return err
			}
		}
	}
	for _, e := range append(append([]Entry{}, ds.Series...), ds.Text...) {
		if !keys[e.Calendar] || e.Title == "" || e.Start == "" {
			return fmt.Errorf("entry %q: title, start and a known calendar are required", e.Title)
		}
		if _, err := time.LoadLocation(e.tz()); err != nil {
			return fmt.Errorf("entry %q: %w", e.Title, err)
		}
		layout := timedLayout
		if e.AllDay {
			layout = dateLayout
		}
		for _, v := range []string{e.Start, e.End} {
			if _, err := time.Parse(layout, v); v != "" && err != nil {
				return fmt.Errorf("entry %q: %w", e.Title, err)
			}
		}
		if (e.Edit == nil) != (e.Delete == nil) {
			return fmt.Errorf("entry %q: edit and delete go together", e.Title)
		}
		for _, o := range []*Occurrence{e.Edit, e.Delete} {
			if o != nil {
				if _, err := time.Parse(layout, o.Occurrence); err != nil {
					return fmt.Errorf("entry %q: occurrence: %w", e.Title, err)
				}
			}
		}
		if e.Edit != nil && e.Edit.Start == "" {
			return fmt.Errorf("entry %q: edit needs a new start", e.Title)
		}
	}
	return nil
}

func (e Entry) tz() string {
	if e.TZ == "" {
		return "UTC"
	}
	return e.TZ
}

func pick(r *rand.Rand, ws []Weighted) string {
	total := 0
	for _, w := range ws {
		total += w.W
	}
	n := r.IntN(total)
	for _, w := range ws {
		if n < w.W {
			return w.V
		}
		n -= w.W
	}
	return ws[len(ws)-1].V
}

// filler is a deterministic text of exactly n bytes.
func filler(n int) string {
	const unit = "Synthetic description for round-trip tests. "
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

func reminders(v string) []string {
	if v == "" || v == "default" {
		return nil
	}
	return []string{v}
}

// Expand returns every event of the data set in a stable order and with stable
// ids (s001, s002, ...): timed, all-day, series, then text cases. The same
// Dataset always expands to the same entries.
func (ds Dataset) Expand() []Entry {
	r := rand.New(rand.NewPCG(ds.Seed, ds.Seed))
	from, _ := time.Parse(dateLayout, ds.Range.From)
	to, _ := time.Parse(dateLayout, ds.Range.To)
	days := int(to.Sub(from).Hours()/24) + 1
	var out []Entry
	add := func(kind string, e Entry) {
		e.Kind = kind
		e.ID = fmt.Sprintf("s%03d", len(out)+1)
		out = append(out, e)
	}

	t := ds.Timed
	for i := 0; i < t.Count; i++ {
		day := from.AddDate(0, 0, r.IntN(days))
		hour := t.Hours[0] + r.IntN(t.Hours[1]-t.Hours[0])
		start := time.Date(day.Year(), day.Month(), day.Day(), hour, 15*r.IntN(4), 0, 0, time.UTC)
		end := start.Add(time.Duration(t.DurationsMin[r.IntN(len(t.DurationsMin))]) * time.Minute)
		cal := pick(r, t.Calendars)
		titles := t.Titles[cal]
		add(KindTimed, Entry{
			Title:     titles[r.IntN(len(titles))],
			Calendar:  cal,
			Start:     start.Format(timedLayout),
			End:       end.Format(timedLayout),
			TZ:        pick(r, t.Timezones),
			Reminders: reminders(pick(r, t.Reminders)),
			Color:     pick(r, t.Colors),
		})
	}

	a := ds.AllDay
	for i := 0; i < a.Count; i++ {
		day := from.AddDate(0, 0, r.IntN(days))
		e := Entry{AllDay: true, Start: day.Format(dateLayout)}
		if r.IntN(100) < a.MultiDayShare {
			e.End = day.AddDate(0, 0, 1+r.IntN(a.MaxDays-1)).Format(dateLayout)
		}
		e.Calendar = pick(r, a.Calendars)
		e.Title = a.Titles[r.IntN(len(a.Titles))]
		e.Reminders = reminders(pick(r, a.Reminders))
		e.Color = pick(r, a.Colors)
		add(KindAllDay, e)
	}
	for _, e := range ds.Series {
		add(KindSeries, e)
	}
	for _, e := range ds.Text {
		if e.DescriptionBytes > 0 && e.Description == "" {
			e.Description = filler(e.DescriptionBytes)
		}
		add(KindText, e)
	}
	return out
}
