// Package pipeline implements the workflow stages. Each stage is a separate,
// explicitly triggered operation that reads the project, calls a model, and
// writes its result back to the project (and generated files to the asset
// library). Nothing runs automatically.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/catalog"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// ErrBusy is returned when the same step is already running.
var ErrBusy = errors.New("this step is already running")

// Event is a progress notification streamed to the UI.
type Event struct {
	Type string `json:"type"` // "delta" (model text) or "status"
	Text string `json:"text"`
}

// Emit receives progress events.
type Emit func(Event)

// RunRequest identifies one step execution.
type RunRequest struct {
	ProjectID string
	Stage     string
	// Target is a visual or shot ID for per-item stages, or "all".
	Target string
	// Provider and Model override the configured defaults for this run.
	Provider string
	Model    string
	// OnlyMissing, with Target "all", skips items that already have output.
	OnlyMissing bool
}

// Service runs pipeline stages.
type Service struct {
	Config   *config.Manager
	Projects *project.Store
	Assets   *assets.Store
	Prompts  *Prompts
	// Log records every model request; nil disables logging.
	Log *reqlog.Store
	// Models caches the input schemas of video models; nil disables the cache.
	Models *catalog.Store

	// Factories; replaceable in tests.
	NewLLM   func(p *config.Provider) (llm.Client, error)
	NewImage func(p *config.Provider) (imagegen.Generator, error)
	// FetchSchema asks a provider which inputs a model takes.
	FetchSchema func(ctx context.Context, p *config.Provider, model string) (*imagegen.InputSchema, error)

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New wires a Service with the real provider factories.
func New(cfg *config.Manager, projects *project.Store, lib *assets.Store, prompts *Prompts) *Service {
	return &Service{
		Config: cfg, Projects: projects, Assets: lib, Prompts: prompts,
		NewLLM: func(p *config.Provider) (llm.Client, error) {
			return llm.New(llm.Options{Type: p.Type, BaseURL: p.BaseURL, APIKey: p.Key(), Headers: p.Headers})
		},
		NewImage: func(p *config.Provider) (imagegen.Generator, error) {
			return imagegen.New(imagegen.Options{Type: p.Type, BaseURL: p.BaseURL, APIKey: p.Key(), Headers: p.Headers})
		},
		FetchSchema: catalog.FetchSchema,
		running:     map[string]context.CancelFunc{},
	}
}

// IsRunning reports whether a run key is active for a project.
func (s *Service) IsRunning(projectID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[projectID+"/"+key] != nil
}

// Cancel stops an active run. It reports whether one was found.
func (s *Service) Cancel(projectID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.running[projectID+"/"+key]; c != nil {
		c()
		return true
	}
	return false
}

func (s *Service) acquire(k string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[k] != nil {
		return false
	}
	s.running[k] = cancel
	return true
}

func (s *Service) release(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, k)
}

// Run executes one step and records its outcome in the project.
func (s *Service) Run(ctx context.Context, r RunRequest, emit Emit) error {
	if emit == nil {
		emit = func(Event) {}
	}
	if r.Target == "" && perItem(r.Stage) {
		r.Target = "all"
	}
	key := project.RunKey(r.Stage, r.Target)
	lock := r.ProjectID + "/" + key
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.acquire(lock, cancel) {
		return ErrBusy
	}
	defer s.release(lock)

	var fn func() (string, string, error)
	switch r.Stage {
	case project.StageIdea:
		fn = func() (string, string, error) { return s.runIdea(ctx, r, emit) }
	case project.StageVisualPrompts:
		fn = func() (string, string, error) { return s.runVisualPrompts(ctx, r, emit) }
	case project.StageVisualImages:
		fn = func() (string, string, error) { return s.runVisualImages(ctx, r, emit) }
	case project.StageStoryboard:
		fn = func() (string, string, error) { return s.runStoryboard(ctx, r, emit) }
	case project.StageShotPrompts:
		fn = func() (string, string, error) { return s.runShotPrompts(ctx, r, emit) }
	case project.StageKeyframes:
		fn = func() (string, string, error) { return s.runKeyframes(ctx, r, emit) }
	case project.StageClips:
		fn = func() (string, string, error) { return s.runClips(ctx, r, emit) }
	default:
		return fmt.Errorf("unknown stage %q", r.Stage)
	}

	start := time.Now().UTC()
	if _, err := s.Projects.Update(r.ProjectID, func(p *project.Project) error {
		setRun(p, key, &project.Run{Status: "running", StartedAt: start})
		return nil
	}); err != nil {
		return err
	}
	prov, model, err := fn()
	run := &project.Run{Status: "done", Provider: prov, Model: model, StartedAt: start, EndedAt: time.Now().UTC()}
	if err != nil {
		run.Status, run.Error = "error", err.Error()
		if ctx.Err() != nil {
			run.Status, run.Error = "error", "stopped"
			err = errors.New("stopped")
		}
	}
	_, _ = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
		setRun(p, key, run)
		return nil
	})
	return err
}

func perItem(stage string) bool {
	return stage == project.StageVisualImages || stage == project.StageShotPrompts ||
		stage == project.StageKeyframes || stage == project.StageClips
}

func setRun(p *project.Project, key string, r *project.Run) {
	if p.Runs == nil {
		p.Runs = map[string]*project.Run{}
	}
	p.Runs[key] = r
}

// writeLog stores a request log entry; failures only produce a status line.
func (s *Service) writeLog(e *reqlog.Entry, start time.Time, err error, emit Emit) {
	e.Time, e.DurationMS, e.Status = start, time.Since(start).Milliseconds(), "ok"
	if err != nil {
		e.Status, e.Error = "error", err.Error()
	}
	if werr := s.Log.Write(e); werr != nil {
		emit(Event{Type: "status", Text: "Could not write the request log: " + werr.Error()})
	}
}

// forEach runs fn over item IDs (all, or one), continuing past failures.
// labels name the items in the progress lines.
func forEach(ctx context.Context, ids, labels []string, emit Emit, fn func(id string) error) error {
	var failed []string
	for i, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(ids) > 1 {
			emit(Event{Type: "status", Text: fmt.Sprintf("── %s (%d/%d)", labels[i], i+1, len(ids))})
		}
		if err := fn(id); err != nil {
			if len(ids) == 1 {
				return err
			}
			emit(Event{Type: "status", Text: "Error: " + err.Error()})
			failed = append(failed, labels[i])
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d failed: %s", len(failed), len(ids), strings.Join(failed, ", "))
	}
	return nil
}

func (s *Service) shotTargets(p *project.Project, r RunRequest, skip func(*project.Shot) bool) ([]string, []string, error) {
	var ids, labels []string
	for i := range p.Shots {
		sh := &p.Shots[i]
		if r.Target != "all" && sh.ID != r.Target {
			continue
		}
		if r.Target == "all" && r.OnlyMissing && skip(sh) {
			continue
		}
		ids, labels = append(ids, sh.ID), append(labels, fmt.Sprintf("Shot %d", sh.Number))
	}
	if len(ids) == 0 {
		if r.Target != "all" {
			return nil, nil, errors.New("shot not found")
		}
		return nil, nil, errors.New("no shots to process")
	}
	return ids, labels, nil
}
