package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// ---------------------------------------------------------------------------
// Image helpers

func withStyle(prompt, style string) string {
	prompt, style = strings.TrimSpace(prompt), strings.TrimSpace(style)
	if style == "" || strings.Contains(prompt, style) {
		return prompt
	}
	return strings.TrimRight(prompt, " .,;") + ". " + style
}

// generateImage calls the image backend and stores the outputs as assets.
// target is the visual or shot ID the image is for.
func (s *Service) generateImage(ctx context.Context, r RunRequest, target, prompt string, refIDs []string, meta assets.Asset, emit Emit) (res *mediaResult, err error) {
	cfg := s.Config.Get()
	ic := cfg.Image
	provName := ic.Provider
	if r.Provider != "" {
		provName = r.Provider
	}
	res = &mediaResult{Provider: provName}
	p := cfg.Provider(provName)
	if p == nil || !p.IsImage() {
		return res, fmt.Errorf("%q is not a configured image provider", provName)
	}
	if err := keyCheck(p); err != nil {
		return res, err
	}
	refs, used := s.loadMedia(refIDs, "image")
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
	greq := &imagegen.Request{
		Model: model, Prompt: prompt, AspectRatio: aspect, NumImages: 1, References: refs, Extra: ic.Extra,
	}
	entry := &reqlog.Entry{
		Kind: reqlog.KindImage, ProjectID: r.ProjectID, Stage: meta.Stage, Target: target, Label: meta.Name,
		Provider: provName, Model: model,
		Request:  map[string]any{"prompt": prompt, "aspect_ratio": aspect},
		Response: map[string]any{},
	}
	if len(used) > 0 {
		entry.Request["references"] = used
	}
	if d, ok := gen.(imagegen.PayloadDescriber); ok {
		entry.Request["payload"] = d.DescribePayload(greq)
	}
	start := time.Now()
	out, err := s.runJob(ctx, r.ProjectID, project.RunKey(meta.Stage, target), provName, gen, greq, emit)
	if err != nil {
		s.writeLog(entry, start, err, emit)
		return res, err
	}
	defer func() { s.logOutputs(entry, start, out, res, err, emit) }()
	meta.Prompt, meta.Model, meta.Parents, meta.ProjectID = prompt, model, used, r.ProjectID
	meta.Params = map[string]any{"provider": provName, "aspect_ratio": aspect}
	return res, s.saveOutputs(out.Images, meta, out, res, emit)
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
	return forEachMedia(ctx, ids, labels, emit, func(id string) (*mediaResult, error) {
		p, err := s.Projects.Get(r.ProjectID)
		if err != nil {
			return nil, err
		}
		v := p.Visual(id)
		if v == nil {
			return nil, errors.New("visual item was deleted")
		}
		if strings.TrimSpace(v.Prompt) == "" {
			return nil, fmt.Errorf("%s has no prompt", v.Name)
		}
		res, err := s.generateImage(ctx, r, id, withStyle(v.Prompt, p.Style), v.ReferenceIDs, assets.Asset{
			Kind: assets.KindConcept, Name: v.Name, Stage: project.StageVisualImages, Tags: []string{v.Kind},
		}, emit)
		if err != nil {
			return res, err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if v := p.Visual(id); v != nil {
				v.AssetIDs = append(v.AssetIDs, assetIDs(res.Assets)...)
			}
			return nil
		})
		return res, err
	})
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
	return forEachMedia(ctx, ids, labels, emit, func(id string) (*mediaResult, error) {
		p, err := s.Projects.Get(r.ProjectID)
		if err != nil {
			return nil, err
		}
		sh := p.Shot(id)
		if sh == nil {
			return nil, errors.New("shot was deleted")
		}
		if strings.TrimSpace(sh.KeyframePrompt) == "" {
			return nil, fmt.Errorf("shot %d has no keyframe prompt yet: run step 5 first", sh.Number)
		}
		refs, _ := ShotRefs(p, sh)
		res, err := s.generateImage(ctx, r, id, withStyle(sh.KeyframePrompt, p.Style), refs, assets.Asset{
			Kind: assets.KindKeyframe, Name: fmt.Sprintf("Shot %d", sh.Number), Stage: project.StageKeyframes,
			ShotID: sh.ID, Tags: []string{"keyframe"},
		}, emit)
		if err != nil {
			return res, err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if sh := p.Shot(id); sh != nil {
				sh.KeyframeIDs = append(sh.KeyframeIDs, assetIDs(res.Assets)...)
			}
			return nil
		})
		return res, err
	})
}
