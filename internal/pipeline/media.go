package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// Resumable provider jobs

// fingerprint identifies what a request asks for, so that a job is only
// resumed by a run that would have sent the same request.
func fingerprint(gen imagegen.Generator, req *imagegen.Request) string {
	var body any = []any{req.Prompt, req.AspectRatio, req.Duration}
	if d, ok := gen.(imagegen.PayloadDescriber); ok {
		body = d.DescribePayload(req) // inline files appear as type + size
	}
	b, _ := json.Marshal([]any{req.Kind, req.Model, body})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (s *Service) setJob(projectID, key string, job *project.Job) {
	_, _ = s.Projects.Update(projectID, func(p *project.Project) error {
		if job == nil {
			delete(p.Jobs, key)
			return nil
		}
		if p.Jobs == nil {
			p.Jobs = map[string]*project.Job{}
		}
		p.Jobs[key] = job
		return nil
	})
}

// runJob sends one media request and returns its result.
//
// With a queue-based provider the accepted job is saved in the project
// (Project.Jobs[key]) until its result has been collected. If a run is
// stopped or the server restarts meanwhile, the next run for the same
// item with an unchanged request collects that job's result instead of
// submitting, and paying for, a new one. A job whose request has since
// changed is cancelled and replaced.
func (s *Service) runJob(ctx context.Context, projectID, key, provName string, gen imagegen.Generator, req *imagegen.Request, emit Emit) (*imagegen.Result, error) {
	q, ok := gen.(imagegen.Queued)
	if !ok {
		return gen.Generate(ctx, req)
	}
	fp := fingerprint(gen, req)
	var job *imagegen.Job
	if p, err := s.Projects.Get(projectID); err == nil {
		if pj := p.Jobs[key]; pj != nil {
			old := &imagegen.Job{RequestID: pj.RequestID, StatusURL: pj.StatusURL, ResponseURL: pj.ResponseURL}
			if pj.Fingerprint == fp && pj.Provider == provName {
				emit(Event{Type: "status", Text: fmt.Sprintf("Collecting the result of request %s, submitted earlier, instead of generating again…", pj.RequestID)})
				job = old
			} else {
				emit(Event{Type: "status", Text: "The request changed since the unfinished one was submitted: cancelling that one."})
				_ = q.Cancel(ctx, old)
				s.setJob(projectID, key, nil)
			}
		}
	}
	if job == nil {
		var err error
		if job, err = q.Submit(ctx, req); err != nil {
			return nil, err
		}
		s.setJob(projectID, key, &project.Job{
			Provider: provName, Model: req.Model, RequestID: job.RequestID, StatusURL: job.StatusURL, ResponseURL: job.ResponseURL,
			Fingerprint: fp, SubmittedAt: time.Now().UTC(),
		})
	}
	res, err := q.Wait(ctx, job, func(state string) {
		emit(Event{Type: "status", Text: "Provider status: " + state})
	})
	if imagegen.Retryable(err) {
		// The job may still finish: keep it so it can be collected later.
		emit(Event{Type: "status", Text: "The request is still with the provider. Run this step again to collect its result."})
		return nil, err
	}
	s.setJob(projectID, key, nil)
	return res, err
}

// DiscardJob forgets an unfinished provider job and asks the provider to
// cancel it (best effort). It reports whether there was one.
func (s *Service) DiscardJob(ctx context.Context, projectID, key string) bool {
	p, err := s.Projects.Get(projectID)
	if err != nil || p.Jobs[key] == nil {
		return false
	}
	pj := p.Jobs[key]
	s.setJob(projectID, key, nil)
	if prov := s.Config.Get().Provider(pj.Provider); prov != nil {
		if gen, err := s.NewImage(prov); err == nil {
			if q, ok := gen.(imagegen.Queued); ok {
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				_ = q.Cancel(ctx, &imagegen.Job{RequestID: pj.RequestID, StatusURL: pj.StatusURL, ResponseURL: pj.ResponseURL})
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Stage 7: video clips

// maxInlineWarn is the total size of inline inputs above which the user is
// warned that the request is heavy.
const maxInlineWarn = 24 << 20

// loadMedia reads assets of one media type as request inputs, skipping
// missing ones. It returns the inputs and the IDs actually used.
func (s *Service) loadMedia(ids []string, media string) (out []imagegen.Image, used []string) {
	for _, id := range ids {
		a, data, err := s.Assets.Read(id)
		if err != nil || a.Media() != media {
			continue
		}
		out = append(out, imagegen.Image{MIME: a.MIME, Data: data})
		used = append(used, id)
	}
	return out, used
}

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
func (s *Service) generateClip(ctx context.Context, r RunRequest, p *project.Project, sh *project.Shot, emit Emit) (res *imageResult, err error) {
	cfg := s.Config.Get()
	vc := cfg.Video
	provName := vc.Provider
	if r.Provider != "" {
		provName = r.Provider
	}
	res = &imageResult{Provider: provName}
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
	defer func() {
		entry.Response["assets"] = assetIDs(res.Assets)
		if out.RequestID != "" {
			entry.Response["request_id"] = out.RequestID
		}
		if len(out.Raw) > 0 {
			entry.Response["raw"] = out.Raw
		}
		s.writeLog(entry, start, err, emit)
	}()
	if len(out.Videos) == 0 {
		return res, errors.New("the model returned no video")
	}
	for _, v := range out.Videos {
		m := assets.Asset{
			Kind: assets.KindVideo, Name: fmt.Sprintf("Shot %d", sh.Number), MIME: v.MIME, Width: v.Width, Height: v.Height,
			ProjectID: p.ID, Stage: project.StageClips, ShotID: sh.ID, Prompt: prompt, Model: model, Parents: parents,
			Params: map[string]any{"provider": provName, "aspect_ratio": p.AspectRatio, "duration": sh.Duration},
			Tags:   []string{"clip"},
		}
		if out.RequestID != "" {
			m.Params["request_id"] = out.RequestID
		}
		a, err := s.Assets.Put(m, v.Data)
		if err != nil {
			return res, err
		}
		emit(Event{Type: "status", Text: "Saved " + a.ID})
		res.Assets = append(res.Assets, a)
	}
	return res, nil
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
		res, err := s.generateClip(ctx, r, p, sh, emit)
		last = res
		if err != nil {
			return err
		}
		_, err = s.Projects.Update(r.ProjectID, func(p *project.Project) error {
			if sh := p.Shot(id); sh != nil {
				sh.ClipIDs = append(sh.ClipIDs, assetIDs(res.Assets)...)
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
