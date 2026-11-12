package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

// ---------------------------------------------------------------------------
// Stage 7: video clips

// maxInlineWarn is the total size of inline inputs above which the user is
// warned that the request is heavy.
const maxInlineWarn = 24 << 20

// videoSchema returns the input schema of a video model, fetching and
// caching it on first use. A model whose schema cannot be fetched still
// runs, with only the conventional inputs.
func (s *Service) videoSchema(ctx context.Context, p *config.Provider, model string, emit Emit) *imagegen.InputSchema {
	if sc := s.Models.Schema(p.Name, model); sc != nil {
		return sc
	}
	if s.FetchSchema == nil {
		return nil
	}
	emit(Event{Type: "status", Text: "Fetching the parameter list of " + model + "…"})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sc, err := s.FetchSchema(ctx, p, model)
	if err != nil {
		emit(Event{Type: "status", Text: "Could not fetch the model's parameter list: " + err.Error()})
		return nil
	}
	sc.Model = model // the cache is keyed by the model that was asked for
	if s.Models != nil {
		_ = s.Models.SaveSchema(p.Name, sc)
	}
	return sc
}

// generateClip produces the video clip of one shot from its keyframe (first
// frame), optional last frame, reference media and motion prompt.
func (s *Service) generateClip(ctx context.Context, r RunRequest, p *project.Project, sh *project.Shot, emit Emit) (res *mediaResult, err error) {
	cfg := s.Config.Get()
	vc := cfg.Video
	provName := vc.Provider
	if r.Provider != "" {
		provName = r.Provider
	}
	res = &mediaResult{Provider: provName}
	if provName == "" {
		return res, errors.New("no video provider configured; set one in Settings")
	}
	prov := cfg.Provider(provName)
	if prov == nil || !prov.IsImage() {
		return res, fmt.Errorf("%q is not a configured image or video provider", provName)
	}
	if err := keyCheck(prov); err != nil {
		return res, err
	}
	model := r.Model
	if model == "" {
		model = vc.Model
	}
	res.Model = model
	if model == "" {
		return res, errors.New("no video model configured; set one in Settings")
	}
	prompt := strings.TrimSpace(sh.MotionPrompt)
	if prompt == "" {
		return res, fmt.Errorf("shot %d has no motion prompt yet: run step 5 or write one", sh.Number)
	}

	greq := &imagegen.Request{
		Kind: imagegen.KindVideo, Model: model, Prompt: prompt, AspectRatio: p.AspectRatio, Duration: sh.Duration, Extra: vc.Extra,
	}
	if model == vc.Model {
		greq.Mapping = vc.Mapping // the override is written for the default model
	}
	logReq := map[string]any{"prompt": prompt, "aspect_ratio": p.AspectRatio, "duration": sh.Duration}
	var parents []string
	one := func(id, name string) *imagegen.Image {
		list, used := s.loadMedia([]string{id}, "image")
		if len(list) == 0 {
			return nil
		}
		logReq[name] = used[0]
		parents = append(parents, used[0])
		return &list[0]
	}
	many := func(ids []string, media, name string) []imagegen.Image {
		list, used := s.loadMedia(ids, media)
		if len(used) > 0 {
			logReq[name] = used
			parents = append(parents, used...)
		}
		return list
	}
	if greq.FirstFrame = one(sh.Keyframe(), "first_frame"); greq.FirstFrame == nil {
		emit(Event{Type: "status", Text: "This shot has no keyframe: generating without a first frame."})
	}
	greq.LastFrame = one(sh.EndFrame(), "last_frame")
	refs, _ := ShotRefs(p, sh)
	greq.References = many(refs, "image", "references")
	greq.RefVideos = many(sh.VideoRefIDs, "video", "reference_videos")
	greq.RefAudios = many(sh.AudioRefIDs, "audio", "reference_audios")

	gen, err := s.NewImage(prov)
	if err != nil {
		return res, err
	}
	greq.Schema = s.videoSchema(ctx, prov, model, emit)
	if ex, ok := gen.(imagegen.Explainer); ok {
		for _, note := range ex.Explain(greq) {
			emit(Event{Type: "status", Text: note})
		}
	}
	inline := 0
	for _, list := range [][]imagegen.Image{greq.References, greq.RefVideos, greq.RefAudios} {
		for _, f := range list {
			inline += len(f.Data)
		}
	}
	if inline > maxInlineWarn {
		emit(Event{Type: "status", Text: fmt.Sprintf("The reference files total %d MB and are sent inline; a request this large can be slow or be rejected.", inline>>20)})
	}

	entry := &reqlog.Entry{
		Kind: reqlog.KindVideo, ProjectID: p.ID, Stage: project.StageClips, Target: sh.ID, Label: fmt.Sprintf("Shot %d", sh.Number),
		Provider: provName, Model: model, Request: logReq, Response: map[string]any{},
	}
	if d, ok := gen.(imagegen.PayloadDescriber); ok {
		entry.Request["payload"] = d.DescribePayload(greq)
	}
	emit(Event{Type: "status", Text: fmt.Sprintf("Generating video with %s / %s…", provName, model)})
	start := time.Now()
	out, err := s.runJob(ctx, p.ID, project.RunKey(project.StageClips, sh.ID), provName, gen, greq, emit)
	if err != nil {
		s.writeLog(entry, start, err, emit)
		return res, err
	}
	defer func() { s.logOutputs(entry, start, out, res, err, emit) }()
	if len(out.Videos) == 0 {
		return res, errors.New("the model returned no video")
	}
	return res, s.saveOutputs(out.Videos, assets.Asset{
		Kind: assets.KindVideo, Name: fmt.Sprintf("Shot %d", sh.Number),
		ProjectID: p.ID, Stage: project.StageClips, ShotID: sh.ID, Prompt: prompt, Model: model, Parents: parents,
		Params: map[string]any{"provider": provName, "aspect_ratio": p.AspectRatio, "duration": sh.Duration},
		Tags:   []string{"clip"},
	}, out, res, emit)
}

func (s *Service) runClips(ctx context.Context, r RunRequest, emit Emit) (string, string, error) {
	p, err := s.Projects.Get(r.ProjectID)
	if err != nil {
		return "", "", err
	}
	ids, labels, err := s.shotTargets(p, r, func(sh *project.Shot) bool { return len(sh.ClipIDs) > 0 })
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
		res, err := s.generateClip(ctx, r, p, sh, emit)
		if err != nil {
			return res, err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if sh := p.Shot(id); sh != nil {
				sh.ClipIDs = append(sh.ClipIDs, assetIDs(res.Assets)...)
			}
			return nil
		})
		return res, err
	})
}
