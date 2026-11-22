package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// Mode is the user's answer: ask after each report (the default), always
// send, never keep one.
type Mode string

const (
	Ask    Mode = "ask"
	Always Mode = "always"
	Never  Mode = "never"
)

// EnvSwitch overrides the mode for one process when set to always or never
// (any case); any other value is ignored.
const EnvSwitch = "PCB_CRASH_REPORTS"

// MaxPending is how many reports wait at most; past it the oldest go.
const MaxPending = 20

// Dir is where the reports wait: <state>/reports.
func Dir(stateDir string) string { return filepath.Join(stateDir, "reports") }

// StorePath is reports.json in the state directory: the mode and the
// sending bookkeeping.
func StorePath(stateDir string) string { return filepath.Join(stateDir, "reports.json") }

func lockPath(stateDir string) string { return filepath.Join(stateDir, "reports.lock") }

// State is reports.json. Seen maps a fingerprint's hash to the UTC day
// (2006-01-02) its report was last saved; the other fields are the
// sender's: RetryAfter (no send before), SentDay and SentCount (the daily
// cap), Attempts (failed sends by event id) and NoticedDay (the CLI's
// once-a-day line).
type State struct {
	Mode       Mode              `json:"mode,omitempty"`
	Seen       map[string]string `json:"seen,omitempty"`
	RetryAfter time.Time         `json:"retry_after,omitzero"`
	SentDay    string            `json:"sent_day,omitempty"`
	SentCount  int               `json:"sent_count,omitempty"`
	Attempts   map[string]int    `json:"attempts,omitempty"`
	NoticedDay string            `json:"noticed_day,omitempty"`
}

// Store is the reports of one state directory. A nil Env reads the
// process's environment, a nil Now the clock.
type Store struct {
	StateDir string
	Env      func(string) string
	Now      func() time.Time
}

// Pending is one report waiting for an answer: JSON is the exact event, as
// it is shown and sent.
type Pending struct {
	ID, Path, Level, Source string
	JSON                    []byte
}

func (s Store) env(key string) string {
	if s.Env == nil {
		return os.Getenv(key)
	}
	return s.Env(key)
}

func (s Store) today() string {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return now().UTC().Format(time.DateOnly)
}

func valid(m Mode) bool { return m == Ask || m == Always || m == Never }

// Mode is the mode in force: PCB_CRASH_REPORTS when it says always or
// never, else reports.json's, else Ask (a missing or damaged file too).
func (s Store) Mode() Mode { return s.mode(s.load().Mode) }

func (s Store) mode(file Mode) Mode {
	if m := Mode(strings.ToLower(strings.TrimSpace(s.env(EnvSwitch)))); m == Always || m == Never {
		return m
	}
	if valid(file) {
		return file
	}
	return Ask
}

// SetMode keeps m in reports.json; Never also deletes the waiting reports.
func (s Store) SetMode(ctx context.Context, m Mode) error {
	if !valid(m) {
		return fmt.Errorf("crash reports: unknown mode %q", m)
	}
	return s.locked(ctx, func(st *State) error {
		st.Mode = m
		if m == Never {
			return s.discard(st, nil)
		}
		return nil
	})
}

// Edit applies f to reports.json under <state>/reports.lock, so that two
// processes never erase each other's change (as internal/update.Edit).
func (s Store) Edit(ctx context.Context, f func(*State)) error {
	return s.locked(ctx, func(st *State) error { f(st); return nil })
}

// Save keeps e until the user answers, unless the mode is Never. A report
// with the fingerprint of one already saved today is folded into it
// (extra.count + 1), or dropped when that one is gone (sent, discarded).
// Past MaxPending the oldest reports are deleted.
func (s Store) Save(ctx context.Context, e Event) error {
	return s.locked(ctx, func(st *State) error {
		if s.mode(st.Mode) == Never {
			return nil
		}
		day := s.today()
		for h, d := range st.Seen {
			if d != day {
				delete(st.Seen, h)
			}
		}
		fp := e.Fingerprint()
		hash := fingerprintHash(fp)
		if st.Seen[hash] == day {
			return s.fold(fp)
		}
		b, err := marshal(e)
		if err != nil {
			return err
		}
		if err := acl.MkdirAll(Dir(s.StateDir)); err != nil {
			return err
		}
		if err := atomicfile.Write(s.pendingPath(e.ID()), b); err != nil {
			return err
		}
		if st.Seen == nil {
			st.Seen = map[string]string{}
		}
		st.Seen[hash] = day
		pend, err := s.pending()
		if err != nil {
			return err
		}
		for len(pend) > MaxPending {
			if err := s.remove(st, pend[0].ID); err != nil {
				return err
			}
			pend = pend[1:]
		}
		return nil
	})
}

// fold adds one to the count of the waiting report with fingerprint fp;
// none left (sent or discarded today) means the repeat is dropped.
func (s Store) fold(fp []string) error {
	pend, err := s.pending()
	if err != nil {
		return err
	}
	for _, p := range pend {
		if slices.Equal(p.event.Grouping, fp) {
			p.event.Extra.Count++
			b, err := marshal(p.event)
			if err != nil {
				return err
			}
			return atomicfile.Write(p.Path, b)
		}
	}
	return nil
}

// Pending lists the waiting reports, oldest first. A file that is not a
// report pcb wrote (not JSON, a field outside the allow list, another
// event's id) is deleted, never shown nor sent.
func (s Store) Pending() ([]Pending, error) {
	pend, err := s.pending()
	if err != nil {
		return nil, err
	}
	out := make([]Pending, len(pend))
	for i, p := range pend {
		out[i] = p.Pending
	}
	return out, nil
}

// Discard deletes the waiting reports ids, or all of them when none is
// given.
func (s Store) Discard(ctx context.Context, ids ...string) error {
	return s.locked(ctx, func(st *State) error { return s.discard(st, ids) })
}

func (s Store) discard(st *State, ids []string) error {
	if len(ids) == 0 {
		pend, err := s.pending()
		if err != nil {
			return err
		}
		for _, p := range pend {
			ids = append(ids, p.ID)
		}
	}
	for _, id := range ids {
		if err := s.remove(st, id); err != nil {
			return err
		}
	}
	return nil
}

// remove deletes the report id and its failed attempts; an id that is not
// one (32 hex digits) names no file.
func (s Store) remove(st *State, id string) error {
	if !eventID.MatchString(id) {
		return nil
	}
	delete(st.Attempts, id)
	if err := os.Remove(s.pendingPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

var (
	eventID     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	pendingName = regexp.MustCompile(`^pending-(.*)\.json$`)
)

func (s Store) pendingPath(id string) string {
	return filepath.Join(Dir(s.StateDir), "pending-"+id+".json")
}

type pendingReport struct {
	Pending
	event Event
	at    time.Time
}

// pending reads every pending-*.json of the reports directory (no state
// directory or a missing one holds none), deleting those that are not a
// report pcb wrote.
func (s Store) pending() ([]pendingReport, error) {
	if s.StateDir == "" {
		return nil, nil
	}
	dir := Dir(s.StateDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []pendingReport
	for _, ent := range entries {
		m := pendingName.FindStringSubmatch(ent.Name())
		if m == nil || !ent.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		b, err := atomicfile.ReadFile(path)
		if err != nil {
			continue // unreadable now (removed meanwhile): not a report
		}
		e, ok := decodeEvent(b)
		at, terr := time.Parse(time.RFC3339, e.Timestamp)
		if !ok || terr != nil || !eventID.MatchString(m[1]) || e.EventID != m[1] {
			_ = os.Remove(path)
			continue
		}
		out = append(out, pendingReport{
			Pending: Pending{ID: e.EventID, Path: path, Level: e.Level, Source: e.Tags.Source, JSON: b},
			event:   e,
			at:      at,
		})
	}
	slices.SortStableFunc(out, func(a, b pendingReport) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// decodeEvent reads b as an Event and nothing else: one JSON object whose
// every field is one of the allow list.
func decodeEvent(b []byte) (Event, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Event
	if dec.Decode(&e) != nil {
		return Event{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Event{}, false
	}
	return e, true
}

func fingerprintHash(fp []string) string {
	sum := sha256.Sum256([]byte(strings.Join(fp, "\n")))
	return hex.EncodeToString(sum[:])
}

// locked runs f on reports.json under <state>/reports.lock and writes the
// result back. The state directory must exist: a report never creates it
// (pcb creates it, with its owner and mode, when it first needs it).
// Where locks are unsupported it runs unlocked.
func (s Store) locked(ctx context.Context, f func(*State) error) error {
	if s.StateDir == "" {
		return fmt.Errorf("crash reports: no state directory: %w", fs.ErrNotExist)
	}
	if info, err := os.Stat(s.StateDir); err != nil {
		return fmt.Errorf("crash reports: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("crash reports: %s is not a directory: %w", s.StateDir, fs.ErrNotExist)
	}
	unlock, err := filelock.Lock(ctx, lockPath(s.StateDir))
	switch {
	case errors.Is(err, filelock.ErrUnsupported):
	case err != nil:
		return err
	default:
		defer unlock()
	}
	st := s.load()
	if err := f(&st); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(StorePath(s.StateDir), append(b, '\n'))
}

// load reads reports.json; no state directory, a missing or a damaged file
// is the zero State.
// Each field is read on its own: a damaged field is dropped, never the
// mode with it, and a mode that is not one reads as none (Ask).
func (s Store) load() State {
	if s.StateDir == "" {
		return State{}
	}
	b, err := atomicfile.ReadFile(StorePath(s.StateDir))
	if err != nil {
		return State{}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return State{}
	}
	mode := field[Mode](fields, "mode")
	if !valid(mode) {
		mode = ""
	}
	return State{
		Mode:       mode,
		Seen:       field[map[string]string](fields, "seen"),
		RetryAfter: field[time.Time](fields, "retry_after"),
		SentDay:    field[string](fields, "sent_day"),
		SentCount:  field[int](fields, "sent_count"),
		Attempts:   field[map[string]int](fields, "attempts"),
		NoticedDay: field[string](fields, "noticed_day"),
	}
}

// field decodes fields[name] as a T, or is T's zero value when it is
// missing or of another shape.
func field[T any](fields map[string]json.RawMessage, name string) T {
	var v T
	if json.Unmarshal(fields[name], &v) != nil {
		var zero T
		return zero
	}
	return v
}
