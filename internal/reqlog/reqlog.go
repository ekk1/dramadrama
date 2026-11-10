// Package reqlog records every model request (LLM calls and media
// generation) so prompts and failures can be inspected afterwards.
//
// There is no index: each request is one JSON file, and the directory tree
// is the database.
//
//	<dir>/2006-01-02/150405.000_<kind>_<stage>_<id>.json
//
// Days sort by name, and so do the files inside a day. The server only
// lists, reads and deletes these files.
package reqlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Kinds of request.
const (
	KindLLM   = "llm"
	KindImage = "image"
	KindVideo = "video"
)

// ErrNotFound is returned for unknown or malformed entry references.
var ErrNotFound = errors.New("log entry not found")

// Entry is one recorded request. Request and Response hold whatever the
// caller finds useful for debugging; they must never contain API keys, and
// binary inputs are recorded as asset IDs, not content.
type Entry struct {
	// Day and Name locate the file; they are set by Write and List.
	Day  string `json:"-"`
	Name string `json:"-"`

	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	ProjectID  string    `json:"project_id,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Target     string    `json:"target,omitempty"` // visual or shot ID
	Label      string    `json:"label,omitempty"`  // human-readable target, e.g. "Shot 3"
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Status     string    `json:"status"` // "ok" or "error"
	Error      string    `json:"error,omitempty"`

	Request  map[string]any `json:"request,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// Day summarises one day's directory.
type Day struct {
	Date  string
	Count int
	Size  int64
}

// Store manages the log directory. A nil *Store is valid and discards
// everything, so logging is optional for callers and tests.
type Store struct {
	dir string
}

// Open returns a store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

var (
	dayRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	nameRe = regexp.MustCompile(`^[0-9]{6}\.[0-9]{3}_[a-z0-9_-]+$`)
	slugRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "x"
	}
	return s
}

// Write stores e as a new file and fills in e.Day and e.Name. Time defaults
// to now.
func (s *Store) Write(e *Entry) error {
	if s == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	t := e.Time.Local()
	e.Day = t.Format("2006-01-02")
	e.Name = fmt.Sprintf("%s_%s_%s_%s", t.Format("150405.000"), slug(e.Kind), slug(e.Stage), hex.EncodeToString(b[:]))
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, e.Day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Prompts can contain private story material: keep the files private.
	return os.WriteFile(filepath.Join(dir, e.Name+".json"), data, 0o600)
}

// Days lists the days that have entries, newest first.
func (s *Store) Days() ([]Day, error) {
	if s == nil {
		return nil, nil
	}
	dirs, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Day
	for _, d := range dirs {
		if !d.IsDir() || !dayRe.MatchString(d.Name()) {
			continue
		}
		day := Day{Date: d.Name()}
		files, _ := os.ReadDir(filepath.Join(s.dir, d.Name()))
		for _, f := range files {
			if info, err := f.Info(); err == nil && strings.HasSuffix(f.Name(), ".json") {
				day.Count++
				day.Size += info.Size()
			}
		}
		if day.Count > 0 {
			out = append(out, day)
		}
	}
	slices.SortFunc(out, func(a, b Day) int { return strings.Compare(b.Date, a.Date) })
	return out, nil
}

// List returns the entries of one day, newest first. Files that cannot be
// parsed are skipped.
func (s *Store) List(day string) ([]*Entry, error) {
	if s == nil || !dayRe.MatchString(day) {
		return nil, nil
	}
	files, err := os.ReadDir(filepath.Join(s.dir, day))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for i := len(files) - 1; i >= 0; i-- { // ReadDir sorts by name = by time
		name, ok := strings.CutSuffix(files[i].Name(), ".json")
		if !ok {
			continue
		}
		if e, err := s.Get(day, name); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Store) path(day, name string) (string, error) {
	if s == nil || !dayRe.MatchString(day) || !nameRe.MatchString(name) {
		return "", ErrNotFound
	}
	return filepath.Join(s.dir, day, name+".json"), nil
}

// Raw returns the stored JSON of one entry.
func (s *Store) Raw(day, name string) ([]byte, error) {
	path, err := s.path(day, name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

// Get reads one entry.
func (s *Store) Get(day, name string) (*Entry, error) {
	data, err := s.Raw(day, name)
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	e.Day, e.Name = day, name
	return &e, nil
}

// DeleteDay removes all entries of one day.
func (s *Store) DeleteDay(day string) error {
	if s == nil || !dayRe.MatchString(day) {
		return ErrNotFound
	}
	return os.RemoveAll(filepath.Join(s.dir, day))
}

// DeleteAll removes every day.
func (s *Store) DeleteAll() error {
	days, err := s.Days()
	if err != nil {
		return err
	}
	for _, d := range days {
		if err := s.DeleteDay(d.Date); err != nil {
			return err
		}
	}
	return nil
}
