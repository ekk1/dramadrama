// Package pipeline implements the workflow stages. Each stage is a separate,
// explicitly triggered operation that reads the project, calls a model, and
// writes its result back to the project (and generated files to the asset
// library). Nothing runs automatically.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/project"
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

	// Factories; replaceable in tests.
	NewLLM   func(p *config.Provider) (llm.Client, error)
	NewImage func(p *config.Provider) (imagegen.Generator, error)

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
		running: map[string]context.CancelFunc{},
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
	return stage == project.StageVisualImages || stage == project.StageShotPrompts || stage == project.StageKeyframes
}

func setRun(p *project.Project, key string, r *project.Run) {
	if p.Runs == nil {
		p.Runs = map[string]*project.Run{}
	}
	p.Runs[key] = r
}

// ---------------------------------------------------------------------------
// LLM helpers

type promptData struct {
	P          *project.Project
	Shot       *project.Shot
	Prev, Next *project.Shot
	ImageCount int
}

// stageModel resolves the provider/model for an LLM stage, applying overrides.
func (s *Service) stageModel(stage string, r RunRequest) config.StageModel {
	sm := s.Config.Get().Stages[stage]
	if r.Provider != "" {
		sm.Provider = r.Provider
	}
	if r.Model != "" {
		sm.Model = r.Model
	}
	return sm
}

// complete streams one LLM call, forwarding text deltas to emit.
func (s *Service) complete(ctx context.Context, sm config.StageModel, system, user string, images []llm.Part, jsonMode bool, emit Emit) (string, error) {
	if sm.Provider == "" || sm.Model == "" {
		return "", errors.New("no provider/model configured for this step; set one in Settings or in the run options")
	}
	cfg := s.Config.Get()
	p := cfg.Provider(sm.Provider)
	if p == nil {
		return "", fmt.Errorf("unknown provider %q", sm.Provider)
	}
	if !p.IsLLM() {
		return "", fmt.Errorf("provider %q is not an LLM provider", sm.Provider)
	}
	if err := keyCheck(p); err != nil {
		return "", err
	}
	client, err := s.NewLLM(p)
	if err != nil {
		return "", err
	}
	content := append(slices.Clone(images), llm.TextPart(user))
	req := &llm.Request{
		Model:       sm.Model,
		System:      system,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: content}},
		Temperature: sm.Temperature,
		MaxTokens:   sm.MaxTokens,
		JSON:        jsonMode,
	}
	emit(Event{Type: "status", Text: fmt.Sprintf("Calling %s / %s…", sm.Provider, sm.Model)})
	resp, err := llm.Collect(client.Stream(ctx, req), func(d string) { emit(Event{Type: "delta", Text: d}) })
	if err != nil {
		return "", err
	}
	switch strings.ToLower(resp.FinishReason) {
	case "length", "max_tokens", "max_output_tokens":
		emit(Event{Type: "status", Text: "Warning: the output hit the token limit and may be truncated."})
	case "":
		// Every provider reports a finish reason at the end of a complete
		// stream; none means the connection closed early.
		emit(Event{Type: "status", Text: "Warning: the stream ended without a finish reason; the output may be incomplete."})
	}
	if strings.TrimSpace(resp.Text) == "" {
		return "", fmt.Errorf("model returned no text (finish reason %q)", resp.FinishReason)
	}
	return resp.Text, nil
}

// keyCheck fails early with an actionable message when a provider that
// requires a key has none. OpenAI-compatible endpoints are exempt because
// local servers (Ollama, vLLM, …) often need no key.
func keyCheck(p *config.Provider) error {
	if p.HasKey() || p.Type == llm.TypeOpenAIChat {
		return nil
	}
	if p.APIKeyEnv != "" {
		return fmt.Errorf("provider %q has no API key: set the %s environment variable or store a key in Settings", p.Name, p.APIKeyEnv)
	}
	return fmt.Errorf("provider %q has no API key: add one in Settings", p.Name)
}

// maxLLMImageBytes skips images larger than most providers accept inline.
const maxLLMImageBytes = 5 << 20

var llmImageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// llmImages loads reference assets as inline image parts.
func (s *Service) llmImages(ids []string, emit Emit) []llm.Part {
	var out []llm.Part
	for _, id := range ids {
		if len(out) >= 6 {
			emit(Event{Type: "status", Text: "Only the first 6 reference images are sent to the model."})
			break
		}
		a, data, err := s.Assets.Read(id)
		if err != nil || !slices.Contains(llmImageTypes, a.MIME) {
			continue
		}
		if len(data) > maxLLMImageBytes {
			emit(Event{Type: "status", Text: fmt.Sprintf("Skipping reference %s: larger than 5 MB.", a.ID)})
			continue
		}
		out = append(out, llm.ImagePart(a.MIME, data))
	}
	return out
}

func (s *Service) render(stage string, data promptData) (string, string, error) {
	sys, err := s.Prompts.Render(stage+".system", data)
	if err != nil {
		return "", "", err
	}
	user, err := s.Prompts.Render(stage+".user", data)
	return sys, user, err
}

// ---------------------------------------------------------------------------
// Stage 2: idea expansion

func (s *Service) runIdea(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm := s.stageModel(project.StageIdea, r)
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	if strings.TrimSpace(p.Idea) == "" {
		return sm.Provider, sm.Model, errors.New("enter a core idea first")
	}
	var images []llm.Part
	if sm.SendImages {
		images = s.llmImages(p.ReferenceIDs, emit)
	}
	sys, user, err := s.render(project.StageIdea, promptData{P: p, ImageCount: len(images)})
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	text, err := s.complete(ctx, sm, sys, user, images, false, emit)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
		p.Story = strings.TrimSpace(text)
		return nil
	})
	return sm.Provider, sm.Model, err
}

// ---------------------------------------------------------------------------
// Stage 3a: visual prompts

func (s *Service) runVisualPrompts(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm := s.stageModel(project.StageVisualPrompts, r)
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	if strings.TrimSpace(p.Story) == "" {
		return sm.Provider, sm.Model, errors.New("the story is empty: run idea expansion or write the story first")
	}
	var images []llm.Part
	if sm.SendImages {
		images = s.llmImages(p.ReferenceIDs, emit)
	}
	sys, user, err := s.render(project.StageVisualPrompts, promptData{P: p, ImageCount: len(images)})
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	text, err := s.complete(ctx, sm, sys, user, images, true, emit)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	return sm.Provider, sm.Model, s.ApplyVisualsJSON(r.ProjectID, text)
}

type visualsJSON struct {
	Style string `json:"style"`
	Items []struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	} `json:"items"`
}

// ApplyVisualsJSON parses visual-prompt JSON (from the model, or edited by
// hand) and replaces the project's visual list. Generated images and
// references of items with the same kind and name are kept. If parsing
// fails, the raw text is saved so the user can fix it.
func (s *Service) ApplyVisualsJSON(projectID, raw string) error {
	var out visualsJSON
	perr := extractJSON(raw, &out)
	if perr == nil && len(out.Items) == 0 {
		perr = errors.New(`no "items" in output`)
	}
	_, err := s.Projects.Update(projectID, func(p *project.Project) error {
		if perr != nil {
			p.VisualsRaw = raw
			return nil
		}
		old := p.Visuals
		p.Visuals = nil
		for _, it := range out.Items {
			v := project.VisualPrompt{
				ID:          assets.NewID("v"),
				Kind:        normKind(it.Kind),
				Name:        strings.TrimSpace(it.Name),
				Description: strings.TrimSpace(it.Description),
				Prompt:      strings.TrimSpace(it.Prompt),
			}
			for _, o := range old {
				if o.Kind == v.Kind && strings.EqualFold(o.Name, v.Name) {
					v.AssetIDs, v.ReferenceIDs = o.AssetIDs, o.ReferenceIDs
					break
				}
			}
			p.Visuals = append(p.Visuals, v)
		}
		if st := strings.TrimSpace(out.Style); st != "" {
			p.Style = st
		}
		p.VisualsRaw = ""
		return nil
	})
	if err != nil {
		return err
	}
	if perr != nil {
		return fmt.Errorf("could not parse the output as JSON (%v); the raw text was saved so you can fix and apply it", perr)
	}
	return nil
}

func normKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "character", "scene", "prop", "style":
		return k
	case "location", "setting", "environment":
		return "scene"
	case "object", "item":
		return "prop"
	}
	return "character"
}

// ---------------------------------------------------------------------------
// Image helpers

func withStyle(prompt, style string) string {
	prompt, style = strings.TrimSpace(prompt), strings.TrimSpace(style)
	if style == "" || strings.Contains(prompt, style) {
		return prompt
	}
	return strings.TrimRight(prompt, " .,;") + ". " + style
}

type imageResult struct {
	Assets   []*assets.Asset
	Provider string
	Model    string
}

// generateImage calls the image backend and stores the outputs as assets.
func (s *Service) generateImage(ctx context.Context, r RunRequest, prompt string, refIDs []string, meta assets.Asset, emit Emit) (*imageResult, error) {
	cfg := s.Config.Get()
	ic := cfg.Image
	provName := ic.Provider
	if r.Provider != "" {
		provName = r.Provider
	}
	res := &imageResult{Provider: provName}
	p := cfg.Provider(provName)
	if p == nil || !p.IsImage() {
		return res, fmt.Errorf("%q is not a configured image provider", provName)
	}
	if err := keyCheck(p); err != nil {
		return res, err
	}
	var refs []imagegen.Image
	var used []string
	for _, id := range refIDs {
		a, data, err := s.Assets.Read(id)
		if err != nil || !a.IsImage() {
			continue
		}
		refs = append(refs, imagegen.Image{MIME: a.MIME, Data: data})
		used = append(used, id)
	}
	model := r.Model
	if model == "" {
		model = ic.Model
		if len(refs) > 0 && ic.RefModel != "" {
			model = ic.RefModel
		}
	}
	res.Model = model
	if model == "" {
		return res, errors.New("no image model configured; set one in Settings")
	}
	aspect := ic.AspectRatio
	if pr, err := s.Projects.Get(r.ProjectID); err == nil && pr.AspectRatio != "" {
		aspect = pr.AspectRatio
	}
	gen, err := s.NewImage(p)
	if err != nil {
		return res, err
	}
	emit(Event{Type: "status", Text: fmt.Sprintf("Generating with %s / %s (%d reference image(s))…", provName, model, len(refs))})
	out, err := gen.Generate(ctx, &imagegen.Request{
		Model: model, Prompt: prompt, AspectRatio: aspect, NumImages: 1, References: refs, Extra: ic.Extra,
	})
	if err != nil {
		return res, err
	}
	for _, img := range out.Images {
		m := meta
		m.MIME = img.MIME
		m.Width, m.Height = img.Width, img.Height
		m.Prompt = prompt
		m.Model = model
		m.Parents = used
		m.ProjectID = r.ProjectID
		m.Params = map[string]any{"provider": provName, "aspect_ratio": aspect}
		if out.Seed != nil {
			m.Params["seed"] = *out.Seed
		}
		if out.RequestID != "" {
			m.Params["request_id"] = out.RequestID
		}
		a, err := s.Assets.Put(m, img.Data)
		if err != nil {
			return res, err
		}
		emit(Event{Type: "status", Text: "Saved " + a.ID})
		res.Assets = append(res.Assets, a)
	}
	return res, nil
}

func assetIDs(list []*assets.Asset) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

// forEach runs fn over item IDs (all, or one), continuing past failures.
func forEach(ctx context.Context, ids []string, label func(i int) string, emit Emit, fn func(id string) error) error {
	var failed []string
	for i, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(ids) > 1 {
			emit(Event{Type: "status", Text: fmt.Sprintf("── %s (%d/%d)", label(i), i+1, len(ids))})
		}
		if err := fn(id); err != nil {
			if len(ids) == 1 {
				return err
			}
			emit(Event{Type: "status", Text: "Error: " + err.Error()})
			failed = append(failed, label(i))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d failed: %s", len(failed), len(ids), strings.Join(failed, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stage 3b: concept images

func (s *Service) runVisualImages(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return "", "", err
	}
	var ids, labels []string
	for _, v := range p.Visuals {
		if r.Target != "all" && v.ID != r.Target {
			continue
		}
		if r.Target == "all" && r.OnlyMissing && len(v.AssetIDs) > 0 {
			continue
		}
		ids, labels = append(ids, v.ID), append(labels, v.Name)
	}
	if len(ids) == 0 {
		if r.Target != "all" {
			return "", "", errors.New("visual item not found")
		}
		return "", "", errors.New("nothing to generate")
	}
	var last *imageResult
	err = forEach(ctx, ids, func(i int) string { return labels[i] }, emit, func(id string) error {
		p, err := s.Projects.Get(r.ProjectID)
		if err != nil {
			return err
		}
		v := p.Visual(id)
		if v == nil {
			return errors.New("visual item was deleted")
		}
		if strings.TrimSpace(v.Prompt) == "" {
			return fmt.Errorf("%s has no prompt", v.Name)
		}
		res, err := s.generateImage(ctx, r, withStyle(v.Prompt, p.Style), v.ReferenceIDs, assets.Asset{
			Kind: assets.KindConcept, Name: v.Name, Stage: project.StageVisualImages, Tags: []string{v.Kind},
		}, emit)
		last = res
		if err != nil {
			return err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if v := p.Visual(id); v != nil {
				v.AssetIDs = append(v.AssetIDs, assetIDs(res.Assets)...)
			}
			return nil
		})
		return err
	})
	if last == nil {
		return "", "", err
	}
	return last.Provider, last.Model, err
}

// ---------------------------------------------------------------------------
// Stage 4: storyboard overview

func (s *Service) runStoryboard(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm := s.stageModel(project.StageStoryboard, r)
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	if strings.TrimSpace(p.Story) == "" {
		return sm.Provider, sm.Model, errors.New("the story is empty: run idea expansion or write the story first")
	}
	var images []llm.Part
	if sm.SendImages {
		images = s.llmImages(p.ReferenceIDs, emit)
	}
	sys, user, err := s.render(project.StageStoryboard, promptData{P: p, ImageCount: len(images)})
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	text, err := s.complete(ctx, sm, sys, user, images, true, emit)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	return sm.Provider, sm.Model, s.ApplyStoryboardJSON(r.ProjectID, text)
}

type storyboardJSON struct {
	Shots []struct {
		Scene      string      `json:"scene"`
		Characters flexStrings `json:"characters"`
		Action     string      `json:"action"`
		Dialogue   string      `json:"dialogue"`
		Camera     string      `json:"camera"`
		Duration   flexFloat   `json:"duration"`
	} `json:"shots"`
}

// ApplyStoryboardJSON parses a shot list (from the model, or edited by hand)
// and replaces the project's shots. If parsing fails, the raw text is saved.
func (s *Service) ApplyStoryboardJSON(projectID, raw string) error {
	var out storyboardJSON
	perr := extractJSON(raw, &out)
	if perr == nil && len(out.Shots) == 0 {
		perr = errors.New(`no "shots" in output`)
	}
	_, err := s.Projects.Update(projectID, func(p *project.Project) error {
		if perr != nil {
			p.StoryboardRaw = raw
			return nil
		}
		p.Shots = nil
		for _, sh := range out.Shots {
			p.Shots = append(p.Shots, project.Shot{
				ID:         assets.NewID("s"),
				Scene:      strings.TrimSpace(sh.Scene),
				Characters: []string(sh.Characters),
				Action:     strings.TrimSpace(sh.Action),
				Dialogue:   strings.TrimSpace(sh.Dialogue),
				Camera:     strings.TrimSpace(sh.Camera),
				Duration:   float64(sh.Duration),
			})
		}
		p.Renumber()
		p.StoryboardRaw = ""
		return nil
	})
	if err != nil {
		return err
	}
	if perr != nil {
		return fmt.Errorf("could not parse the output as JSON (%v); the raw text was saved so you can fix and apply it", perr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stage 5: shot-level prompts

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

func (s *Service) runShotPrompts(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm := s.stageModel(project.StageShotPrompts, r)
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	ids, labels, err := s.shotTargets(p, r, func(sh *project.Shot) bool { return sh.KeyframePrompt != "" })
	if err != nil {
		return sm.Provider, sm.Model, err
	}
	err = forEach(ctx, ids, func(i int) string { return labels[i] }, emit, func(id string) error {
		p, err := s.Projects.Get(r.ProjectID)
		if err != nil {
			return err
		}
		idx := p.ShotIndex(id)
		if idx < 0 {
			return errors.New("shot was deleted")
		}
		data := promptData{P: p, Shot: &p.Shots[idx]}
		if idx > 0 {
			data.Prev = &p.Shots[idx-1]
		}
		if idx+1 < len(p.Shots) {
			data.Next = &p.Shots[idx+1]
		}
		sys, user, err := s.render(project.StageShotPrompts, data)
		if err != nil {
			return err
		}
		text, err := s.complete(ctx, sm, sys, user, nil, true, emit)
		if err != nil {
			return err
		}
		emit(Event{Type: "delta", Text: "\n"})
		var out struct {
			StoryboardPrompt string `json:"storyboard_prompt"`
			KeyframePrompt   string `json:"keyframe_prompt"`
			MotionPrompt     string `json:"motion_prompt"`
		}
		if err := extractJSON(text, &out); err != nil {
			return fmt.Errorf("could not parse the output as JSON: %v", err)
		}
		if strings.TrimSpace(out.KeyframePrompt) == "" {
			return errors.New("output has no keyframe_prompt")
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			sh := p.Shot(id)
			if sh == nil {
				return errors.New("shot was deleted")
			}
			sh.StoryboardPrompt = strings.TrimSpace(out.StoryboardPrompt)
			sh.KeyframePrompt = strings.TrimSpace(out.KeyframePrompt)
			sh.MotionPrompt = strings.TrimSpace(out.MotionPrompt)
			return nil
		})
		return err
	})
	return sm.Provider, sm.Model, err
}

// ---------------------------------------------------------------------------
// Stage 6: keyframes

// maxAutoRefs caps automatically selected reference images per shot.
const maxAutoRefs = 4

// ShotRefs returns the reference assets used for a shot's keyframe: the
// shot's explicit references if any, otherwise (auto=true) the latest
// concept image of each character, scene and prop the shot mentions.
func ShotRefs(p *project.Project, sh *project.Shot) (ids []string, auto bool) {
	if len(sh.ReferenceIDs) > 0 {
		return sh.ReferenceIDs, false
	}
	var chars, others []string
	scene := strings.ToLower(sh.Scene)
	action := strings.ToLower(sh.Action)
	for _, v := range p.Visuals {
		if len(v.AssetIDs) == 0 || strings.TrimSpace(v.Name) == "" {
			continue
		}
		latest := v.AssetIDs[len(v.AssetIDs)-1]
		name := strings.ToLower(strings.TrimSpace(v.Name))
		switch v.Kind {
		case "character":
			for _, c := range sh.Characters {
				c = strings.ToLower(strings.TrimSpace(c))
				if c != "" && (c == name || strings.Contains(c, name) || strings.Contains(name, c)) {
					chars = append(chars, latest)
					break
				}
			}
		case "scene":
			if strings.Contains(scene, name) {
				others = append(others, latest)
			}
		case "prop":
			if strings.Contains(action, name) || strings.Contains(scene, name) {
				others = append(others, latest)
			}
		}
	}
	ids = append(chars, others...)
	if len(ids) > maxAutoRefs {
		ids = ids[:maxAutoRefs]
	}
	return ids, true
}

func (s *Service) runKeyframes(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return "", "", err
	}
	ids, labels, err := s.shotTargets(p, r, func(sh *project.Shot) bool { return len(sh.KeyframeIDs) > 0 })
	if err != nil {
		return "", "", err
	}
	var last *imageResult
	err = forEach(ctx, ids, func(i int) string { return labels[i] }, emit, func(id string) error {
		p, err := s.Projects.Get(r.ProjectID)
		if err != nil {
			return err
		}
		sh := p.Shot(id)
		if sh == nil {
			return errors.New("shot was deleted")
		}
		if strings.TrimSpace(sh.KeyframePrompt) == "" {
			return fmt.Errorf("shot %d has no keyframe prompt yet: run step 5 first", sh.Number)
		}
		refs, _ := ShotRefs(p, sh)
		res, err := s.generateImage(ctx, r, withStyle(sh.KeyframePrompt, p.Style), refs, assets.Asset{
			Kind: assets.KindKeyframe, Name: fmt.Sprintf("Shot %d", sh.Number), Stage: project.StageKeyframes,
			ShotID: sh.ID, Tags: []string{"keyframe"},
		}, emit)
		last = res
		if err != nil {
			return err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if sh := p.Shot(id); sh != nil {
				sh.KeyframeIDs = append(sh.KeyframeIDs, assetIDs(res.Assets)...)
			}
			return nil
		})
		return err
	})
	if last == nil {
		return "", "", err
	}
	return last.Provider, last.Model, err
}
