// Package project holds the per-project workflow state and its JSON store.
// Generated binaries never live here: projects reference assets by ID.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
)

// Stage identifiers, in pipeline order.
const (
	StageInput         = "input"
	StageIdea          = "idea"           // 2. idea expansion (LLM)
	StageVisualPrompts = "visual_prompts" // 3a. visual prompt generation (LLM)
	StageVisualImages  = "visual_images"  // 3b. concept image generation
	StageStoryboard    = "storyboard"     // 4. storyboard overview (LLM)
	StageShotPrompts   = "shot_prompts"   // 5. per-shot prompts (LLM)
	StageKeyframes     = "keyframes"      // 6. final shot generation
)

// ErrNotFound is returned for unknown project IDs.
var ErrNotFound = errors.New("project not found")

// Project is the full state of one short-drama project.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// 1. Input
	Idea         string   `json:"idea"`
	Requirements string   `json:"requirements"` // e.g. episode length, number of shots, tone
	AspectRatio  string   `json:"aspect_ratio"`
	ReferenceIDs []string `json:"reference_ids"` // style reference assets

	// 2. Expanded story
	Story string `json:"story"`

	// 3. Visual development
	Style      string         `json:"style"` // global visual style, appended to image prompts
	Visuals    []VisualPrompt `json:"visuals"`
	VisualsRaw string         `json:"visuals_raw,omitempty"` // last LLM output if it could not be parsed

	// 4-6. Storyboard and shots
	Shots         []Shot `json:"shots"`
	StoryboardRaw string `json:"storyboard_raw,omitempty"`

	// Status of the most recent run of each stage.
	Runs map[string]*Run `json:"runs,omitempty"`
}

// VisualPrompt is one character / scene / prop design.
type VisualPrompt struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"` // character | scene | prop | style
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Prompt       string   `json:"prompt"` // English image prompt
	ReferenceIDs []string `json:"reference_ids,omitempty"`
	AssetIDs     []string `json:"asset_ids,omitempty"` // generated concept images, newest last
}

// Shot is one storyboard shot. Stage 4 fills the overview fields, stage 5
// the prompts, stage 6 the keyframes. ClipID is reserved for video.
type Shot struct {
	ID         string   `json:"id"`
	Number     int      `json:"number"`
	Scene      string   `json:"scene"`
	Characters []string `json:"characters,omitempty"`
	Action     string   `json:"action"`
	Dialogue   string   `json:"dialogue,omitempty"`
	Camera     string   `json:"camera,omitempty"`
	Duration   float64  `json:"duration,omitempty"` // seconds

	StoryboardPrompt string `json:"storyboard_prompt,omitempty"`
	KeyframePrompt   string `json:"keyframe_prompt,omitempty"`
	MotionPrompt     string `json:"motion_prompt,omitempty"` // for future image-to-video

	ReferenceIDs []string `json:"reference_ids,omitempty"` // explicit conditioning images
	KeyframeIDs  []string `json:"keyframe_ids,omitempty"`  // generated keyframes, newest last
	ClipID       string   `json:"clip_id,omitempty"`       // reserved: generated video clip
}

// Run records the latest execution of a stage (or stage+target).
type Run struct {
	Status    string    `json:"status"` // running | done | error
	Error     string    `json:"error,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// Visual returns the visual prompt with the given ID.
func (p *Project) Visual(id string) *VisualPrompt {
	for i := range p.Visuals {
		if p.Visuals[i].ID == id {
			return &p.Visuals[i]
		}
	}
	return nil
}

// Shot returns the shot with the given ID.
func (p *Project) Shot(id string) *Shot {
	for i := range p.Shots {
		if p.Shots[i].ID == id {
			return &p.Shots[i]
		}
	}
	return nil
}

// ShotIndex returns the index of a shot, or -1.
func (p *Project) ShotIndex(id string) int {
	return slices.IndexFunc(p.Shots, func(s Shot) bool { return s.ID == id })
}

// Renumber sets shot numbers to their 1-based position.
func (p *Project) Renumber() {
	for i := range p.Shots {
		p.Shots[i].Number = i + 1
	}
}

// RemoveAssetRefs drops an asset ID from every reference list in the
// project. Used when an asset is deleted from the library.
func (p *Project) RemoveAssetRefs(id string) {
	del := func(s []string) []string { return slices.DeleteFunc(s, func(x string) bool { return x == id }) }
	p.ReferenceIDs = del(p.ReferenceIDs)
	for i := range p.Visuals {
		p.Visuals[i].ReferenceIDs = del(p.Visuals[i].ReferenceIDs)
		p.Visuals[i].AssetIDs = del(p.Visuals[i].AssetIDs)
	}
	for i := range p.Shots {
		p.Shots[i].ReferenceIDs = del(p.Shots[i].ReferenceIDs)
		p.Shots[i].KeyframeIDs = del(p.Shots[i].KeyframeIDs)
		if p.Shots[i].ClipID == id {
			p.Shots[i].ClipID = ""
		}
	}
}

// RunKey builds the key for Project.Runs.
func RunKey(stage, target string) string {
	if target == "" {
		return stage
	}
	return stage + ":" + target
}

// Store persists projects as <dir>/<id>/project.json.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open returns a store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return "", ErrNotFound
	}
	return filepath.Join(s.dir, id, "project.json"), nil
}

func (s *Store) load(id string) (*Project, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("project %s: %w", id, err)
	}
	return &p, nil
}

func (s *Store) save(p *Project) error {
	path, err := s.path(p.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return assets.WriteFileAtomic(path, data)
}

// Create stores a new project.
func (s *Store) Create(name, idea string) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	p := &Project{
		ID: assets.NewID("p"), Name: strings.TrimSpace(name), Idea: idea,
		AspectRatio: "9:16", CreatedAt: now, UpdatedAt: now,
	}
	if p.Name == "" {
		p.Name = "Untitled"
	}
	return p, s.save(p)
}

// Get loads a project.
func (s *Store) Get(id string) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

// Update loads a project, applies fn and saves it, all under the store
// lock, so concurrent stage runs and user edits never clobber each other.
// If fn returns an error nothing is saved.
func (s *Store) Update(id string, fn func(*Project) error) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if err := fn(p); err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Now().UTC()
	return p, s.save(p)
}

// List returns all projects, most recently updated first.
func (s *Store) List() ([]*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := s.load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Delete removes a project directory. Its assets stay in the library.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(path))
}
