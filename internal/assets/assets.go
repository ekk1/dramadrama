// Package assets is the central asset library. Every image used or produced
// by the pipeline (reference uploads, concept art, keyframes, and later video
// clips) is stored here once and referenced by ID from anywhere else.
//
// Layout on disk:
//
//	<dir>/index.json            metadata for all assets
//	<dir>/files/<sha256>.<ext>  content, de-duplicated by hash
package assets

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders for DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind classifies an asset by its role in the workflow.
type Kind string

const (
	KindReference Kind = "reference" // user-supplied style/character references
	KindConcept   Kind = "concept"   // stage 3: characters, scenes, props
	KindKeyframe  Kind = "keyframe"  // stage 6: per-shot keyframes
	KindVideo     Kind = "video"     // stage 7: per-shot video clips
)

// Kinds lists all kinds in display order.
var Kinds = []Kind{KindReference, KindConcept, KindKeyframe, KindVideo}

// ErrNotFound is returned for unknown asset IDs.
var ErrNotFound = errors.New("asset not found")

// Asset is the metadata record of one stored file.
type Asset struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Name      string    `json:"name,omitempty"`
	MIME      string    `json:"mime"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Ext       string    `json:"ext"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Provenance.
	ProjectID string         `json:"project_id,omitempty"`
	Stage     string         `json:"stage,omitempty"`
	ShotID    string         `json:"shot_id,omitempty"`
	Prompt    string         `json:"prompt,omitempty"`
	Model     string         `json:"model,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Parents   []string       `json:"parents,omitempty"` // assets used as inputs

	Tags []string `json:"tags,omitempty"`
}

// IsImage reports whether the asset is an image.
func (a *Asset) IsImage() bool { return a.Media() == "image" }

// Media returns the broad type of the asset's content: "image", "video",
// "audio", or "" for anything else.
func (a *Asset) Media() string {
	major, _, _ := strings.Cut(a.MIME, "/")
	switch major {
	case "image", "video", "audio":
		return major
	}
	if a.MIME == "application/ogg" {
		return "audio"
	}
	return ""
}

// Store is a concurrency-safe asset library backed by the filesystem.
type Store struct {
	dir string
	mu  sync.RWMutex
	idx map[string]*Asset
}

// Open loads (or initialises) the library in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, idx: map[string]*Asset{}}
	data, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var list []*Asset
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("assets: corrupt index: %w", err)
		}
		for _, a := range list {
			s.idx[a.ID] = a
		}
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

// FilePath returns the on-disk path of an asset's content.
func (s *Store) FilePath(a *Asset) string {
	return filepath.Join(s.dir, "files", a.SHA256+a.Ext)
}

// saveLocked writes the index atomically. Caller holds the write lock.
func (s *Store) saveLocked() error {
	list := make([]*Asset, 0, len(s.idx))
	for _, a := range s.idx {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.indexPath(), data)
}

// Put stores data and records meta. ID, SHA256, Size, Ext, CreatedAt and
// (when possible) MIME/Width/Height are filled in; other fields are kept.
func (s *Store) Put(meta Asset, data []byte) (*Asset, error) {
	if len(data) == 0 {
		return nil, errors.New("assets: empty content")
	}
	sum := sha256.Sum256(data)
	a := meta
	a.ID = NewID("a")
	a.SHA256 = hex.EncodeToString(sum[:])
	a.Size = int64(len(data))
	if a.MIME == "" || a.MIME == "application/octet-stream" {
		a.MIME = http.DetectContentType(data)
	}
	if mt, _, err := mime.ParseMediaType(a.MIME); err == nil {
		a.MIME = mt
	}
	a.Ext = extFor(a.MIME)
	if a.Kind == "" {
		a.Kind = KindReference
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.IsImage() && (a.Width == 0 || a.Height == 0) {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			a.Width, a.Height = cfg.Width, cfg.Height
		}
	}
	a.Tags = normTags(a.Tags)

	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.FilePath(&a)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := WriteFileAtomic(path, data); err != nil {
			return nil, err
		}
	}
	s.idx[a.ID] = &a
	if err := s.saveLocked(); err != nil {
		delete(s.idx, a.ID)
		return nil, err
	}
	cp := a
	return &cp, nil
}

// Get returns a copy of the asset's metadata.
func (s *Store) Get(id string) (*Asset, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.idx[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

// Read returns an asset's metadata and content.
func (s *Store) Read(id string) (*Asset, []byte, error) {
	a, err := s.Get(id)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(s.FilePath(a))
	return a, data, err
}

// Filter selects assets in List. Zero values match everything.
type Filter struct {
	ProjectID string
	Kind      Kind
	Tag       string
	Query     string // substring of name, prompt or tags
	Media     string // "image", "video" or "audio"
	IDs       []string
}

// List returns matching assets, newest first.
func (s *Store) List(f Filter) []*Asset {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := strings.ToLower(strings.TrimSpace(f.Query))
	var out []*Asset
	for _, a := range s.idx {
		if f.ProjectID != "" && a.ProjectID != f.ProjectID {
			continue
		}
		if f.Kind != "" && a.Kind != f.Kind {
			continue
		}
		if f.Tag != "" && !slices.Contains(a.Tags, f.Tag) {
			continue
		}
		if f.Media != "" && a.Media() != f.Media {
			continue
		}
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, a.ID) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(a.Name+" "+a.Prompt+" "+strings.Join(a.Tags, " ")), q) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Update applies fn to the stored metadata. Content-derived fields
// (ID, SHA256, Size, Ext, MIME) cannot be changed.
func (s *Store) Update(id string, fn func(*Asset)) (*Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.idx[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	fn(&cp)
	cp.ID, cp.SHA256, cp.Size, cp.Ext, cp.MIME = a.ID, a.SHA256, a.Size, a.Ext, a.MIME
	cp.Tags = normTags(cp.Tags)
	s.idx[id] = &cp
	if err := s.saveLocked(); err != nil {
		s.idx[id] = a
		return nil, err
	}
	out := cp
	return &out, nil
}

// Delete removes an asset record, and its file when no other record
// shares the same content.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.idx[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.idx, id)
	if err := s.saveLocked(); err != nil {
		s.idx[id] = a
		return err
	}
	for _, o := range s.idx {
		if o.SHA256 == a.SHA256 && o.Ext == a.Ext {
			return nil
		}
	}
	if err := os.Remove(s.FilePath(a)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Tags returns all tags in use, sorted.
func (s *Store) Tags() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := map[string]bool{}
	for _, a := range s.idx {
		for _, t := range a.Tags {
			set[t] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ParseTags splits a comma/space separated tag string.
func ParseTags(s string) []string {
	return normTags(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }))
}

func normTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func extFor(mt string) string {
	switch mt {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// NewID returns a random identifier with the given prefix, e.g. "a_3f9c…".
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// WriteFileAtomic writes data to a temp file and renames it into place.
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
