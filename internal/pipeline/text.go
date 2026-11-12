package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

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

// callInfo says what a model request is for; it ends up in the request log.
type callInfo struct {
	ProjectID string
	Stage     string
	Target    string   // visual or shot ID, if the call is for one item
	Label     string   // human-readable target
	ImageIDs  []string // assets attached to an LLM call
}

// complete streams one LLM call, forwarding text deltas to emit.
func (s *Service) complete(ctx context.Context, sm config.StageModel, info callInfo, system, user string, images []llm.Part, jsonMode bool, emit Emit) (string, error) {
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
	start := time.Now()
	resp, err := llm.Collect(client.Stream(ctx, req), func(d string) { emit(Event{Type: "delta", Text: d}) })
	entry := &reqlog.Entry{
		Kind: reqlog.KindLLM, ProjectID: info.ProjectID, Stage: info.Stage, Target: info.Target, Label: info.Label,
		Provider: sm.Provider, Model: sm.Model,
		Request:  map[string]any{"system": system, "user": user, "json": jsonMode},
		Response: map[string]any{"text": resp.Text, "finish_reason": resp.FinishReason, "usage": resp.Usage},
	}
	if len(info.ImageIDs) > 0 {
		entry.Request["images"] = info.ImageIDs
	}
	if sm.Temperature != nil {
		entry.Request["temperature"] = *sm.Temperature
	}
	if sm.MaxTokens > 0 {
		entry.Request["max_tokens"] = sm.MaxTokens
	}
	s.writeLog(entry, start, err, emit)
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
func (s *Service) llmImages(ids []string, emit Emit) (out []llm.Part, used []string) {
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
		used = append(used, a.ID)
	}
	return out, used
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

// projectText runs an LLM stage that reads the whole project: it checks
// that the stage has its input, renders the stage's prompts, attaches the
// project's reference images if the stage is set to send them, and returns
// the model's text.
func (s *Service) projectText(ctx context.Context, r RunRequest, jsonMode bool, ready func(*project.Project) error, emit Emit) (config.StageModel, string, error) {
	sm := s.stageModel(r.Stage, r)
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return sm, "", err
	}
	if err := ready(p); err != nil {
		return sm, "", err
	}
	var images []llm.Part
	var imageIDs []string
	if sm.SendImages {
		images, imageIDs = s.llmImages(p.ReferenceIDs, emit)
	}
	sys, user, err := s.render(r.Stage, promptData{P: p, ImageCount: len(images)})
	if err != nil {
		return sm, "", err
	}
	text, err := s.complete(ctx, sm, callInfo{ProjectID: p.ID, Stage: r.Stage, ImageIDs: imageIDs}, sys, user, images, jsonMode, emit)
	return sm, text, err
}

// needStory is the input check of the stages that read the story.
func needStory(p *project.Project) error {
	if strings.TrimSpace(p.Story) == "" {
		return errors.New("the story is empty: run idea expansion or write the story first")
	}
	return nil
}

func (s *Service) runIdea(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm, text, err := s.projectText(ctx, r, false, func(p *project.Project) error {
		if strings.TrimSpace(p.Idea) == "" {
			return errors.New("enter a core idea first")
		}
		return nil
	}, emit)
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
	sm, text, err := s.projectText(ctx, r, true, needStory, emit)
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
// Stage 4: storyboard overview

func (s *Service) runStoryboard(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	sm, text, err := s.projectText(ctx, r, true, needStory, emit)
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
	err = forEach(ctx, ids, labels, emit, func(id string) error {
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
		info := callInfo{ProjectID: p.ID, Stage: project.StageShotPrompts, Target: id, Label: fmt.Sprintf("Shot %d", p.Shots[idx].Number)}
		text, err := s.complete(ctx, sm, info, sys, user, nil, true, emit)
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
